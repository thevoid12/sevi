package core

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	"sevi/internal/event"
	"sevi/internal/model"
	"sevi/internal/objectstore"
	"sevi/internal/pricing"
	"sevi/internal/provider"
	"sevi/internal/reqid"
	"sevi/internal/store"
	"sevi/internal/store/storetest"
)

// A link that fails at the second chunk keeps the first one; sharing it again
// makes only the missing chunk and then finishes, without asking about cost again.
func TestFailedLinkContinuesFromTheSavedChunk(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	log := zap.NewNop().Sugar()
	st := storetest.Open(t, log)
	objects := objectstore.Local{Root: t.TempDir() + "/objects"}
	cfg := viper.New()
	cfg.Set("models.tts.provider", "p")
	cfg.Set("models.tts.model", "m")
	cfg.Set("models.tts.voice", "v")
	cfg.Set("models.tts.chunk_chars", 3000)
	cfg.Set("models.llm.provider", "p")
	cfg.Set("models.llm.model", "llm-m")
	cfg.Set("cost.confirm_over_usd", 0.0001) // would ask on a fresh start
	cfg.Set("cost.est_usd_per_1k_chars", 0.014)
	tts := &fakeTTS{failAt: 2}
	prices := &pricing.Book{Store: st, TTL: time.Hour, Log: log,
		Sources: map[string]pricing.Source{"p": func(context.Context) (map[string]provider.Price, error) {
			return map[string]provider.Price{"m": {Prompt: 1e-6, Completion: 1e-4}, "llm-m": {Prompt: 1e-7, Completion: 4e-7}}, nil
		}}}
	const url = "https://blog.example/resume"
	c := New(Deps{
		Service: &model.Service{Cfg: cfg, Store: st, Objects: objects, Log: log},
		TTS:     tts, LLM: fakeLLM{}, Prices: prices,
		Markdown: func(context.Context, string) (string, error) { return page("Resume Me", 5700), nil },
	})
	t.Cleanup(c.Close)
	send := func(m Message) JobView {
		t.Helper()
		j, err := c.Handle(reqid.With(context.Background(), "ref-1"), m)
		if err != nil {
			t.Fatal(err)
		}
		return waitDone(t, j)
	}
	share := Message{Parts: []Part{{Type: "url", URL: url}}}

	// The first run is confirmed by the person, then fails at chunk 2.
	v := send(share)
	if v.Status != NeedsConfirmation {
		t.Fatalf("first run: %s", v.Status)
	}
	v = send(Message{SessionID: v.SessionID, Parts: []Part{{Type: "confirm", Decision: "yes"}}})
	if v.Status != Failed {
		t.Fatalf("want Failed, got %s", v.Status)
	}
	last := v.Events[len(v.Events)-1]
	if last.Kind != event.Error || !strings.Contains(last.Message, "The first 1 of 2 parts are saved") {
		t.Errorf("the message should say what is saved: %q", last.Message)
	}
	p, err := st.GetPartial(context.Background(), store.URLHash(url), "m", "v")
	if err != nil || p.DoneChunks != 1 || p.TotalChunks != 2 {
		t.Fatalf("partial row: %+v, %v", p, err)
	}
	if p.CostUsd <= 0 {
		t.Errorf("the saved partial must remember what the done chunk cost: %v", p.CostUsd)
	}
	if small, err := objects.Get(context.Background(), p.AudioKey); err != nil || len(small) == 0 || len(small) >= 48000/4 {
		t.Fatalf("the saved audio should be compressed (raw would be 48000 bytes): %d bytes, %v", len(small), err)
	}

	// Sharing again: no cost prompt, only the missing chunk is made.
	before := tts.calls.Load()
	v = send(share)
	if v.Status != Done {
		t.Fatalf("second run: %s (events %+v)", v.Status, v.Events)
	}
	if made := tts.calls.Load() - before; made != 1 {
		t.Errorf("tts calls on resume = %d, want 1", made)
	}
	// The new session paid for one chunk; the shown cost adds the first run's chunk.
	sc, _ := st.SessionCost(context.Background(), v.SessionID)
	if got, want := *v.Parts[0].CostUSD, sc.TotalUsd+p.CostUsd; got < want*0.999 || got > want*1.001 || got < sc.TotalUsd*1.5 {
		t.Errorf("cost shown = %v, want this run %v + earlier %v", got, sc.TotalUsd, p.CostUsd)
	}
	if v.Parts[0].Seconds != 2 { // chunk 1 (saved) + chunk 2 (new), one second each
		t.Errorf("seconds = %d, want 2", v.Parts[0].Seconds)
	}
	if _, err := st.GetPartial(context.Background(), store.URLHash(url), "m", "v"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the saved partial should be cleared after success: %v", err)
	}
}
