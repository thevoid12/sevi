package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"sevi/internal/auth"
)

var secret = []byte(strings.Repeat("s", 40))

// newTestAPI has no Core behind it on purpose: every request these tests expect
// to be refused must be refused before any handler (which would use the Core) runs.
func newTestAPI(t *testing.T) http.Handler {
	t.Helper()
	v, err := auth.NewVerifier(secret, []string{"telegram", "web"})
	if err != nil {
		t.Fatal(err)
	}
	return New(nil, nil, nil, zap.NewNop().Sugar(), v).Handler()
}

func do(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEveryRouteRefusesACallerWithoutAValidToken(t *testing.T) {
	h := newTestAPI(t)
	badSecret, _ := auth.Mint([]byte(strings.Repeat("x", 40)), "telegram", time.Minute)
	unknown, _ := auth.Mint(secret, "discord", time.Minute) // right secret, adapter not on the list

	routes := [][2]string{
		{"POST", "/v1/messages"}, {"GET", "/v1/jobs/abc"}, {"GET", "/v1/audio/abc"},
		{"POST", "/v1/threads"}, {"GET", "/v1/sessions/abc"}, {"GET", "/v1/sessions/abc/cost"},
	}
	for _, r := range routes {
		for name, tok := range map[string]string{"no token": "", "garbage": "abc.def.ghi", "wrong secret": badSecret, "unknown adapter": unknown} {
			rec := do(h, r[0], r[1], tok, `{}`)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: status %d, want 401", r[0], r[1], name, rec.Code)
			}
			if body := rec.Body.String(); strings.Contains(body, "signature") || strings.Contains(body, "unknown adapter") || strings.Contains(body, "expired") {
				t.Errorf("%s %s: the refusal explains too much to the caller: %s", r[0], r[1], body)
			}
			if rec.Header().Get("X-Request-Id") == "" {
				t.Errorf("%s %s: a refusal should still carry a request id for the log", r[0], r[1])
			}
		}
	}
}

func TestAnAdapterCannotSpeakAsAnotherOrFileThreadsUnderIt(t *testing.T) {
	h := newTestAPI(t)
	tok, _ := auth.Mint(secret, "telegram", time.Minute)

	// Authenticated as telegram, claiming to be web: refused before the Core is touched.
	rec := do(h, "POST", "/v1/messages", tok, `{"adapter":"web","parts":[{"type":"text","text":"hi"}]}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("posing as another adapter: status %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	rec = do(h, "POST", "/v1/threads", tok, `{"adapter":"web","thread_ref":"1:1","message_id":"m"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("registering a thread under another adapter: status %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
}
