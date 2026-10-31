package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
