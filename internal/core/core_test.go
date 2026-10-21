package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"sevi/internal/blob"
	"sevi/internal/config"
	"sevi/internal/event"
	"sevi/internal/httpx"
	"sevi/internal/pricing"
	"sevi/internal/provider"
	"sevi/internal/reqid"
	"sevi/internal/store"
)

// fakeTTS returns one second of silence per call and can fail on a given call.
type fakeTTS struct {
	calls  atomic.Int32
	failAt int32 // 1-based call number that fails, 0 = never
}

func (f *fakeTTS) Synth(_ context.Context, _ string) (provider.Speech, error) {
	n := f.calls.Add(1)
	if f.failAt != 0 && n == f.failAt {
		return provider.Speech{}, &httpx.StatusError{Host: "tts.example", Code: 500, Status: "500", Body: "SECRET-PROVIDER-DETAIL"}
	}
	return provider.Speech{PCM: make([]byte, 48000), GenerationID: fmt.Sprintf("gen-%d", n)}, nil
}

// fakeLLM returns scripted replies and remembers every request it got.
type fakeLLM struct {
	mu      sync.Mutex
	replies []string // JSON texts returned in order; the last repeats
	reqs    []provider.ChatRequest
	err     error
	usage   provider.ChatUsage
	rateLim int // the first N calls fail with a 429
}

func (f *fakeLLM) Chat(_ context.Context, req provider.ChatRequest) (provider.ChatResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if len(f.reqs) <= f.rateLim {
		return provider.ChatResult{}, &httpx.StatusError{Host: "openrouter.ai", Code: 429, Status: "429", Body: "temporarily rate-limited upstream"}
	}
	if f.err != nil {
		return provider.ChatResult{}, f.err
	}
	i := len(f.reqs) - 1
	if i >= len(f.replies) {
		i = len(f.replies) - 1
	}
	return provider.ChatResult{Text: f.replies[i], GenerationID: fmt.Sprintf("llm-gen-%d", len(f.reqs)), Usage: f.usage}, nil
}

func (f *fakeLLM) lastRequest() provider.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

func (f *fakeLLM) requestText(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, p := range f.reqs[i].Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func replyJSON(transcript, format, answer string) string {
	b, _ := json.Marshal(map[string]string{"transcript": transcript, "format": format, "answer": answer})
	return string(b)
}

type env struct {
	core *Core
	tts  *fakeTTS
	llm  *fakeLLM
	st   *store.Store
	bl   blob.Disk
}

// Prices used by the tests (USD per token).
var (
	testTTSPrice = provider.Price{Prompt: 1e-6, Completion: 1e-4}
	testLLMPrice = provider.Price{Prompt: 1e-7, Completion: 4e-7, Audio: 3e-7}
)

// newEnv builds a Core on a temp database with fake network. Each blog page is
// ~5700 chars so it splits into 2 chunks of 3000.
func newEnv(t *testing.T, confirmOverUSD, estPer1K float64) *env {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	log := zap.NewNop().Sugar()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), dir+"/t.db", log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bl := blob.Disk{Root: dir + "/blobs"}
	cfg := &config.Config{
		Models: config.Models{
			TTS: config.TTS{Provider: "p", Model: "m", Voice: "v", ChunkChars: 3000},
			LLM: config.LLM{Provider: "p", Model: "llm-m", SystemPrompt: "Be brief."},
		},
		Reply: config.Reply{Voice: true},
		Cost:  config.Cost{ConfirmOverUSD: confirmOverUSD, EstUSDPer1KChar: estPer1K},
	}
	tts := &fakeTTS{}
	llm := &fakeLLM{replies: []string{replyJSON("", "voice", "An answer.")}, usage: provider.ChatUsage{PromptTokens: 1000, AudioTokens: 0, CompletionTokens: 50}}
	prices := &pricing.Book{
		Store: st, TTL: time.Hour, Log: log,
		Sources: map[string]pricing.Source{"p": func(context.Context) (map[string]provider.Price, error) {
			return map[string]provider.Price{"m": testTTSPrice, "llm-m": testLLMPrice}, nil
		}},
	}
	page := "Title: A Test Post\n\nMarkdown Content:\n" + strings.Repeat("This is a sentence that will be read aloud. ", 130) // ~5700 chars
	c := New(Deps{
		Cfg: cfg, Store: st, Blob: bl, Log: log, TTS: tts, LLM: llm, Prices: prices,
		Markdown: func(context.Context, string) (string, error) { return page, nil },
	})
	t.Cleanup(c.Close)
	return &env{core: c, tts: tts, llm: llm, st: st, bl: bl}
}

func (e *env) send(t *testing.T, m Message) JobView {
	t.Helper()
	ctx := reqid.With(context.Background(), "test-req")
	j, err := e.core.Handle(ctx, m)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return waitJob(t, j, false)
}

// waitJob waits until the job stops running. (cost is unused now that costs are
// known when the job finishes; kept so call sites read the same.)
func waitJob(t *testing.T, j *Job, _ bool) JobView {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		ch := j.Changed()
		v := j.View(0)
		if v.Status != Running {
			return v
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("job did not finish: %+v", v)
		}
	}
}

func link(u string) Message { return Message{Parts: []Part{{Type: "url", URL: u}}} }

