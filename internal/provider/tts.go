// Package provider holds the model-role interfaces (TTS here; LLM and STT
// later) and their implementations.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"sevi/internal/httpx"
	"sevi/internal/logx"
)

// Speech is one synthesized chunk. PCM is 24kHz mono s16le. GenerationID
// identifies the call at the provider so its cost can be looked up.
// When a chunk had to be split or moved to another model, the audio came from
// several calls: Parts lists them in order (PCM is all of them joined), so each
// call gets its own cost row. Parts is empty for a chunk done in one call.
type Speech struct {
	PCM          []byte
	GenerationID string
	Parts        []SpeechPart
}

// SpeechPart is one provider call inside a Speech.
type SpeechPart struct {
	Model        string
	GenerationID string
	Chars        int
	PCM          []byte
}

// ErrEmptyAudio means the provider answered but sent no audio.
var ErrEmptyAudio = errors.New("empty audio in response")

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

// Synth speaks text, retrying a failed call up to Retries more times.
func (t *OpenRouterTTS) Synth(ctx context.Context, text string) (Speech, error) {
	req := map[string]any{
		"model":           t.Model,
		"input":           text,
		"response_format": "pcm", // Gemini TTS supports pcm only
	}
	if t.Voice != "" { // some models take no voice; sending one makes the request fail
		req["voice"] = t.Voice
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Speech{}, err
	}
	url := strings.TrimRight(t.BaseURL, "/") + "/audio/speech"

	attempts := t.Retries + 1
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			wait := time.Duration(i) * 2 * time.Second
			logx.With(ctx, t.Log).Warnw("tts retry", "attempt", i+1, "of", attempts, "wait", wait, "model", t.Model, "chars", len([]rune(text)), "err", lastErr)
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
			err = fmt.Errorf("%s: %w", req.URL.Host, ErrEmptyAudio)
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
	logx.With(ctx, t.Log).Warnw("tts call failed", "model", t.Model, "attempts", attempts, "chars", len([]rune(text)), "err", lastErr)
	return Speech{}, fmt.Errorf("tts failed after %d attempts: %w", attempts, lastErr)
}
