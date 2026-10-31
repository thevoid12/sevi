package main

import (
	"context"
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
		TTS: &provider.OpenRouterTTS{
			BaseURL: ttsURL, Key: ttsKey, Model: cfg.GetString("models.tts.model"), Voice: cfg.GetString("models.tts.voice"),
			Retries: cfg.GetInt("app.retries"), Log: log,
		},
		LLM: &provider.Retrying{LLM: provider.NewOpenAICompatLLM(llmURL, llmKey, cfg.GetString("models.llm.model"), 0), Retries: cfg.GetInt("app.retries"), Log: log},
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
