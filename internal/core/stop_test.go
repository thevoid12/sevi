package core

import (
	"context"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	"sevi/internal/model"
	"sevi/internal/objectstore"
	"sevi/internal/pricing"
	"sevi/internal/provider"
	"sevi/internal/reqid"
	"sevi/internal/store/storetest"
)

// hangingTTS makes the first chunk, then blocks on the second until it is cancelled.
type hangingTTS struct {
	calls   atomic.Int32
	reached chan struct{} // closed when the second chunk starts
}

func (h *hangingTTS) Synth(ctx context.Context, _ string) (provider.Speech, error) {
	if h.calls.Add(1) == 1 {
		return provider.Speech{PCM: make([]byte, 48000), GenerationID: "gen"}, nil
	}
	close(h.reached)
	<-ctx.Done()
	return provider.Speech{}, ctx.Err()
}

// Stopping a blog part way sends the audio made so far, marked as stopped, and
// keeps it so sharing the link again continues from there.
func TestStopSendsWhatIsMadeSoFar(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	log := zap.NewNop().Sugar()
	st := storetest.Open(t, log)
	cfg := viper.New()
	cfg.Set("models.tts.provider", "p")
	cfg.Set("models.tts.model", "m")
	cfg.Set("models.tts.voice", "v")
	cfg.Set("models.tts.chunk_chars", 3000)
	cfg.Set("models.llm.provider", "p")
	cfg.Set("models.llm.model", "llm-m")
	cfg.Set("cost.confirm_over_usd", 100.0)
	tts := &hangingTTS{reached: make(chan struct{})}
	prices := &pricing.Book{Store: st, TTL: time.Hour, Log: log,
		Sources: map[string]pricing.Source{"p": func(context.Context) (map[string]provider.Price, error) {
			return map[string]provider.Price{"m": {Prompt: 1e-6, Completion: 1e-4}, "llm-m": {Prompt: 1e-7, Completion: 4e-7}}, nil
		}}}
	c := New(Deps{
		Service: &model.Service{Cfg: cfg, Store: st, Objects: objectstore.Local{Root: t.TempDir() + "/objects"}, Log: log},
		TTS:     tts, LLM: fakeLLM{}, Prices: prices,
		Markdown: func(context.Context, string) (string, error) { return page("Stop Me", 5700), nil },
	})
	t.Cleanup(c.Close)

	j, err := c.Handle(reqid.With(context.Background(), "ref-stop"), Message{Parts: []Part{{Type: "url", URL: "https://blog.example/stop"}}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-tts.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the second chunk never started")
	}
	if !c.StopJob(j.ID) {
		t.Fatal("StopJob: the job was running")
	}
	v := waitDone(t, j)
	if v.Status != Done || len(v.Parts) != 1 || v.Parts[0].Type != "audio" {
		t.Fatalf("want Done with the partial audio, got %s %+v (events %+v)", v.Status, v.Parts, v.Events)
	}
	if n := v.Parts[0].Note; !strings.HasPrefix(n, "Stopped.") || !strings.Contains(n, "first 5") || !strings.Contains(n, "%") {
		t.Errorf("note: %q", n)
	}
	if c.StopJob(j.ID) {
		t.Error("a finished job cannot be stopped again")
	}
	if c.StopJob("nope") {
		t.Error("an unknown job cannot be stopped")
	}
}

// Stopping before any audio exists ends the job cleanly, not as a failure.
func TestStopBeforeAnyAudioIsNotAFailure(t *testing.T) {
	log := zap.NewNop().Sugar()
	st := storetest.Open(t, log)
	cfg := viper.New()
	cfg.Set("models.tts.provider", "p")
	cfg.Set("models.tts.model", "m")
	cfg.Set("models.tts.voice", "v")
	cfg.Set("models.tts.chunk_chars", 3000)
	cfg.Set("models.llm.provider", "p")
	cfg.Set("models.llm.model", "llm-m")
	cfg.Set("cost.confirm_over_usd", 100.0)
	started := make(chan struct{})
	c := New(Deps{
		Service: &model.Service{Cfg: cfg, Store: st, Objects: objectstore.Local{Root: t.TempDir() + "/objects"}, Log: log},
		TTS:     &fakeTTS{}, LLM: fakeLLM{},
		Prices: &pricing.Book{Store: st, TTL: time.Hour, Log: log, Sources: map[string]pricing.Source{"p": func(context.Context) (map[string]provider.Price, error) {
			return map[string]provider.Price{"m": {Prompt: 1e-6, Completion: 1e-4}, "llm-m": {}}, nil
		}}},
		Markdown: func(ctx context.Context, _ string) (string, error) {
			close(started)
			<-ctx.Done() // the page fetch is slow; the user stops here
			return "", ctx.Err()
		},
	})
	t.Cleanup(c.Close)
	j, err := c.Handle(reqid.With(context.Background(), "ref-stop2"), Message{Parts: []Part{{Type: "url", URL: "https://blog.example/slow"}}})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	c.StopJob(j.ID)
	if v := waitDone(t, j); v.Status != Done || len(v.Parts) != 0 {
		t.Fatalf("want Done with nothing, got %s %+v", v.Status, v.Events)
	}
}
