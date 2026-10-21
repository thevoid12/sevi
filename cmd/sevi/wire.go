package main

import (
	"context"
	"os"

	"go.uber.org/zap"

	"sevi/internal/blob"
	"sevi/internal/config"
	"sevi/internal/core"
	"sevi/internal/fetch"
	"sevi/internal/provider"
	"sevi/internal/store"
)

// newCore wires Core to the real providers described in config.
func newCore(cfg *config.Config, st *store.Store, bl blob.Store, log *zap.SugaredLogger) (*core.Core, error) {
	ttsCfg := cfg.Models.TTS
	key, err := cfg.ProviderKey(ttsCfg.Provider)
	if err != nil {
		return nil, err
	}
	prov := cfg.Provider(ttsCfg.Provider)

	jinaURL, jinaKey := "https://r.jina.ai", ""
	if j, ok := cfg.Providers["jina"]; ok {
		jinaURL, jinaKey = j.BaseURL, os.Getenv(j.KeyEnv)
	}

	return core.New(core.Deps{
		Cfg: cfg, Store: st, Blob: bl, Log: log,
		TTS: &provider.OpenRouterTTS{
			BaseURL: prov.BaseURL, Key: key, Model: ttsCfg.Model, Voice: ttsCfg.Voice,
			Retries: cfg.App.Retries, Log: log,
		},
		Markdown: func(ctx context.Context, pageURL string) (string, error) {
			return fetch.Markdown(ctx, jinaURL, jinaKey, pageURL)
		},
		Usage: func(ctx context.Context, generationID string) (provider.Usage, error) {
			return provider.OpenRouterUsage(ctx, prov.BaseURL, key, generationID)
		},
	}), nil
}
