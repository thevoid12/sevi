package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

const token = "123:SECRET-TOKEN"

// fakeTelegram serves the Bot API methods the adapter uses and records calls.
type fakeTelegram struct {
	srv *httptest.Server
	mu  sync.Mutex
	// updates are handed out once, by the first getUpdates call.
	updates []map[string]any
	calls   []call
	nextMsg int64
}

type call struct {
	method string
	body   string
}

func newFakeTelegram(updates ...map[string]any) *fakeTelegram {
	f := &fakeTelegram{updates: updates, nextMsg: 500}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/bot"+token+"/")
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, call{method, string(raw)})
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getUpdates":
			ups := f.updates
			f.updates = nil
			if len(ups) == 0 {
				f.mu.Unlock()
				time.Sleep(30 * time.Millisecond)
				f.mu.Lock()
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": append([]map[string]any{}, ups...)})
		case "sendMessage", "sendAudio":
			f.nextMsg++
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": f.nextMsg}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		}
	}))
	return f
}

func (f *fakeTelegram) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.method == method {
			n++
		}
	}
	return n
}

func (f *fakeTelegram) find(method, contains string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.method == method && strings.Contains(c.body, contains) {
			return true
		}
	}
	return false
}

// fakeCore stands in for the Sevi API. confirmFirst makes the first link ask
// for confirmation.
type fakeCore struct {
	srv          *httptest.Server
	confirmFirst bool
	mu           sync.Mutex
	threads      []string
	confirmed    string
}

func newFakeCore(confirmFirst bool) *fakeCore {
	f := &fakeCore{confirmFirst: confirmFirst}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			Parts []struct {
				Type, Decision string
			} `json:"parts"`
		}
		json.NewDecoder(r.Body).Decode(&m)
		if m.Parts[0].Type == "confirm" {
			f.mu.Lock()
			f.confirmed = m.Parts[0].Decision
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"job_id": "j2", "session_id": "s1", "status": "running", "events": []any{}})
			return
		}
		if f.confirmFirst {
			json.NewEncoder(w).Encode(map[string]any{"job_id": "j1", "session_id": "s1", "status": "needs_confirmation",
				"events": []any{map[string]any{"seq": 1, "kind": "confirm", "message": "about $0.40. Go ahead?"}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"job_id": "j1", "session_id": "s1", "status": "running", "events": []any{}})
	})
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"job_id": r.PathValue("id"), "session_id": "s1", "status": "done",
			"events": []any{
				map[string]any{"seq": 1, "kind": "progress", "message": "Generating audio 1/1"},
				map[string]any{"seq": 2, "kind": "cost", "message": "Cost $0.12", "cost_usd": 0.12},
			},
			"parts":        []any{map[string]any{"type": "audio", "message_id": "m1", "seconds": 534, "title": "Cheap Satisfaction"}},
			"cost_pending": false})
	})
	mux.HandleFunc("GET /v1/audio/{id}", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("fake-mp3-bytes")) })
	mux.HandleFunc("GET /v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": "s1", "url": "https://www.thisisvoid.in/blogs/2026_10/cheap_satisfy"})
	})
	mux.HandleFunc("POST /v1/threads", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.threads = append(f.threads, string(raw))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func runBot(t *testing.T, tg *fakeTelegram, core *fakeCore) {
	t.Helper()
	bot := New(Options{Token: token, AllowedIDs: []int64{42}, CoreURL: core.srv.URL, ShowCost: true, TelegramURL: tg.srv.URL}, zap.NewNop().Sugar())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { bot.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; tg.srv.Close(); core.srv.Close() })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func textUpdate(id int, from int64, text string) map[string]any {
	return map[string]any{"update_id": id, "message": map[string]any{
		"message_id": 1, "from": map[string]any{"id": from}, "chat": map[string]any{"id": from}, "text": text}}
}

func TestLinkBecomesAudioWithCostAndThread(t *testing.T) {
	tg := newFakeTelegram(textUpdate(1, 42, "please read https://www.thisisvoid.in/blogs/2026_10/cheap_satisfy."))
	core := newFakeCore(false)
	runBot(t, tg, core)

	eventually(t, "audio sent", func() bool { return tg.count("sendAudio") == 1 })
	eventually(t, "cost added to caption", func() bool { return tg.find("editMessageCaption", "$0.12") })
	if !tg.find("sendAudio", "Cheap Satisfaction") {
		t.Error("audio upload should carry the article title")
	}
	if !tg.find("editMessageCaption", "Cheap Satisfaction") {
		t.Error("the caption edited in with the cost must keep the title")
	}
	if !tg.find("sendAudio", "534") {
		t.Error("audio upload should carry the duration")
	}
	if tg.count("deleteMessage") != 1 {
		t.Error("status message should be deleted once the audio is sent")
	}
	eventually(t, "thread registered", func() bool {
		core.mu.Lock()
		defer core.mu.Unlock()
		return len(core.threads) == 1 && strings.Contains(core.threads[0], `"thread_ref":"42:`) && strings.Contains(core.threads[0], `"session_id":"s1"`)
	})
}

func TestStrangerIsIgnored(t *testing.T) {
	tg := newFakeTelegram(textUpdate(1, 999, "https://example.com/post"))
	core := newFakeCore(false)
	runBot(t, tg, core)

	time.Sleep(300 * time.Millisecond)
	for _, m := range []string{"sendMessage", "sendAudio", "editMessageText"} {
		if n := tg.count(m); n != 0 {
			t.Errorf("%s called %d times for a user not on the allowlist", m, n)
		}
	}
}

func TestConfirmationButtons(t *testing.T) {
	tg := newFakeTelegram(textUpdate(1, 42, "https://example.com/long"))
	core := newFakeCore(true)
	runBot(t, tg, core)

	eventually(t, "confirm question with buttons", func() bool {
		return tg.find("editMessageText", "Go ahead?") && tg.find("editMessageText", "c:s1:y") && tg.find("editMessageText", "c:s1:n")
	})
	if tg.count("sendAudio") != 0 {
		t.Fatal("no audio before the user answers")
	}

	tg.mu.Lock()
	tg.updates = append(tg.updates, map[string]any{"update_id": 2, "callback_query": map[string]any{
		"id": "cb1", "from": map[string]any{"id": 42}, "data": "c:s1:y",
		"message": map[string]any{"message_id": 501, "chat": map[string]any{"id": 42}}}})
	tg.mu.Unlock()

	eventually(t, "audio after yes", func() bool { return tg.count("sendAudio") == 1 })
	core.mu.Lock()
	defer core.mu.Unlock()
	if core.confirmed != "yes" {
		t.Errorf("core was told %q, want yes", core.confirmed)
	}
}

func TestTokenIsRedactedFromErrors(t *testing.T) {
	b := &botAPI{base: "http://127.0.0.1:1", token: token} // nothing listens: transport error with the URL in it
	err := b.call(context.Background(), "getMe", nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Errorf("token leaked into error text: %v", err)
	}
}

func TestFirstURL(t *testing.T) {
	for in, want := range map[string]string{
		"read https://a.com/x, please": "https://a.com/x",
		"(https://a.com/x)":            "https://a.com/x",
		"no link here":                 "",
	} {
		if got := firstURL(in); got != want {
			t.Errorf("firstURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildCaption(t *testing.T) {
	cost := 0.12
	if got := buildCaption("Title", 534, &cost); got != "Title\n9 min · $0.12" {
		t.Errorf("got %q", got)
	}
	if got := buildCaption("Title", 60, nil); got != "Title\n60 sec" {
		t.Errorf("got %q", got)
	}
}
