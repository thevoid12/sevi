// Package provider holds the model-role interfaces (TTS here; LLM and STT
// later) and their implementations.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"sevi/internal/httpx"
)

// Speech is one synthesized chunk. PCM is 24kHz mono s16le. GenerationID
// identifies the call at the provider so its cost can be looked up.
type Speech struct {
	PCM          []byte
	GenerationID string
}

type TTS interface {
	Synth(ctx context.Context, text string) (Speech, error)
}

// OpenRouterTTS calls OpenRouter's /audio/speech endpoint (Gemini TTS).
type OpenRouterTTS struct {
	BaseURL string
	Key     string
	Model   string
	Voice   string
	Retries int // extra attempts after the first
	Log     *zap.SugaredLogger
}

func (t *OpenRouterTTS) Synth(ctx context.Context, text string) (Speech, error) {
	body, err := json.Marshal(map[string]any{
		"model":           t.Model,
		"input":           text,
		"voice":           t.Voice,
		"response_format": "pcm", // Gemini TTS supports pcm only
	})
	if err != nil {
		return Speech{}, err
	}
	url := strings.TrimRight(t.BaseURL, "/") + "/audio/speech"

	attempts := t.Retries + 1
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			wait := time.Duration(i) * 2 * time.Second
			t.Log.Warnw("tts retry", "attempt", i+1, "of", attempts, "wait", wait, "model", t.Model, "err", lastErr)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return Speech{}, ctx.Err()
			}
		}
		req, err := http.NewRequest("POST", url, bytes.NewReader(body))
		if err != nil {
			return Speech{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+t.Key)
		b, h, err := httpx.Do(ctx, req)
		if err == nil && len(b) == 0 {
			err = fmt.Errorf("%s: empty audio in response", req.URL.Host)
		}
		if err == nil {
			return Speech{PCM: b, GenerationID: h.Get("X-Generation-Id")}, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return Speech{}, ctx.Err()
		}
		if !httpx.Retryable(err) {
			return Speech{}, fmt.Errorf("tts request rejected: %w", err)
		}
	}
	return Speech{}, fmt.Errorf("tts failed after %d attempts: %w", attempts, lastErr)
}
