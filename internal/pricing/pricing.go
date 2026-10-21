// Package pricing turns token counts into dollars without asking the provider
// after every call. Per-token model prices are kept in SQLite with the time
// they were fetched; a price older than the configured age is fetched again,
// and that refresh is also the moment we check our numbers against one real
// charge from the provider.
package pricing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"go.uber.org/zap"

	"sevi/internal/provider"
	"sevi/internal/store"
)

// Source fetches the current prices of every model a provider lists.
type Source func(ctx context.Context) (map[string]provider.Price, error)

// Reporter asks the provider what it actually charged for one past call. Used
// only when prices are refreshed, to confirm the stored prices still add up.
type Reporter func(ctx context.Context, generationID string) (provider.Usage, error)

type Book struct {
	Store    *store.Store
	Sources  map[string]Source   // by provider name
	Reporter map[string]Reporter // by provider name; optional
	TTL      time.Duration
	Log      *zap.SugaredLogger
	Now      func() time.Time // default time.Now; set in tests

	mu sync.Mutex // one refresh at a time
}

// Get returns the price of a model, fetching it when it has never been seen or
// is older than the TTL. If a refresh fails, a stale stored price is used (and
// logged) rather than failing the call; with no stored price at all it errors.
func (b *Book) Get(ctx context.Context, prov, model string) (provider.Price, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	row, err := b.Store.GetPrice(ctx, prov, model)
	have := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return provider.Price{}, err
	}
	if have && b.now().Sub(time.Unix(row.FetchedOn, 0)) < b.TTL {
		return toPrice(row), nil
	}

	if rerr := b.refresh(ctx, prov, model); rerr != nil {
		if have {
			b.Log.Warnw("could not refresh model price, using the stored one", "provider", prov, "model", model,
				"fetched_on", time.Unix(row.FetchedOn, 0).Format(time.RFC3339), "err", rerr)
			return toPrice(row), nil
		}
		return provider.Price{}, fmt.Errorf("no price for %s/%s: %w", prov, model, rerr)
	}
	row, err = b.Store.GetPrice(ctx, prov, model)
	if err != nil {
		return provider.Price{}, fmt.Errorf("no price for %s/%s: %w", prov, model, err)
	}
	return toPrice(row), nil
}

func (b *Book) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func toPrice(r store.ModelPrice) provider.Price {
	return provider.Price{Prompt: r.Prompt, Completion: r.Completion, Audio: r.Audio, CacheRead: r.CacheRead}
}

// refresh fetches the provider's list once and updates the requested model plus
// every model already stored for that provider, so one weekly call re-prices
// everything in use. Changes are logged.
func (b *Book) refresh(ctx context.Context, prov, want string) error {
	src, ok := b.Sources[prov]
	if !ok {
		return fmt.Errorf("no price source for provider %q", prov)
	}
	all, err := src(ctx)
	if err != nil {
		return err
	}
	stored, err := b.Store.PricedModels(ctx, prov)
	if err != nil {
		return err
	}
	models := map[string]bool{want: true}
	for _, m := range stored {
		models[m] = true
	}
	now := b.now().Unix()
	var updated []string
	for m := range models {
		np, ok := all[m]
		if !ok {
			b.Log.Warnw("model not in the provider's price list, keeping stored price", "provider", prov, "model", m)
			continue
		}
		if old, err := b.Store.GetPrice(ctx, prov, m); err == nil {
			if o := toPrice(old); o != np {
				b.Log.Warnw("model price changed", "provider", prov, "model", m, "old", o, "new", np)
			}
		}
		if err := b.Store.PutPrice(ctx, prov, m, store.ModelPrice{Prompt: np.Prompt, Completion: np.Completion, Audio: np.Audio, CacheRead: np.CacheRead, FetchedOn: now}); err != nil {
			return err
		}
		updated = append(updated, m)
	}
	b.Log.Infow("model prices refreshed", "provider", prov, "models", updated)

	if rep := b.Reporter[prov]; rep != nil {
		for _, m := range updated {
			// Off the request path: the provider's charge record can take a while to appear.
			go b.reconcile(context.WithoutCancel(ctx), prov, m, rep)
		}
	}
	return nil
}

