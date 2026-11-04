package core

import (
	"context"
	"os/exec"
	"runtime"
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

// bigTTS returns a lot of audio per chunk, so a leak would show.
type bigTTS struct{}

func (bigTTS) Synth(context.Context, string) (provider.Speech, error) {
	return provider.Speech{PCM: make([]byte, 2_000_000), GenerationID: "gen"}, nil
}

func heapMB() float64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / 1e6
}

// After a blog is finished its audio must not stay in memory: the next blog
// should start from the same footprint.
func TestAudioMemoryIsReleasedAfterABlog(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	log := zap.NewNop().Sugar()
	st := storetest.Open(t, log)
	cfg := viper.New()
	cfg.Set("models.tts.provider", "p")
	cfg.Set("models.tts.model", "m")
	cfg.Set("models.tts.voice", "v")
	cfg.Set("models.tts.chunk_chars", 800)
	cfg.Set("models.tts.concurrency", 3)
	cfg.Set("models.llm.provider", "p")
	cfg.Set("models.llm.model", "llm-m")
	cfg.Set("cost.confirm_over_usd", 1000.0)
	prices := &pricing.Book{Store: st, TTL: time.Hour, Log: log,
		Sources: map[string]pricing.Source{"p": func(context.Context) (map[string]provider.Price, error) {
			return map[string]provider.Price{"m": {Prompt: 1e-6, Completion: 1e-4}, "llm-m": {}}, nil
		}}}
	c := New(Deps{
		Service: &model.Service{Cfg: cfg, Store: st, Objects: objectstore.Local{Root: t.TempDir() + "/objects"}, Log: log},
		TTS:     bigTTS{}, LLM: fakeLLM{}, Prices: prices,
		Markdown: func(context.Context, string) (string, error) { return page("Big", 16000), nil }, // ~20 chunks, ~40 MB of audio
	})
	t.Cleanup(c.Close)

	run := func(url string) {
		j, err := c.Handle(reqid.With(context.Background(), "ref-mem"), Message{Parts: []Part{{Type: "url", URL: url}}})
		if err != nil {
			t.Fatal(err)
		}
		if v := waitDone(t, j); v.Status != Done {
			t.Fatalf("job: %s %+v", v.Status, v.Events)
		}
	}
	run("https://blog.example/warmup") // first run allocates one-off things (caches, pools)
	base := heapMB()
	var peak float64
	stop := make(chan struct{})
	go func() { // sample the heap while the job runs
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				runtime.ReadMemStats(&m)
				if mb := float64(m.HeapAlloc) / 1e6; mb > peak {
					peak = mb
				}
			}
		}
	}()
	run("https://blog.example/measured")
	close(stop)
	after := heapMB()
	t.Logf("heap: before %.0f MB, peak during the job %.0f MB, after %.0f MB (audio of the job: ~40 MB)", base, peak, after)
	if after > base+15 {
		t.Errorf("memory was not released: %.0f MB before, %.0f MB after the job", base, after)
	}
	if peak < base+30 {
		t.Logf("note: the sampler saw a lower peak than expected (%.0f MB)", peak)
	}
}
