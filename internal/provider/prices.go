package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"sevi/internal/httpx"
)

// Price is a model's price in USD per token. Audio is the price of audio
// input tokens and CacheRead of prompt tokens served from the provider's
// cache; 0 for either means the same as Prompt.
type Price struct {
	Prompt, Completion, Audio, CacheRead float64
}

// OpenRouterPrices fetches the price of every model OpenRouter lists, text
// and speech models alike. The endpoint is public (no key needed).
func OpenRouterPrices(ctx context.Context, baseURL string) (map[string]Price, error) {
	req, err := http.NewRequest("GET", strings.TrimRight(baseURL, "/")+"/models?output_modalities=all", nil)
	if err != nil {
		return nil, err
	}
	b, _, err := httpx.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("fetch model prices: %w", err)
	}
	var r struct {
		Data []struct {
			ID      string                     `json:"id"`
			Pricing map[string]json.RawMessage `json:"pricing"` // values are mostly strings, but not all (e.g. "overrides")
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse model prices: %w", err)
	}
	out := make(map[string]Price, len(r.Data))
	for _, m := range r.Data {
		p := Price{Prompt: parsePrice(m.Pricing["prompt"]), Completion: parsePrice(m.Pricing["completion"]), Audio: parsePrice(m.Pricing["audio"]), CacheRead: parsePrice(m.Pricing["input_cache_read"])}
		if p.Prompt < 0 || p.Completion < 0 { // router pseudo-models use negative placeholders
			continue
		}
		out[m.ID] = p
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("fetch model prices: empty list")
	}
	return out, nil
}

// parsePrice reads a price given as a JSON string ("0.0000001"); anything else is 0.
func parsePrice(raw json.RawMessage) float64 {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}
