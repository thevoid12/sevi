package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// ttsServer answers /audio/speech. fail decides, from the request, whether it
// answers with the OpenRouter "empty stream" 502.
type ttsServer struct {
	*httptest.Server
	mu    sync.Mutex
	calls []call
}

type call struct {
	Model, Input string
}

func newTTSServer(t *testing.T, fail func(model, input string) bool) *ttsServer {
	s := &ttsServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Model, Input string }
		json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.calls = append(s.calls, call{req.Model, req.Input})
		s.mu.Unlock()
		if fail(req.Model, req.Input) {
			w.WriteHeader(502)
			w.Write([]byte(`{"error":{"message":"Provider returned an empty audio stream","code":502}}`))
			return
		}
		w.Header().Set("X-Generation-Id", "gen-"+req.Model)
		w.Write(make([]byte, 100)) // 100 bytes of "audio" per call
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *ttsServer) count(model string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c.Model == model {
			n++
		}
	}
	return n
}

const text = "The first sentence is here. The second sentence follows it. The third one is next. The fourth closes it."

func resilient(url string, retries int, fallbacks ...string) *Resilient {
	primary := &OpenRouterTTS{BaseURL: url, Key: "k", Model: "main", Voice: "v1", Retries: retries, Log: zap.NewNop().Sugar()}
	r := &Resilient{Primary: primary, Log: zap.NewNop().Sugar()}
	for _, m := range fallbacks {
		r.Fallbacks = append(r.Fallbacks, &OpenRouterTTS{BaseURL: url, Key: "k", Model: m, Voice: "v2", Log: zap.NewNop().Sugar()})
	}
	return r
}

func TestResilientLeavesAHealthyChunkAlone(t *testing.T) {
	srv := newTTSServer(t, func(string, string) bool { return false })
	sp, err := resilient(srv.URL, 0).Synth(t.Context(), text)
	if err != nil || len(sp.PCM) != 100 || len(sp.Parts) != 0 {
		t.Fatalf("%v pcm=%d parts=%d", err, len(sp.PCM), len(sp.Parts))
	}
}

func TestResilientSplitsInHalvesAndDoesNotRetryThePieces(t *testing.T) {
	// The whole chunk always fails; anything shorter works.
	srv := newTTSServer(t, func(_, in string) bool { return in == text })
	sp, err := resilient(srv.URL, 1).Synth(t.Context(), text)
	if err != nil {
		t.Fatal(err)
	}
	// 2 tries of the whole chunk (1 retry), then exactly one try per half.
	if n := srv.count("main"); n != 2+2 {
		t.Errorf("calls = %d, want 4", n)
	}
	if len(sp.PCM) != 200 || len(sp.Parts) != 2 {
		t.Fatalf("pcm=%d parts=%d", len(sp.PCM), len(sp.Parts))
	}
	joined := sp.Parts[0].Chars + sp.Parts[1].Chars
	if joined < len([]rune(text))-2 || joined > len([]rune(text)) {
		t.Errorf("the halves should cover the text: %d of %d", joined, len([]rune(text)))
	}
	if sp.Parts[0].Model != "main" || sp.Parts[0].GenerationID != "gen-main" {
		t.Errorf("part: %+v", sp.Parts[0])
	}
}

func TestResilientSplitsOnlyThePieceThatFailsThenUsesTheFallback(t *testing.T) {
	// Fails on the whole chunk, on the second half, and on every piece of it
	// that mentions "third" (so its quarters fail too): that quarter goes to the fallback.
	srv := newTTSServer(t, func(model, in string) bool {
		return model == "main" && (in == text || strings.Contains(in, "third"))
	})
	sp, err := resilient(srv.URL, 0, "backup").Synth(t.Context(), text)
	if err != nil {
		t.Fatal(err)
	}
	if srv.count("backup") != 1 {
		t.Errorf("fallback calls = %d, want 1", srv.count("backup"))
	}
	var backups int
	for _, p := range sp.Parts {
		if p.Model == "backup" {
			backups++
		}
	}
	if backups != 1 || len(sp.Parts) < 3 {
		t.Errorf("parts: %+v", sp.Parts)
	}
	if len(sp.PCM) != 100*len(sp.Parts) {
		t.Errorf("pcm %d for %d parts", len(sp.PCM), len(sp.Parts))
	}
}

func TestResilientGivesUpWhenEverythingFails(t *testing.T) {
	srv := newTTSServer(t, func(string, string) bool { return true })
	_, err := resilient(srv.URL, 0, "backup").Synth(t.Context(), text)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("want the provider error, got %v", err)
	}
	// It stops at the first piece that cannot be done: the whole chunk, its first
	// half, that half's first quarter, then the one fallback try.
	if srv.count("main") != 3 || srv.count("backup") != 1 {
		t.Errorf("main=%d backup=%d, want 3 and 1", srv.count("main"), srv.count("backup"))
	}
}

func TestResilientDoesNotSplitARateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer srv.Close()
	_, err := resilient(srv.URL, 0, "backup").Synth(t.Context(), text)
	if err == nil {
		t.Fatal("want an error")
	}
}

func TestNoVoiceIsNotSent(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte{1, 2})
	}))
	defer srv.Close()
	tts := &OpenRouterTTS{BaseURL: srv.URL, Key: "k", Model: "m", Log: zap.NewNop().Sugar()}
	if _, err := tts.Synth(t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	if _, has := body["voice"]; has {
		t.Errorf("an empty voice must be left out: %v", body)
	}
}

func TestResilientSkipsAFailingPrimaryForAWhile(t *testing.T) {
	// The primary never works; the backup always does.
	srv := newTTSServer(t, func(model, _ string) bool { return model == "main" })
	r := resilient(srv.URL, 4, "backup")
	now := time.Unix(1_000_000, 0)
	r.now = func() time.Time { return now }

	for i := 0; i < primaryStreak; i++ { // each chunk tries the primary at most twice, then is cut up
		if _, err := r.Synth(t.Context(), text); err != nil {
			t.Fatal(err)
		}
	}
	tried := srv.count("main")
	// Per chunk: 5 attempts on the whole chunk (retries=4), then no retries: 2 halves, 4 quarters.
	if want := primaryStreak * (5 + 2 + 4); tried != want {
		t.Errorf("primary was tried %d times, want %d", tried, want)
	}
	// Now the primary is skipped: the whole chunk goes to the backup in one call.
	sp, err := r.Synth(t.Context(), text)
	if err != nil || len(sp.Parts) != 1 || sp.Parts[0].Model != "backup" {
		t.Fatalf("want one backup call, got %+v, %v", sp.Parts, err)
	}
	if srv.count("main") != tried {
		t.Error("the primary must not be tried during the cooldown")
	}
	// After the cooldown it is tried again.
	now = now.Add(primaryCooldown + time.Second)
	if _, err := r.Synth(t.Context(), text); err != nil {
		t.Fatal(err)
	}
	if srv.count("main") == tried {
		t.Error("the primary should be tried again after the cooldown")
	}
}
