package pricing

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"sevi/internal/provider"
	"sevi/internal/store"
)

func newBook(t *testing.T, src Source, now *time.Time) (*Book, *store.Store) {
	t.Helper()
	log := zap.NewNop().Sugar()
	st, err := store.Open(context.Background(), t.TempDir()+"/t.db", log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Book{
		Store: st, Sources: map[string]Source{"or": src}, TTL: 7 * 24 * time.Hour, Log: log,
		Now: func() time.Time { return *now },
	}, st
}

func TestPriceIsFetchedOnceThenReusedUntilItExpires(t *testing.T) {
	var calls atomic.Int32
	price := provider.Price{Prompt: 1e-7, Completion: 4e-7}
	src := func(context.Context) (map[string]provider.Price, error) {
		calls.Add(1)
		return map[string]provider.Price{"m": price}, nil
	}
	now := time.Now()
	b, _ := newBook(t, src, &now)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		got, err := b.Get(ctx, "or", "m")
		if err != nil || got != price {
			t.Fatalf("Get = %+v, %v", got, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("source called %d times for 5 lookups inside the TTL, want 1", calls.Load())
	}

	now = now.Add(6 * 24 * time.Hour) // still fresh
	b.Get(ctx, "or", "m")
	if calls.Load() != 1 {
		t.Fatalf("source called again after 6 days (TTL is 7)")
	}

	now = now.Add(2 * 24 * time.Hour)                      // 8 days: expired
	price = provider.Price{Prompt: 2e-7, Completion: 8e-7} // the provider changed it
	got, _ := b.Get(ctx, "or", "m")
	if calls.Load() != 2 || got != price {
		t.Fatalf("after expiry: calls=%d price=%+v, want a refresh to the new price", calls.Load(), got)
	}
}

func TestFailedRefreshFallsBackToStoredPrice(t *testing.T) {
	fail := false
	src := func(context.Context) (map[string]provider.Price, error) {
		if fail {
			return nil, errors.New("offline")
		}
		return map[string]provider.Price{"m": {Prompt: 1e-7, Completion: 4e-7}}, nil
	}
	now := time.Now()
	b, _ := newBook(t, src, &now)
	ctx := context.Background()
	b.Get(ctx, "or", "m")

	now = now.Add(30 * 24 * time.Hour)
	fail = true
	got, err := b.Get(ctx, "or", "m")
	if err != nil || got.Prompt != 1e-7 {
		t.Fatalf("stale fallback: %+v, %v", got, err)
	}

	if _, err := b.Get(ctx, "or", "never-seen"); err == nil {
		t.Fatal("a model with no stored price and a failing source must error")
	}
}

func TestOneRefreshReprices_EveryStoredModel(t *testing.T) {
	var calls atomic.Int32
	v := 1.0
	src := func(context.Context) (map[string]provider.Price, error) {
		calls.Add(1)
		return map[string]provider.Price{"a": {Prompt: v * 1e-7, Completion: 1e-6}, "b": {Prompt: v * 2e-7, Completion: 2e-6}}, nil
	}
	now := time.Now()
	b, _ := newBook(t, src, &now)
	ctx := context.Background()
	b.Get(ctx, "or", "a")
	b.Get(ctx, "or", "b") // a is already stored, b is new -> refresh updates both
	now = now.Add(8 * 24 * time.Hour)
	v = 3
	b.Get(ctx, "or", "a") // expired -> one fetch re-prices a and b
	before := calls.Load()
	got, _ := b.Get(ctx, "or", "b") // b was refreshed along with a: no new fetch
	if calls.Load() != before || math.Abs(got.Prompt-6e-7) > 1e-12 {
		t.Fatalf("b should have been re-priced by a's refresh without another fetch: calls %d->%d price %+v", before, calls.Load(), got)
	}
}

// The prices and token counts below are real values from this project's ledger
// (Gemini TTS via OpenRouter): the formula must reproduce the provider's charge.
func TestCostFormulaMatchesRealCharges(t *testing.T) {
	tts := provider.Price{Prompt: 5e-7, Completion: 9e-6}
	for _, c := range []struct {
		in, out int
		want    float64
	}{{71, 543, 0.0049225}, {641, 4460, 0.0404605}, {715, 4689, 0.0425585}} {
		if got := TTSCost(tts, c.in, c.out); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("TTSCost(%d,%d) = %v, want %v", c.in, c.out, got, c.want)
		}
	}
	// 25 audio tokens per second: the 534 s blog billed 13356 output tokens.
	if _, out := TTSTokens(8722, 534.24, 0); out != 13356 {
		t.Errorf("output tokens for 534.24 s = %d, want 13356", out)
	}
}

func TestLLMCostSplitsAudioTokens(t *testing.T) {
	p := provider.Price{Prompt: 1e-7, Completion: 4e-7, Audio: 3e-7}
	got := LLMCost(p, provider.ChatUsage{PromptTokens: 3000, AudioTokens: 1000, CompletionTokens: 200})
	want := 2000*1e-7 + 1000*3e-7 + 200*4e-7
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("got %v want %v", got, want)
	}
	// A model with no separate audio rate bills audio at the prompt rate.
	p.Audio = 0
	if got := LLMCost(p, provider.ChatUsage{PromptTokens: 1000, AudioTokens: 1000}); math.Abs(got-1000*1e-7) > 1e-12 {
		t.Errorf("audio fallback: %v", got)
	}
}

