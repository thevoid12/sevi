package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"sevi/internal/httpx"
)

type scriptedLLM struct {
	errs  []error // returned in order; nil means success
	calls int
}

func (s *scriptedLLM) Chat(context.Context, ChatRequest) (ChatResult, error) {
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return ChatResult{}, s.errs[i]
	}
	return ChatResult{Text: "ok"}, nil
}

func rateLimited() error {
	return &httpx.StatusError{Host: "openrouter.ai", Code: 429, Status: "429", Body: "temporarily rate-limited upstream"}
}

func fast(llm LLM, retries int) *Retrying {
	return &Retrying{LLM: llm, Retries: retries, Log: zap.NewNop().Sugar(), Backoff: func(int) time.Duration { return time.Millisecond }}
}

func TestRateLimitIsRetriedUntilItPasses(t *testing.T) {
	llm := &scriptedLLM{errs: []error{rateLimited(), rateLimited()}} // third call succeeds
	var notified []int
	res, err := fast(llm, 4).Chat(context.Background(), ChatRequest{OnRetry: func(n int, _ time.Duration, _ error) { notified = append(notified, n) }})
	if err != nil || res.Text != "ok" {
		t.Fatalf("want success after two rate limits, got %v %v", res, err)
	}
	if llm.calls != 3 || len(notified) != 2 || notified[0] != 1 || notified[1] != 2 {
		t.Errorf("calls=%d notified=%v, want 3 calls and notices for retries 1 and 2", llm.calls, notified)
	}
}

func TestGivesUpAfterTheConfiguredRetriesWithTheLastError(t *testing.T) {
	llm := &scriptedLLM{errs: []error{rateLimited(), rateLimited(), rateLimited(), rateLimited(), rateLimited()}}
	_, err := fast(llm, 2).Chat(context.Background(), ChatRequest{})
	var se *httpx.StatusError
	if !errors.As(err, &se) || se.Code != 429 {
		t.Fatalf("want the 429 back so callers can explain it, got %v", err)
	}
	if llm.calls != 3 {
		t.Errorf("calls = %d, want 1 + 2 retries", llm.calls)
	}
}

func TestRejectedKeyIsNotRetried(t *testing.T) {
	llm := &scriptedLLM{errs: []error{&httpx.StatusError{Host: "x", Code: 401, Status: "401"}, nil}}
	if _, err := fast(llm, 4).Chat(context.Background(), ChatRequest{}); err == nil {
		t.Fatal("a 401 must fail")
	}
	if llm.calls != 1 {
		t.Errorf("a rejected key was retried %d times", llm.calls-1)
	}
}

func TestServerErrorsAndNetworkErrorsAreRetried(t *testing.T) {
	for name, e := range map[string]error{
		"503":     &httpx.StatusError{Host: "x", Code: 503, Status: "503"},
		"network": errors.New("connection reset by peer"),
	} {
		llm := &scriptedLLM{errs: []error{e}}
		if _, err := fast(llm, 2).Chat(context.Background(), ChatRequest{}); err != nil || llm.calls != 2 {
			t.Errorf("%s: err=%v calls=%d, want a retry that succeeds", name, err, llm.calls)
		}
	}
}

func TestShuttingDownStopsTheWaiting(t *testing.T) {
	llm := &scriptedLLM{errs: []error{rateLimited(), rateLimited(), rateLimited()}}
	r := &Retrying{LLM: llm, Retries: 5, Log: zap.NewNop().Sugar(), Backoff: func(int) time.Duration { return time.Hour }}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	done := make(chan error, 1)
	go func() { _, err := r.Chat(ctx, ChatRequest{}); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling must interrupt the wait between retries")
	}
}

func TestDefaultBackoffGrows(t *testing.T) {
	r := &Retrying{}
	got := []time.Duration{r.backoff(1), r.backoff(2), r.backoff(3), r.backoff(4)}
	want := []time.Duration{3 * time.Second, 6 * time.Second, 12 * time.Second, 24 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got[i], want[i])
		}
	}
}
