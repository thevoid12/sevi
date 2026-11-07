package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func testBot(t *testing.T) *Bot {
	t.Helper()
	b, err := New(Options{
		Token: "tok", WebhookSecret: "s3cret", AllowedIDs: []int64{1},
		CoreURL: "http://127.0.0.1:1", CoreToken: func() (string, error) { return "t", nil },
	}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func post(h http.Handler, method, secret, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/telegram", strings.NewReader(body))
	if secret != "" {
		r.Header.Set(secretHeader, secret)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestNewRequiresSecretsAndCore(t *testing.T) {
	tok := func() (string, error) { return "", nil }
	for name, o := range map[string]Options{
		"no token":  {WebhookSecret: "s", CoreURL: "http://x", CoreToken: tok},
		"no secret": {Token: "t", CoreURL: "http://x", CoreToken: tok},
		"no core":   {Token: "t", WebhookSecret: "s"},
	} {
		if _, err := New(o, zap.NewNop().Sugar()); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestWebhookChecks(t *testing.T) {
	h := testBot(t).Handler(context.Background())
	// A user not on the allowlist: acknowledged, never reaches Telegram or Core.
	ok := `{"update_id": 10, "message": {"message_id": 1, "from": {"id": 999}, "chat": {"id": 5}, "text": "hi"}}`
	for _, tc := range []struct {
		name, method, secret, body string
		want                       int
	}{
		{"get", http.MethodGet, "s3cret", "", 405},
		{"no secret", http.MethodPost, "", ok, 403},
		{"wrong secret", http.MethodPost, "nope", ok, 403},
		{"bad json", http.MethodPost, "s3cret", "{", 400},
		{"no update id", http.MethodPost, "s3cret", `{}`, 400},
		{"too big", http.MethodPost, "s3cret", `{"update_id":1,"x":"` + strings.Repeat("a", maxBodyBytes) + `"}`, 413},
		{"ok", http.MethodPost, "s3cret", ok, 200},
		{"duplicate", http.MethodPost, "s3cret", ok, 200},
	} {
		if got := post(h, tc.method, tc.secret, tc.body).Code; got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestSeenDedupesAndIsBounded(t *testing.T) {
	b := testBot(t)
	if b.seen(7) || !b.seen(7) {
		t.Error("first sight false, second true")
	}
	for i := 100; i < 100+seenWindow+5; i++ {
		b.seen(i)
	}
	if len(b.seenIDs) > seenWindow || len(b.seenOrd) > seenWindow {
		t.Errorf("window not bounded: %d", len(b.seenIDs))
	}
	if b.seen(7) {
		t.Error("old id should have been forgotten")
	}
}

func TestCommandStripsBotName(t *testing.T) {
	for in, want := range map[string]string{"/stop": "/stop", "/STOP@sevi_bot": "/stop", "/stop now": "/stop", "stop": "", "https://x.y/z": ""} {
		if got := command(in); got != want {
			t.Errorf("command(%q) = %q, want %q", in, got, want)
		}
	}
}

// /stop as a reply to a blog's message stops that job; without a reply it stops
// the chat's only job; with nothing to stop it says so.
func TestStopCommandFindsTheJob(t *testing.T) {
	var stopped, said []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/stop"):
			stopped = append(stopped, r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var body struct {
				Text string `json:"text"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			said = append(said, body.Text)
			w.Write([]byte(`{"ok":true,"result":{"message_id":99}}`))
		default:
			t.Errorf("unexpected call %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	b, err := New(Options{
		Token: "tok", WebhookSecret: "s", AllowedIDs: []int64{1}, TelegramURL: srv.URL,
		CoreURL: srv.URL, CoreToken: func() (string, error) { return "t", nil },
	}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	b.stop(ctx, in{chatID: 5, msgID: 20, text: "/stop"})
	if len(stopped) != 0 || len(said) != 1 || !strings.Contains(said[0], "Nothing is running") {
		t.Fatalf("nothing running: stopped=%v said=%v", stopped, said)
	}

	done := b.track(5, 11, 10, "job-1") // status message 11, the user's link was message 10
	b.stop(ctx, in{chatID: 5, msgID: 21, replyTo: 11, text: "/stop"})
	b.stop(ctx, in{chatID: 5, msgID: 22, replyTo: 10, text: "/stop"})
	b.stop(ctx, in{chatID: 5, msgID: 23, text: "/stop"}) // not a reply: the chat's only job
	if len(stopped) != 3 || stopped[0] != "/v1/jobs/job-1/stop" {
		t.Fatalf("stopped = %v", stopped)
	}

	b.track(5, 31, 30, "job-2")
	b.stop(ctx, in{chatID: 5, msgID: 24, text: "/stop"}) // two jobs and no reply: ambiguous
	b.stop(ctx, in{chatID: 5, msgID: 25, replyTo: 999, text: "/stop"})
	if len(stopped) != 3 {
		t.Errorf("an ambiguous or unknown /stop must stop nothing: %v", stopped)
	}
	done()
	b.stop(ctx, in{chatID: 5, msgID: 26, replyTo: 11, text: "/stop"})
	if len(stopped) != 3 {
		t.Errorf("a finished job is forgotten: %v", stopped)
	}
}

// When the server stops while a job runs, the person is told, not left looking at a frozen status.
func TestFollowTellsThePersonWhenTheServerStops(t *testing.T) {
	var mu sync.Mutex
	var edits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			var body struct {
				Text string `json:"text"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			edits = append(edits, body.Text)
			mu.Unlock()
			w.Write([]byte(`{"ok":true,"result":true}`))
		default: // the job poll: the job is still running
			w.Write([]byte(`{"job_id":"j","session_id":"s","status":"running","events":[]}`))
		}
	}))
	defer srv.Close()
	b, err := New(Options{
		Token: "tok", WebhookSecret: "s", AllowedIDs: []int64{1}, TelegramURL: srv.URL,
		CoreURL: srv.URL, CoreToken: func() (string, error) { return "t", nil },
	}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.follow(ctx, 5, 11, 10, "https://blog.example/x", jobView{ID: "j", SessionID: "s", Status: "running"})
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel() // the server is shutting down
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not return")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1], "restarted") || !strings.Contains(edits[len(edits)-1], "send the link again") {
		t.Errorf("status edits: %q", edits)
	}
}
