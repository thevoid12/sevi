package core

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"sevi/internal/blob"
	"sevi/internal/config"
	"sevi/internal/event"
	"sevi/internal/httpx"
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

type env struct {
	core *Core
	tts  *fakeTTS
	st   *store.Store
	bl   blob.Disk
}

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
		Models: config.Models{TTS: config.TTS{Provider: "p", Model: "m", Voice: "v", ChunkChars: 3000}},
		Cost:   config.Cost{ConfirmOverUSD: confirmOverUSD, EstUSDPer1KChar: estPer1K},
	}
	tts := &fakeTTS{}
	page := "Title: A Test Post\n\nMarkdown Content:\n" + strings.Repeat("This is a sentence that will be read aloud. ", 130) // ~5700 chars
	c := New(Deps{
		Cfg: cfg, Store: st, Blob: bl, Log: log, TTS: tts,
		Markdown: func(context.Context, string) (string, error) { return page, nil },
		Usage: func(context.Context, string) (provider.Usage, error) {
			return provider.Usage{CostUSD: 0.05, PromptTokens: 10, CompletionTokens: 100}, nil
		},
	})
	t.Cleanup(c.Close)
	return &env{core: c, tts: tts, st: st, bl: bl}
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

// waitJob waits until the job stops running (and, if cost is true, until its cost resolves).
func waitJob(t *testing.T, j *Job, cost bool) JobView {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		ch := j.Changed()
		v := j.View(0)
		if v.Status != Running && (!cost || !v.CostPending) {
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

func TestShortBlogRunsAndCostsResolve(t *testing.T) {
	e := newEnv(t, 100, 0.014) // threshold far above estimate
	ctx := reqid.With(context.Background(), "r")
	j, err := e.core.Handle(ctx, link("https://blog.example/a"))
	if err != nil {
		t.Fatal(err)
	}
	v := waitJob(t, j, true)
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
	if sc.Calls != 2 || sc.UnknownCalls != 0 || sc.TotalUsd < 0.099 || sc.TotalUsd > 0.101 {
		t.Errorf("session cost = %+v, want 2 priced calls totalling 0.10", sc)
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
