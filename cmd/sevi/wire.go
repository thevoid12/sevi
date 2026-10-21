package main

import (
	"context"
	"os"
	"time"

	"go.uber.org/zap"

	"sevi/internal/blob"
	"sevi/internal/config"
	"sevi/internal/core"
	"sevi/internal/fetch"
	"sevi/internal/pricing"
	"sevi/internal/provider"
	"sevi/internal/store"
)

// newCore wires Core to the real providers described in config.
func newCore(cfg *config.Config, st *store.Store, bl blob.Store, log *zap.SugaredLogger) (*core.Core, error) {
	m := cfg.Models
	ttsKey, err := cfg.ProviderKey(m.TTS.Provider)
	if err != nil {
		return nil, err
	}
	llmKey, err := cfg.ProviderKey(m.LLM.Provider)
	if err != nil {
		return nil, err
	}
	ttsProv, llmProv := cfg.Provider(m.TTS.Provider), cfg.Provider(m.LLM.Provider)

	jinaURL, jinaKey := "https://r.jina.ai", ""
	if j, ok := cfg.Providers["jina"]; ok {
		jinaURL, jinaKey = j.BaseURL, os.Getenv(j.KeyEnv)
	}

	d := core.Deps{
		Cfg: cfg, Store: st, Blob: bl, Log: log,
		TTS: &provider.OpenRouterTTS{
			BaseURL: ttsProv.BaseURL, Key: ttsKey, Model: m.TTS.Model, Voice: m.TTS.Voice,
			Retries: cfg.App.Retries, Log: log,
		},
		LLM: &provider.Retrying{LLM: provider.NewOpenAICompatLLM(llmProv.BaseURL, llmKey, m.LLM.Model, 0), Retries: cfg.App.Retries, Log: log},
		Markdown: func(ctx context.Context, pageURL string) (string, error) {
			return fetch.Markdown(ctx, jinaURL, jinaKey, pageURL)
		},
	}
	if m.STT != nil {
		sttKey, err := cfg.ProviderKey(m.STT.Provider)
		if err != nil {
			return nil, err
		}
		d.STT = &provider.Retrying{LLM: provider.NewOpenAICompatLLM(cfg.Provider(m.STT.Provider).BaseURL, sttKey, m.STT.Model, 0), Retries: cfg.App.Retries, Log: log}
	}

	// Prices: one source and one charge-reporter per provider in use. Both are
	// OpenRouter's today (see "Generic providers" in implementation.md).
	ttl := time.Duration(cfg.Cost.PriceTTLDays) * 24 * time.Hour
	if ttl == 0 {
		ttl = 7 * 24 * time.Hour
	}
	book := &pricing.Book{
		Store: st, TTL: ttl, Log: log,
		Sources: map[string]pricing.Source{}, Reporter: map[string]pricing.Reporter{},
	}
	used := []string{m.TTS.Provider, m.LLM.Provider}
	if m.STT != nil {
		used = append(used, m.STT.Provider)
	}
	for _, name := range used {
		prov := cfg.Provider(name)
		key, _ := cfg.ProviderKey(name)
		book.Sources[name] = func(ctx context.Context) (map[string]provider.Price, error) {
			return provider.OpenRouterPrices(ctx, prov.BaseURL)
		}
		book.Reporter[name] = func(ctx context.Context, generationID string) (provider.Usage, error) {
			return provider.OpenRouterUsage(ctx, prov.BaseURL, key, generationID)
		}
	}
	d.Prices = book
	return core.New(d), nil
}
