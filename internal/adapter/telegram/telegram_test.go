package telegram

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

const token = "123:SECRET-TOKEN"

var voiceBytes = []byte("fake-ogg-voice-note")

// fakeTelegram serves the Bot API methods the adapter uses and records calls.
type fakeTelegram struct {
	srv     *httptest.Server
	mu      sync.Mutex
	updates []map[string]any // handed out once, by the next getUpdates
	calls   []call
	nextMsg int64
}

type call struct{ method, body string }

func newFakeTelegram(updates ...map[string]any) *fakeTelegram {
	f := &fakeTelegram{updates: updates, nextMsg: 500}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/file/bot"+token+"/") {
			w.Write(voiceBytes) // a downloaded voice note
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/bot"+token+"/")
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
		case "sendMessage", "sendAudio", "sendVoice":
			f.nextMsg++
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": f.nextMsg}})
		case "getFile":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": "voice/file_1.oga"}})
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

// lastID is the message id Telegram handed out most recently.
func (f *fakeTelegram) lastID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nextMsg
}

// fakeCore stands in for the Sevi API. result picks what a finished job holds.
type fakeCore struct {
	srv          *httptest.Server
	confirmFirst bool
	result       string // "blog" | "voice" | "text"
	mu           sync.Mutex
	messages     []map[string]any // bodies POSTed to /v1/messages
	threads      []map[string]string
}

func newFakeCore(result string, confirmFirst bool) *fakeCore {
	f := &fakeCore{result: result, confirmFirst: confirmFirst}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		f.mu.Lock()
		f.messages = append(f.messages, m)
		f.mu.Unlock()
		parts, _ := m["parts"].([]any)
		first, _ := parts[0].(map[string]any)
		if first["type"] == "url" && f.confirmFirst {
			json.NewEncoder(w).Encode(map[string]any{"job_id": "j1", "session_id": "s1", "status": "needs_confirmation",
				"events": []any{map[string]any{"seq": 1, "kind": "confirm", "message": "about $0.40. Go ahead?"}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"job_id": "j1", "session_id": "s1", "status": "running", "events": []any{}})
	})
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		var part map[string]any
		switch f.result {
		case "voice":
			part = map[string]any{"type": "audio", "message_id": "m2", "seconds": 42, "mime": "audio/ogg", "cost_usd": 0.0027144}
		case "text":
			part = map[string]any{"type": "text", "message_id": "m3", "text": "It means the price went up.", "cost_usd": 0.0003}
		default:
			part = map[string]any{"type": "audio", "message_id": "m1", "seconds": 534, "mime": "audio/mpeg", "title": "Cheap Satisfaction", "cost_usd": 0.12}
		}
		json.NewEncoder(w).Encode(map[string]any{"job_id": r.PathValue("id"), "session_id": "s1", "status": "done",
			"events": []any{map[string]any{"seq": 1, "kind": "progress", "message": "Thinking…"}, map[string]any{"seq": 2, "kind": "done", "message": "ok"}},
			"parts":  []any{part}})
	})
	mux.HandleFunc("GET /v1/audio/{id}", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("fake-audio-bytes")) })
	mux.HandleFunc("GET /v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": "s1", "url": "https://www.thisisvoid.in/blogs/2026_10/cheap_satisfy"})
	})
	mux.HandleFunc("POST /v1/threads", func(w http.ResponseWriter, r *http.Request) {
		var t map[string]string
		json.NewDecoder(r.Body).Decode(&t)
		f.mu.Lock()
		f.threads = append(f.threads, t)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeCore) firstMessage(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) == 0 {
		t.Fatal("core received no message")
	}
	return f.messages[0]
}

