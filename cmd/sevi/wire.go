package main

import (
	"context"
	"sevi/internal/config"
	"time"

	"sevi/internal/core"
	"sevi/internal/fetch"
	"sevi/internal/model"
	"sevi/internal/pricing"
	"sevi/internal/provider"
)

// app is the running program: the shared Service plus where its config came from.
// Everything that used to take cfg, secrets, store, objects and log as separate
// parameters is a method on it.
type app struct {
	*model.Service
	cfgPath string
}

// newCore wires Core to the real providers described in config.
func (a *app) newCore() (*core.Core, error) {
	cfg, log := a.Cfg, a.Log
	ttsName, llmName := cfg.GetString("models.tts.provider"), cfg.GetString("models.llm.provider")
	ttsKey, err := a.ProviderKey(ttsName)
	if err != nil {
		return nil, err
	}
	llmKey, err := a.ProviderKey(llmName)
	if err != nil {
		return nil, err
	}
	ttsURL, llmURL := a.BaseURL(ttsName), a.BaseURL(llmName)

	jinaURL, jinaKey := "https://r.jina.ai", ""
	if cfg.IsSet("providers.jina.base_url") {
		jinaURL, jinaKey = a.BaseURL("jina"), a.OptionalSecret("JINA_API_KEY")
	}

	d := core.Deps{
		Service: a.Service,
		TTS:     a.newTTS(ttsURL, ttsKey),
		LLM:     &provider.Retrying{LLM: provider.NewOpenAICompatLLM(llmURL, llmKey, cfg.GetString("models.llm.model"), 0), Retries: cfg.GetInt("app.retries"), Log: log},
		Markdown: func(ctx context.Context, pageURL string) (string, error) {
			return fetch.Markdown(ctx, jinaURL, jinaKey, pageURL)
		},
	}
	stt := cfg.IsSet("models.stt.provider")
	if stt {
		sttName := cfg.GetString("models.stt.provider")
		sttKey, err := a.ProviderKey(sttName)
		if err != nil {
			return nil, err
		}
		d.STT = &provider.Retrying{LLM: provider.NewOpenAICompatLLM(a.BaseURL(sttName), sttKey, cfg.GetString("models.stt.model"), 0), Retries: cfg.GetInt("app.retries"), Log: log}
	}

	// Prices: one source and one charge-reporter per provider in use. Both are
	// OpenRouter's today (see "Generic providers" in implementation.md).
	ttl := time.Duration(cfg.GetInt("cost.price_ttl_days")) * 24 * time.Hour
	if ttl == 0 {
		ttl = 7 * 24 * time.Hour
	}
	book := &pricing.Book{
		Store: a.Store, TTL: ttl, Log: log,
		Sources: map[string]pricing.Source{}, Reporter: map[string]pricing.Reporter{},
	}
	used := []string{ttsName, llmName}
	if stt {
		used = append(used, cfg.GetString("models.stt.provider"))
	}
	for _, name := range used {
		url := a.BaseURL(name)
		key, _ := a.ProviderKey(name)
		book.Sources[name] = func(ctx context.Context) (map[string]provider.Price, error) {
			return provider.OpenRouterPrices(ctx, url)
		}
		book.Reporter[name] = func(ctx context.Context, generationID string) (provider.Usage, error) {
			return provider.OpenRouterUsage(ctx, url, key, generationID)
		}
	}
	d.Prices = book
	return core.New(d), nil
}

// newTTS builds the speech model: the main one with its retries, and the
// optional models.tts.fallbacks ([{"model": "...", "voice": "..."}], same provider; no voice means none is sent)
// that take over for a piece the main model cannot produce even after the
// chunk was split (see provider.Resilient).
func (a *app) newTTS(baseURL, key string) provider.TTS {
	cfg, log := a.Cfg, a.Log
	primary := &provider.OpenRouterTTS{
		BaseURL: baseURL, Key: key, Model: cfg.GetString("models.tts.model"), Voice: cfg.GetString("models.tts.voice"),
		Retries: ttsRetries(cfg), Log: log,
	}
	r := &provider.Resilient{Primary: primary, Log: log}
	var fallbacks []struct{ Model, Voice string }
	if err := cfg.UnmarshalKey("models.tts.fallbacks", &fallbacks); err != nil {
		log.Warnw("models.tts.fallbacks is not a list of {model, voice}; ignoring it", "err", err)
	}
	for _, fb := range fallbacks {
		if fb.Model == "" {
			continue
		}
		// An empty voice is sent as no voice, for models that take none.
		r.Fallbacks = append(r.Fallbacks, &provider.OpenRouterTTS{BaseURL: baseURL, Key: key, Model: fb.Model, Voice: fb.Voice, Log: log})
	}
	return r
}

// ttsRetries is how many extra attempts the main speech model gets for a whole
// chunk: models.tts.retries, or app.retries when that is not set. It is its own
// setting because the same failure repeats, so speech wants fewer retries than
// the LLM calls that share app.retries.
func ttsRetries(cfg *config.Config) int {
	if cfg.IsSet("models.tts.retries") {
		return cfg.GetInt("models.tts.retries")
	}
	return cfg.GetInt("app.retries")
}