// reconcile compares what we recorded for the latest call of a model with what
// the provider says it charged. A big gap means the stored price or the token
// estimate is wrong.
func (b *Book) reconcile(ctx context.Context, prov, model string, rep Reporter) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	row, err := b.Store.LatestCostedCall(ctx, prov, model)
	if err != nil {
		return // never used yet
	}
	got, err := rep(ctx, row.GenerationID.String)
	if err != nil {
		b.Log.Warnw("price check: could not get the provider's charge", "provider", prov, "model", model, "generation", row.GenerationID.String, "err", err)
		return
	}
	recorded := row.CostUsd.Float64
	diff := 0.0
	if got.CostUSD > 0 {
		diff = (recorded - got.CostUSD) / got.CostUSD
	}
	fields := []any{"provider", prov, "model", model, "role", row.Role,
		"recorded_usd", recorded, "provider_usd", got.CostUSD, "diff_pct", math.Round(diff*1000) / 10,
		"recorded_in_tokens", row.InputUnits.Int64, "provider_in_tokens", got.PromptTokens,
		"recorded_out_tokens", row.OutputUnits.Int64, "provider_out_tokens", got.CompletionTokens}
	// Flag only a real gap: more than 10% off and more than a tenth of a cent in
	// absolute terms (tiny calls drift by a few percent because cached prompt
	// tokens cannot be split exactly between text and audio).
	if math.Abs(diff) > 0.10 && math.Abs(recorded-got.CostUSD) > 0.001 {
		b.Log.Warnw("price check: recorded cost differs from the provider's charge by more than 10%", fields...)
		return
	}
	b.Log.Infow("price check: recorded cost matches the provider's charge", fields...)
}

// --- cost arithmetic --------------------------------------------------------

// charsPerToken approximates how many characters of English text make one
// input token. Input tokens are a small part of a TTS charge, so the
// approximation is cheap; the weekly price check would flag a large error.
const charsPerToken = 4.5

// DefaultAudioTokensPerSec is what Gemini TTS bills per second of speech.
const DefaultAudioTokensPerSec = 25.0

// LLMCost prices a chat call: audio prompt tokens at the audio rate, cached
// text tokens at the cache rate, the rest of the prompt at the prompt rate, and
// completion tokens. The usage report does not say whether cached tokens were
// text or audio; they are treated as text, so a turn that cached audio is
// priced slightly high (by well under a tenth of a cent in practice).
func LLMCost(p provider.Price, u provider.ChatUsage) float64 {
	audioRate, cacheRate := p.Audio, p.CacheRead
	if audioRate == 0 {
		audioRate = p.Prompt
	}
	if cacheRate == 0 {
		cacheRate = p.Prompt
	}
	text := max(u.PromptTokens-u.AudioTokens, 0)
	cached := min(u.CachedTokens, text)
	return float64(text-cached)*p.Prompt + float64(cached)*cacheRate +
		float64(u.AudioTokens)*audioRate + float64(u.CompletionTokens)*p.Completion
}

// TTSTokens estimates the billed tokens of one TTS call: input tokens from the
// text length, output tokens from the seconds of speech produced.
func TTSTokens(chars int, seconds, tokensPerSec float64) (in, out int) {
	if tokensPerSec <= 0 {
		tokensPerSec = DefaultAudioTokensPerSec
	}
	return int(math.Ceil(float64(chars) / charsPerToken)), int(math.Round(seconds * tokensPerSec))
}

func TTSCost(p provider.Price, in, out int) float64 {
	return float64(in)*p.Prompt + float64(out)*p.Completion
}

// EstimateTTS prices speaking `chars` characters before doing it, using the
// configured speaking rate.
func EstimateTTS(p provider.Price, chars, charsPerMinute int, tokensPerSec float64) float64 {
	seconds := float64(chars) / float64(charsPerMinute) * 60
	in, out := TTSTokens(chars, seconds, tokensPerSec)
	return TTSCost(p, in, out)
}
