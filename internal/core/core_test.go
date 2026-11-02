package core

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	"sevi/internal/event"
	"sevi/internal/httpx"
	"sevi/internal/model"
	"sevi/internal/objectstore"
	"sevi/internal/pricing"
	"sevi/internal/provider"
	"sevi/internal/reqid"
	"sevi/internal/store/storetest"
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
	return provider.Speech{PCM: make([]byte, 48000), GenerationID: "gen"}, nil
}

type fakeLLM struct{}

func (fakeLLM) Chat(context.Context, provider.ChatRequest) (provider.ChatResult, error) {
	return provider.ChatResult{}, errors.New("llm is not used by link jobs")
}

// page builds a Jina-style markdown page of about n characters.
func page(title string, n int) string {
	return "Title: " + title + "\n\nMarkdown Content:\n" + strings.Repeat("This is a sentence that will be read aloud. ", n/44)
}

// TestLinksEndToEnd drives real Core, store, object store and ffmpeg with only the
// network faked (page fetch, TTS, prices). One row per kind of link.
func TestLinksEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	cases := []struct {
		name      string
		url       string
		page      string
		ttsFailAt int32

		wantErr    bool   // Handle itself rejects the message
		wantStatus Status // when the job runs
		wantTitle  string
		wantTTS    int32 // tts calls after the first share
		wantMsg    string
		reshare    bool // sharing it again must be free
		confirm    bool // answer "yes" to the cost prompt and expect Done
	}{
		{name: "medium", url: "https://medium.com/@someone/a-post-123", page: page("A Medium Post", 5700),
			wantStatus: Done, wantTitle: "A Medium Post", wantTTS: 2, reshare: true},
		{name: "x thread", url: "https://x.com/someone/status/1234567890", page: page("Someone on X", 2000),
			wantStatus: Done, wantTitle: "Someone on X", wantTTS: 1, reshare: true},
		{name: "plain blog", url: "https://blog.example/a", page: page("A Blog Post", 5700),
			wantStatus: Done, wantTitle: "A Blog Post", wantTTS: 2, reshare: true},
		{name: "long post asks first", url: "https://blog.example/long", page: page("Long Read", 600000),
			wantStatus: NeedsConfirmation, wantTTS: 0, confirm: true},
		{name: "tts fails on the first chunk", url: "https://substack.example/p/x", page: page("Fails", 5700), ttsFailAt: 1,
			wantStatus: Failed, wantTTS: 1, wantMsg: "part 1 of 2"},
		{name: "not http", url: "ftp://x", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := zap.NewNop().Sugar()
			dir := t.TempDir()
			st := storetest.Open(t, log)

			cfg := viper.New()
			cfg.Set("models.tts.provider", "p")
			cfg.Set("models.tts.model", "m")
			cfg.Set("models.tts.voice", "v")
			cfg.Set("models.tts.chunk_chars", 3000)
			cfg.Set("models.llm.provider", "p")
			cfg.Set("models.llm.model", "llm-m")
			cfg.Set("reply.voice", true)
			cfg.Set("cost.confirm_over_usd", 5)
			cfg.Set("cost.est_usd_per_1k_chars", 0.014)
			tts := &fakeTTS{failAt: tc.ttsFailAt}
			prices := &pricing.Book{
				Store: st, TTL: time.Hour, Log: log,
				Sources: map[string]pricing.Source{"p": func(context.Context) (map[string]provider.Price, error) {
					return map[string]provider.Price{
						"m":     {Prompt: 1e-6, Completion: 1e-4},
						"llm-m": {Prompt: 1e-7, Completion: 4e-7},
					}, nil
				}},
			}
			c := New(Deps{
				Service: &model.Service{Cfg: cfg, Store: st, Objects: objectstore.Local{Root: dir + "/objects"}, Log: log},
				TTS:     tts, LLM: fakeLLM{}, Prices: prices,
				Markdown: func(_ context.Context, u string) (string, error) {
					if u != tc.url {
						t.Errorf("fetched %q, want %q", u, tc.url)
					}
					return tc.page, nil
				},
			})
			t.Cleanup(c.Close)

			send := func(m Message) JobView {
				t.Helper()
				j, err := c.Handle(reqid.With(context.Background(), "ref-123"), m)
				if err != nil {
					t.Fatalf("Handle: %v", err)
				}
				return waitDone(t, j)
			}
			share := Message{Parts: []Part{{Type: "url", URL: tc.url}}}

			if tc.wantErr {
				_, err := c.Handle(context.Background(), share)
				var ue *event.UserError
				if !errors.As(err, &ue) || ue.Message == "" {
					t.Fatalf("want a UserError with a message, got %v", err)
				}
				return
			}

			v := send(share)
			if v.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s (events %+v)", v.Status, tc.wantStatus, v.Events)
			}
			if got := tts.calls.Load(); got != tc.wantTTS {
				t.Errorf("tts calls = %d, want %d", got, tc.wantTTS)
			}
			last := v.Events[len(v.Events)-1]

			switch tc.wantStatus {
			case Done:
				if len(v.Parts) != 1 || v.Parts[0].Type != "audio" || v.Parts[0].Title != tc.wantTitle {
					t.Fatalf("want one audio part titled %q, got %+v", tc.wantTitle, v.Parts)
				}
				if v.Parts[0].CostUSD == nil || *v.Parts[0].CostUSD <= 0 {
					t.Errorf("audio should carry a cost: %+v", v.Parts[0])
				}
				sc, err := st.SessionCost(context.Background(), v.SessionID)
				if err != nil || int32(sc.Calls) != tc.wantTTS || sc.UnknownCalls != 0 {
					t.Errorf("ledger = %+v, %v; want %d priced calls", sc, err, tc.wantTTS)
				}
			case NeedsConfirmation:
				if last.Kind != event.Confirm || !strings.Contains(last.Message, "$") {
					t.Errorf("confirm event should carry an estimate, got %+v", last)
				}
			case Failed:
				if last.Kind != event.Error || last.Ref != "ref-123" || !strings.Contains(last.Message, tc.wantMsg) {
					t.Errorf("error event should name the part and carry the ref: %+v", last)
				}
				if strings.Contains(last.Message, "SECRET-PROVIDER-DETAIL") || strings.Contains(last.Message, "tts.example") {
					t.Errorf("raw provider text leaked to the user: %q", last.Message)
				}
			}

			if tc.confirm {
				v2 := send(Message{SessionID: v.SessionID, Parts: []Part{{Type: "confirm", Decision: "yes"}}})
				if v2.Status != Done || tts.calls.Load() == 0 {
					t.Fatalf("after yes: status %s, tts calls %d", v2.Status, tts.calls.Load())
				}
			}
			if tc.reshare {
				before := tts.calls.Load()
				v2 := send(share)
				if v2.Status != Done || tts.calls.Load() != before || v2.Parts[0].Title != tc.wantTitle {
					t.Errorf("reshare should be a free cache hit: status %s, new tts calls %d, parts %+v",
						v2.Status, tts.calls.Load()-before, v2.Parts)
				}
			}
		})
	}
}

func waitDone(t *testing.T, j *Job) JobView {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		ch := j.Changed()
		if v := j.View(0); v.Status != Running {
			return v
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("job did not finish: %+v", j.View(0))
		}
	}
}
