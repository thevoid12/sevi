package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"sevi/internal/httpx"
)

// Usage is what one provider call consumed and cost.
type Usage struct {
	CostUSD          float64
	PromptTokens     int
	CompletionTokens int
}

// OpenRouterUsage looks up a generation's usage and cost. OpenRouter's record
// lags the request by seconds to a minute or more, so this polls (3s, then
// every 5s, about two minutes in all) until it appears. A 404 means "not yet".
func OpenRouterUsage(ctx context.Context, baseURL, key, generationID string) (Usage, error) {
	if generationID == "" {
		return Usage{}, fmt.Errorf("no generation id returned by provider")
	}
	url := strings.TrimRight(baseURL, "/") + "/generation?id=" + generationID
	var lastErr error
	for i := 0; i < 24; i++ {
		wait := 5 * time.Second
		if i == 0 {
			wait = 3 * time.Second
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return Usage{}, ctx.Err()
		}
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return Usage{}, err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		b, _, err := httpx.Do(ctx, req)
		if err != nil {
			lastErr = err
			continue
		}
		var r struct {
			Data struct {
				Usage            float64 `json:"usage"`
				PromptTokens     int     `json:"native_tokens_prompt"`
				CompletionTokens int     `json:"native_tokens_completion"`
			} `json:"data"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			lastErr = err
			continue
		}
		return Usage{CostUSD: r.Data.Usage, PromptTokens: r.Data.PromptTokens, CompletionTokens: r.Data.CompletionTokens}, nil
	}
	return Usage{}, fmt.Errorf("usage lookup for %s failed: %w", generationID, lastErr)
}