func TestShortBlogIsPricedWhenItFinishes(t *testing.T) {
	e := newEnv(t, 100, 0.014) // threshold far above estimate
	ctx := reqid.With(context.Background(), "r")
	j, err := e.core.Handle(ctx, link("https://blog.example/a"))
	if err != nil {
		t.Fatal(err)
	}
	v := waitJob(t, j, false)
	if v.Status != Done || len(v.Parts) != 1 || v.Parts[0].Type != "audio" {
		t.Fatalf("want done with one audio part, got %+v", v)
	}
	if got := e.tts.calls.Load(); got != 2 {
		t.Errorf("tts calls = %d, want 2 chunks", got)
	}
	if p := v.Parts[0]; p.Title != "A Test Post" {
		t.Errorf("audio part should carry the article title: %+v", p)
	}
	sc, err := e.st.SessionCost(ctx, v.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Each fake chunk is 1 s of speech = 25 output tokens; input tokens from its length.
	var want float64
	rows, _ := e.st.ListLedger(ctx, v.SessionID)
	for _, r := range rows {
		in, out := pricing.TTSTokens(int(r.Chars), 1, 0)
		want += pricing.TTSCost(testTTSPrice, in, out)
		if int(r.InputUnits.Int64) != in || int(r.OutputUnits.Int64) != out {
			t.Errorf("ledger tokens %d/%d, want %d/%d", r.InputUnits.Int64, r.OutputUnits.Int64, in, out)
		}
	}
	if sc.Calls != 2 || sc.UnknownCalls != 0 || math.Abs(sc.TotalUsd-want) > 1e-9 || want == 0 {
		t.Errorf("session cost = %+v, want 2 priced calls totalling %v", sc, want)
	}
	if v.Parts[0].CostUSD == nil || math.Abs(*v.Parts[0].CostUSD-want) > 1e-9 {
		t.Errorf("audio part should carry the session cost %v: %+v", want, v.Parts[0].CostUSD)
	}
	var sawProgress, sawCost bool
	for _, ev := range v.Events {
		sawProgress = sawProgress || (ev.Kind == event.Progress && ev.Total == 2)
		sawCost = sawCost || ev.Kind == event.Cost
	}
	if !sawProgress || !sawCost {
		t.Errorf("missing progress/cost events: %+v", v.Events)
	}
}

func TestLongBlogAsksThenRuns(t *testing.T) {
	e := newEnv(t, 0.01, 0.014) // ~6000 chars * 0.014/1k = $0.084 > 0.01
	v := e.send(t, link("https://blog.example/long"))
	if v.Status != NeedsConfirmation {
		t.Fatalf("status = %s, want needs_confirmation", v.Status)
	}
	if e.tts.calls.Load() != 0 {
		t.Fatal("tts must not run before confirmation")
	}
	last := v.Events[len(v.Events)-1]
	if last.Kind != event.Confirm || !strings.Contains(last.Message, "$") {
		t.Errorf("confirm event should carry an estimate, got %+v", last)
	}

	v2 := e.send(t, Message{SessionID: v.SessionID, Parts: []Part{{Type: "confirm", Decision: "yes"}}})
	if v2.Status != Done || e.tts.calls.Load() != 2 {
		t.Fatalf("after yes: status %s, tts calls %d", v2.Status, e.tts.calls.Load())
	}
	// Nothing is waiting any more.
	_, err := e.core.Handle(context.Background(), Message{SessionID: v.SessionID, Parts: []Part{{Type: "confirm", Decision: "yes"}}})
	if err == nil {
		t.Error("second confirm should be rejected")
	}
}

func TestDecliningSpendsNothing(t *testing.T) {
	e := newEnv(t, 0.01, 0.014)
	v := e.send(t, link("https://blog.example/long"))
	v2 := e.send(t, Message{SessionID: v.SessionID, Parts: []Part{{Type: "confirm", Decision: "no"}}})
	if v2.Status != Done || e.tts.calls.Load() != 0 {
		t.Fatalf("after no: status %s, tts calls %d", v2.Status, e.tts.calls.Load())
	}
}

func TestResharedLinkIsFree(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	e.send(t, link("https://blog.example/a"))
	before := e.tts.calls.Load()
	v := e.send(t, link("https://blog.example/a"))
	if v.Status != Done || e.tts.calls.Load() != before {
		t.Fatalf("second share: status %s, new tts calls %d", v.Status, e.tts.calls.Load()-before)
	}
	if p := v.Parts[0]; p.Title != "A Test Post" {
		t.Errorf("cached audio should still carry the title: %+v", p)
	}
}

func TestProviderFailureShowsSafeMessageAndKeepsPartial(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	e.tts.failAt = 2
	ctx := reqid.With(context.Background(), "ref-123")
	j, err := e.core.Handle(ctx, link("https://blog.example/a"))
	if err != nil {
		t.Fatal(err)
	}
	v := waitJob(t, j, false)
	if v.Status != Failed {
		t.Fatalf("status = %s, want failed", v.Status)
	}
	last := v.Events[len(v.Events)-1]
	if last.Kind != event.Error || last.Ref != "ref-123" {
		t.Errorf("error event should carry the request id as ref: %+v", last)
	}
	if strings.Contains(last.Message, "SECRET-PROVIDER-DETAIL") || strings.Contains(last.Message, "tts.example") {
		t.Errorf("raw provider text leaked to the user: %q", last.Message)
	}
	if !strings.Contains(last.Message, "part 2 of 2") {
		t.Errorf("message should name the failing part: %q", last.Message)
	}
	if _, err := e.bl.Get(ctx, "partial/"+v.SessionID+".mp3"); err != nil {
		t.Errorf("partial audio not kept: %v", err)
	}
}

func TestBadInput(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	for name, m := range map[string]Message{
		"not a url":      link("ftp://x"),
		"no parts":       {},
		"confirm no sid": {Parts: []Part{{Type: "confirm", Decision: "yes"}}},
	} {
		_, err := e.core.Handle(context.Background(), m)
		var ue *event.UserError
		if !errors.As(err, &ue) || ue.Message == "" {
			t.Errorf("%s: want a UserError with a message, got %v", name, err)
		}
	}
}
