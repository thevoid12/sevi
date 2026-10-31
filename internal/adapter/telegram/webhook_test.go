package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeTelegram(t *testing.T, got *map[string]any, info string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bottok/") {
			t.Errorf("token not in path: %s", r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, "/setWebhook") {
			json.NewDecoder(r.Body).Decode(got)
		}
		if strings.HasSuffix(r.URL.Path, "/getWebhookInfo") {
			w.Write([]byte(info))
			return
		}
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
}

func TestSetWebhookSendsSecretAndUpdateKinds(t *testing.T) {
	var got map[string]any
	srv := fakeTelegram(t, &got, "")
	defer srv.Close()
	a := NewAdmin("tok", srv.URL)
	if err := a.SetWebhook(context.Background(), "https://gw.example/telegram?key=K", "s3cret-ok_1"); err != nil {
		t.Fatal(err)
	}
	if got["url"] != "https://gw.example/telegram?key=K" || got["secret_token"] != "s3cret-ok_1" {
		t.Errorf("wrong params: %v", got)
	}
	if ups, _ := got["allowed_updates"].([]any); len(ups) != 2 {
		t.Errorf("allowed_updates: %v", got["allowed_updates"])
	}
}

func TestSetWebhookRejectsBadInput(t *testing.T) {
	a := NewAdmin("tok", "http://127.0.0.1:1") // never reached
	for name, c := range map[string][2]string{
		"http url":     {"http://gw.example/telegram", "ok"},
		"no host":      {"https://", "ok"},
		"empty secret": {"https://gw.example/t", ""},
		"bad chars":    {"https://gw.example/t", "has space"},
		"not a url":    {"nope", "ok"},
	} {
		if err := a.SetWebhook(context.Background(), c[0], c[1]); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestInfoHidesTheGatewayKey(t *testing.T) {
	var got map[string]any
	srv := fakeTelegram(t, &got, `{"ok":true,"result":{"url":"https://gw.example/telegram?key=SUPERSECRET","pending_update_count":2,"max_connections":4}}`)
	defer srv.Close()
	info, err := NewAdmin("tok", srv.URL).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(info.String(), "SUPERSECRET") || !strings.Contains(info.String(), "gw.example/telegram") {
		t.Errorf("key leaked or url missing: %s", info)
	}
}