func (f *fakeCore) hasThread(ref, messageID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, th := range f.threads {
		if th["thread_ref"] == ref && th["message_id"] == messageID && th["adapter"] == "telegram" {
			return true
		}
	}
	return false
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

func msgUpdate(id int, from int64, msgID int, extra map[string]any) map[string]any {
	m := map[string]any{"message_id": msgID, "from": map[string]any{"id": from}, "chat": map[string]any{"id": from}}
	for k, v := range extra {
		m[k] = v
	}
	return map[string]any{"update_id": id, "message": m}
}

func TestLinkBecomesAudioWithTitleCostAndThread(t *testing.T) {
	tg := newFakeTelegram(msgUpdate(1, 42, 7, map[string]any{"text": "please read https://www.thisisvoid.in/blogs/2026_10/cheap_satisfy."}))
	core := newFakeCore("blog", false)
	runBot(t, tg, core)

	eventually(t, "audio sent", func() bool { return tg.count("sendAudio") == 1 })
	if !tg.find("sendAudio", "Cheap Satisfaction") || !tg.find("sendAudio", "534") || !tg.find("sendAudio", "$0.12") {
		t.Error("audio upload should carry the title, duration and cost")
	}
	if tg.count("deleteMessage") != 1 {
		t.Error("status message should be deleted once the audio is sent")
	}
	eventually(t, "blog audio registered as a thread", func() bool {
		return core.hasThread("42:"+itoa(tg.lastID()), "m1")
	})
}

func TestVoiceQuestionIsAReplyAndAnswerComesBackAsVoiceNote(t *testing.T) {
	tg := newFakeTelegram(msgUpdate(1, 42, 8, map[string]any{
		"voice":            map[string]any{"file_id": "F1", "duration": 5, "mime_type": "audio/ogg"},
		"reply_to_message": map[string]any{"message_id": 7},
	}))
	core := newFakeCore("voice", false)
	runBot(t, tg, core)

	eventually(t, "voice answer sent", func() bool { return tg.count("sendVoice") == 1 })
	m := core.firstMessage(t)
	if m["adapter"] != "telegram" || m["thread_ref"] != "42:8" || m["reply_to_ref"] != "42:7" {
		t.Errorf("core should be told the message handle and the one it replies to: %v", m)
	}
	p := m["parts"].([]any)[0].(map[string]any)
	got, _ := base64.StdEncoding.DecodeString(p["data"].(string))
	if p["type"] != "audio" || string(got) != string(voiceBytes) || p["mime"] != "audio/ogg" {
		t.Errorf("the downloaded voice note should reach core as audio: %v", p)
	}
	if !tg.find("sendVoice", `"message_id":8`) {
		t.Error("the answer should be sent as a reply to the question")
	}
	if !tg.find("sendVoice", "$0.0027") {
		t.Error("the voice note should show what the answer cost")
	}
	if tg.count("deleteMessage") != 1 {
		t.Error("the status message should go away once the voice note is sent")
	}
	eventually(t, "answer registered", func() bool { return core.hasThread("42:"+itoa(tg.lastID()), "m2") })
}

func TestTypedQuestionGetsATextAnswerInPlaceOfTheStatus(t *testing.T) {
	tg := newFakeTelegram(msgUpdate(1, 42, 9, map[string]any{"text": "what does that mean? answer in text"}))
	core := newFakeCore("text", false)
	runBot(t, tg, core)

	eventually(t, "text answer shown", func() bool { return tg.find("editMessageText", "It means the price went up.") })
	if !tg.find("editMessageText", "$0.0003") {
		t.Error("a text answer should end with what it cost")
	}
	if tg.count("sendVoice") != 0 || tg.count("sendAudio") != 0 {
		t.Error("a text answer must not send audio")
	}
	p := core.firstMessage(t)["parts"].([]any)[0].(map[string]any)
	if p["type"] != "text" {
		t.Errorf("a typed question should reach core as text: %v", p)
	}
	// The status message became the answer, so a reply to it must find the conversation.
	eventually(t, "answer registered", func() bool { return core.hasThread("42:"+itoa(tg.lastID()), "m3") })
}

func TestStrangerIsIgnored(t *testing.T) {
	tg := newFakeTelegram(msgUpdate(1, 999, 7, map[string]any{"text": "https://example.com/post"}),
		msgUpdate(2, 999, 8, map[string]any{"voice": map[string]any{"file_id": "F9"}}))
	core := newFakeCore("blog", false)
	runBot(t, tg, core)

	time.Sleep(300 * time.Millisecond)
	for _, m := range []string{"sendMessage", "sendAudio", "sendVoice", "editMessageText", "getFile"} {
		if n := tg.count(m); n != 0 {
			t.Errorf("%s called %d times for a user not on the allowlist", m, n)
		}
	}
}

func TestConfirmationButtons(t *testing.T) {
	tg := newFakeTelegram(msgUpdate(1, 42, 7, map[string]any{"text": "https://example.com/long"}))
	core := newFakeCore("blog", true)
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
	last := core.messages[len(core.messages)-1]
	if p := last["parts"].([]any)[0].(map[string]any); p["decision"] != "yes" || last["session_id"] != "s1" {
		t.Errorf("core should be told yes for session s1: %v", last)
	}
}

func TestTokenIsRedactedFromErrors(t *testing.T) {
	b := &botAPI{base: "http://127.0.0.1:1", token: token} // nothing listens: the transport error contains the URL
	for name, err := range map[string]error{
		"call":     b.call(context.Background(), "getMe", nil, nil),
		"download": func() error { _, e := b.downloadFile(context.Background(), "x"); return e }(),
	} {
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		if strings.Contains(err.Error(), "SECRET-TOKEN") {
			t.Errorf("%s: token leaked into error text: %v", name, err)
		}
	}
}

func TestCostIsHiddenWhenDisabled(t *testing.T) {
	b := New(Options{ShowCost: false}, zap.NewNop().Sugar())
	c := 0.0027
	if b.costLine(&c) != "" {
		t.Error("cost.show_in_reply=false must hide the cost")
	}
	b.showCost = true
	if b.costLine(nil) != "" {
		t.Error("an unknown cost shows nothing rather than $0.00")
	}
	if got := b.costLine(&c); got != "$0.0027" {
		t.Errorf("got %q, want $0.0027", got)
	}
	big := 0.1149
	if got := b.costLine(&big); got != "$0.11" {
		t.Errorf("got %q, want $0.11", got)
	}
}

func TestFirstURLAndCaption(t *testing.T) {
	for in, want := range map[string]string{
		"read https://a.com/x, please": "https://a.com/x",
		"(https://a.com/x)":            "https://a.com/x",
		"no link here":                 "",
	} {
		if got := firstURL(in); got != want {
			t.Errorf("firstURL(%q) = %q, want %q", in, got, want)
		}
	}
	cost := 0.12
	if got := buildCaption("Title", 534, &cost); got != "Title\n9 min · $0.12" {
		t.Errorf("got %q", got)
	}
	if got := buildCaption("Title", 60, nil); got != "Title\n60 sec" {
		t.Errorf("got %q", got)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
