package provider

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"sevi/internal/httpx"
)

// Retrying wraps an LLM and tries again when a call fails in a way that may
// pass: a rate limit (429), a server error (5xx) or a network error. Shared
// upstream pools (e.g. Google AI Studio behind OpenRouter) rate-limit in bursts
// that last several seconds, so the waits grow: 3s, 6s, 12s, 24s...
// A rejected key, a bad request and the like are not retried.
type Retrying struct {
	LLM     LLM
	Retries int                           // extra attempts after the first
	Backoff func(retry int) time.Duration // wait before retry number n (1-based); default 3s doubling
	Log     *zap.SugaredLogger
}

func (r *Retrying) Chat(ctx context.Context, req ChatRequest) (ChatResult, error) {
	var err error
	for attempt := 0; ; attempt++ {
		var res ChatResult
		if res, err = r.LLM.Chat(ctx, req); err == nil {
			return res, nil
		}
		if ctx.Err() != nil || attempt >= r.Retries || !httpx.Retryable(err) {
			return ChatResult{}, err
		}
		wait := r.backoff(attempt + 1)
		r.Log.Warnw("model call failed, will retry", "attempt", attempt+1, "of", r.Retries+1, "wait", wait, "err", err)
		if req.OnRetry != nil {
			req.OnRetry(attempt+1, wait, err)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ChatResult{}, fmt.Errorf("%w (while waiting to retry: %v)", ctx.Err(), err)
		}
	}
}

func (r *Retrying) backoff(retry int) time.Duration {
	if r.Backoff != nil {
		return r.Backoff(retry)
	}
	return 3 * time.Second << (retry - 1)
}