func TestEstimateTTSTracksTheRealBlog(t *testing.T) {
	// The 8,722-character blog really cost $0.1212; the estimate must be close.
	est := EstimateTTS(provider.Price{Prompt: 5e-7, Completion: 9e-6}, 8722, 950, 0)
	if est < 0.11 || est > 0.14 {
		t.Errorf("estimate %.4f is not near the real $0.1212", est)
	}
}

// The real voice-note turn that exposed provider-side prompt caching: 2962
// prompt tokens of which 335 audio and 1938 served from cache, 74 completion.
// OpenRouter charged $0.000179; without cache pricing the formula said $0.000393.
func TestLLMCostCountsCachedPromptTokensAtTheCacheRate(t *testing.T) {
	p := provider.Price{Prompt: 1e-7, Completion: 4e-7, Audio: 3e-7, CacheRead: 1e-8}
	u := provider.ChatUsage{PromptTokens: 2962, AudioTokens: 335, CachedTokens: 1938, CompletionTokens: 74}
	got := LLMCost(p, u)
	uncached := LLMCost(p, provider.ChatUsage{PromptTokens: 2962, AudioTokens: 335, CompletionTokens: 74})
	if got >= uncached {
		t.Fatalf("cached tokens must lower the cost: %v vs %v", got, uncached)
	}
	const real = 0.00017896
	if got < real || got > real*1.25 { // cached tokens are assumed text, so a little high is expected
		t.Errorf("cost %.6f should be within 25%% above the real charge %.6f", got, real)
	}
	// With no cache price the cached tokens are billed as normal prompt tokens.
	p.CacheRead = 0
	if LLMCost(p, u) != uncached {
		t.Errorf("without a cache price, caching must not change the cost")
	}
}

func TestPriceCheckFlagsRealGapsButNotNoise(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	now := time.Now()
	b, st := newBook(t, func(context.Context) (map[string]provider.Price, error) { return nil, nil }, &now)
	b.Log = zap.New(core).Sugar()
	ctx := context.Background()

	sess, _ := st.CreateSession(ctx, "chat", "")
	add := func(cost float64) {
		c := cost
		if _, err := st.AddLedger(ctx, store.LedgerEntry{SessionID: sess.ID, Role: "tts", Provider: "or", Model: "m", GenerationID: "g", InputUnits: 1, OutputUnits: 1, CostUSD: &c}); err != nil {
			t.Fatal(err)
		}
	}
	report := func(usd float64) Reporter {
		return func(context.Context, string) (provider.Usage, error) { return provider.Usage{CostUSD: usd}, nil }
	}
	level := func() zapcore.Level { e := logs.TakeAll(); return e[len(e)-1].Level }

	add(0.0100)
	b.reconcile(ctx, "or", "m", report(0.0100))
	if l := level(); l != zapcore.InfoLevel {
		t.Errorf("a matching charge should log at info, got %v", l)
	}
	b.reconcile(ctx, "or", "m", report(0.0150)) // 33% and half a cent off
	if l := level(); l != zapcore.WarnLevel {
		t.Errorf("a large gap should warn, got %v", l)
	}
	add(0.0003) // tiny call, 120% off but $0.0002 absolute: noise
	b.reconcile(ctx, "or", "m", report(0.0001))
	if l := level(); l != zapcore.InfoLevel {
		t.Errorf("a sub-tenth-of-a-cent gap is noise and must not warn, got %v", l)
	}
}
