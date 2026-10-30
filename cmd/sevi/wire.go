package main

import (
	"context"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	"sevi/internal/blob"
	"sevi/internal/config"
	"sevi/internal/core"
	"sevi/internal/fetch"
	"sevi/internal/pricing"
	"sevi/internal/provider"
	"sevi/internal/secrets"
	"sevi/internal/store"
)

func baseURL(cfg *config.Config, provider string) string {
	return cfg.GetString("providers." + provider + ".base_url")
}

// providerKey is the named provider's key; secrets.Require errors if it is not set.
func providerKey(cfg *config.Config, sec *viper.Viper, name string) (string, error) {
	return secrets.Require(sec, "providers."+name+".key_env", cfg.GetString("providers."+name+".key_env"))
}

func optional(sec *viper.Viper, name string) string {
	v, _ := secrets.Get(sec, name)
	return v
}

// newCore wires Core to the real providers described in config.
func newCore(cfg *config.Config, sec *viper.Viper, st *store.Store, bl blob.Store, log *zap.SugaredLogger) (*core.Core, error) {
	ttsName, llmName := cfg.GetString("models.tts.provider"), cfg.GetString("models.llm.provider")
	ttsKey, err := providerKey(cfg, sec, ttsName)
	if err != nil {
		return nil, err
	}
	llmKey, err := providerKey(cfg, sec, llmName)
	if err != nil {
		return nil, err
	}
	ttsURL, llmURL := baseURL(cfg, ttsName), baseURL(cfg, llmName)

	jinaURL, jinaKey := "https://r.jina.ai", ""
	if cfg.IsSet("providers.jina.base_url") {
		jinaURL, jinaKey = baseURL(cfg, "jina"), optional(sec, cfg.GetString("providers.jina.key_env"))
	}

	d := core.Deps{
		Cfg: cfg, Store: st, Blob: bl, Log: log,
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
		sttKey, err := providerKey(cfg, sec, sttName)
		if err != nil {
			return nil, err
		}
		d.STT = &provider.Retrying{LLM: provider.NewOpenAICompatLLM(baseURL(cfg, sttName), sttKey, cfg.GetString("models.stt.model"), 0), Retries: cfg.GetInt("app.retries"), Log: log}
	}

	// Prices: one source and one charge-reporter per provider in use. Both are
	// OpenRouter's today (see "Generic providers" in implementation.md).
	ttl := time.Duration(cfg.GetInt("cost.price_ttl_days")) * 24 * time.Hour
	if ttl == 0 {
		ttl = 7 * 24 * time.Hour
	}
	book := &pricing.Book{
		Store: st, TTL: ttl, Log: log,
		Sources: map[string]pricing.Source{}, Reporter: map[string]pricing.Reporter{},
	}
	used := []string{ttsName, llmName}
	if stt {
		used = append(used, cfg.GetString("models.stt.provider"))
	}
	for _, name := range used {
		url := baseURL(cfg, name)
		key, _ := providerKey(cfg, sec, name)
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
