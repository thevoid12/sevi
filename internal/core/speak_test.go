package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"

	"sevi/internal/httpx"
	"sevi/internal/model"
	"sevi/internal/objectstore"
	"sevi/internal/pricing"
	"sevi/internal/provider"
	"sevi/internal/reqid"
	"sevi/internal/store/storetest"
)

// gateTTS records how many calls run at once and fails chunks that contain "BAD".
type gateTTS struct {
	delay    time.Duration
	inFlight atomic.Int32
	maxSeen  atomic.Int32
	mu       sync.Mutex
	started  []string
}

func (g *gateTTS) Synth(ctx context.Context, text string) (provider.Speech, error) {
	n := g.inFlight.Add(1)
	defer g.inFlight.Add(-1)
	for {
		m := g.maxSeen.Load()
		if n <= m || g.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	g.mu.Lock()
	g.started = append(g.started, text)
	g.mu.Unlock()
	select {
	case <-time.After(g.delay):
	case <-ctx.Done():
		return provider.Speech{}, ctx.Err()
	}
	if strings.Contains(text, "BAD") {
		return provider.Speech{}, errors.New("provider failed")
	}
	return provider.Speech{PCM: make([]byte, 4800), GenerationID: "gen"}, nil
}

func speakCore(t *testing.T, tts provider.TTS, slots int) *Core {
	t.Helper()
	log := zap.NewNop().Sugar()
	st := storetest.Open(t, log)
	cfg := viper.New()
	cfg.Set("models.tts.provider", "p")
	cfg.Set("models.tts.model", "m")
	cfg.Set("models.tts.concurrency", slots)
	prices := &pricing.Book{Store: st, TTL: time.Hour, Log: log,
		Sources: map[string]pricing.Source{"p": func(context.Context) (map[string]provider.Price, error) {
			return map[string]provider.Price{"m": {Prompt: 1e-6, Completion: 1e-4}}, nil
		}}}
	c := New(Deps{Service: &model.Service{Cfg: cfg, Store: st, Objects: objectstore.Local{Root: t.TempDir()}, Log: log}, TTS: tts, Prices: prices})
	t.Cleanup(c.Close)
	return c
}

func spansOf(n int, bad ...int) []model.Span {
	var out []model.Span
	for i := 0; i < n; i++ {
		text := fmt.Sprintf("Chunk number %d.", i)
		for _, b := range bad {
			if b == i {
				text += " BAD"
			}
		}
		out = append(out, model.Span{Start: i * 100, End: i*100 + 100, Text: text})
	}
	return out
}

func newTestJob(c *Core, t *testing.T) (*Job, string) {
	t.Helper()
	sess, err := c.Store.CreateSession(context.Background(), "blog", "https://x.example/"+t.Name())
	if err != nil {
		t.Fatal(err)
	}
	return newJob("job-"+t.Name(), sess.ID), sess.ID
}

func TestSpeakRunsChunksInParallelUpToTheLimit(t *testing.T) {
	tts := &gateTTS{delay: 40 * time.Millisecond}
	c := speakCore(t, tts, 3)
	j, sid := newTestJob(c, t)
	segs, cost, _, fail := c.speak(reqid.With(context.Background(), "r"), j, sid, spansOf(9))
	if fail != nil || len(segs) != 9 || cost <= 0 {
		t.Fatalf("fail=%v segs=%d cost=%v", fail, len(segs), cost)
	}
	if got := tts.maxSeen.Load(); got != 3 {
		t.Errorf("most calls at once = %d, want exactly 3", got)
	}
	// The result of each chunk carries its own position, whatever order they finished in.
	seen := map[int]bool{}
	for _, s := range segs {
		seen[s.Start/100] = true
	}
	if len(seen) != 9 {
		t.Errorf("positions: %v", seen)
	}
	if _, covered := prefixSegs(segs); covered != 900 {
		t.Errorf("the segments should tile the text: covered %d", covered)
	}
}

func TestSpeakLimitIsSharedByJobs(t *testing.T) {
	tts := &gateTTS{delay: 30 * time.Millisecond}
	c := speakCore(t, tts, 3)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess, _ := c.Store.CreateSession(context.Background(), "blog", fmt.Sprintf("https://x.example/%d", i))
			j := newJob(fmt.Sprint("job", i), sess.ID)
			segs, _, _, fail := c.speak(context.Background(), j, sess.ID, spansOf(6))
			if fail != nil || len(segs) != 6 {
				t.Errorf("job %d: fail=%v segs=%d", i, fail, len(segs))
			}
		}()
	}
	wg.Wait()
	if got := tts.maxSeen.Load(); got > 3 {
		t.Errorf("two jobs made %d calls at once, the shared limit is 3", got)
	}
}

func TestSpeakKeepsWhatFinishedWhenAChunkFails(t *testing.T) {
	tts := &gateTTS{delay: 20 * time.Millisecond}
	c := speakCore(t, tts, 2)
	j, sid := newTestJob(c, t)
	segs, _, _, fail := c.speak(context.Background(), j, sid, spansOf(12, 3))
	if fail == nil || fail.part != 4 {
		t.Fatalf("want a failure at part 4, got %+v", fail)
	}
	// Nothing new is started after the failure: at most the calls already running finish.
	if len(segs) >= 11 {
		t.Errorf("kept going after the failure: %d chunks made", len(segs))
	}
	if len(segs) < 3 {
		t.Errorf("chunks before the failure should be kept: %d", len(segs))
	}
	for _, s := range segs {
		if s.Start == 300 {
			t.Error("the failed chunk must not be in the result")
		}
	}
	if len(tts.started) > 3+2+1 {
		t.Errorf("%d chunks were started; the failure should have stopped new ones", len(tts.started))
	}
}

func TestSpeakStopCutsRunningCallsAndKeepsFinished(t *testing.T) {
	tts := &gateTTS{delay: 200 * time.Millisecond}
	c := speakCore(t, tts, 3)
	j, sid := newTestJob(c, t)
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(260 * time.Millisecond) // the first three are done, the next three are running
		cancel(ErrStopped)
	}()
	start := time.Now()
	segs, _, _, fail := c.speak(ctx, j, sid, spansOf(30))
	if fail == nil || !errors.Is(fail.err, context.Canceled) {
		t.Fatalf("want a cancelled failure, got %+v", fail)
	}
	if len(segs) != 3 {
		t.Errorf("finished chunks kept: %d, want 3", len(segs))
	}
	if time.Since(start) > 800*time.Millisecond {
		t.Errorf("stop took %v: running calls should be cut short", time.Since(start))
	}
}

func TestSpeakGoesOneAtATimeAfterARateLimit(t *testing.T) {
	var inFlight, maxAfter atomic.Int32
	var limited atomic.Bool
	tts := ttsFunc(func(ctx context.Context, text string) (provider.Speech, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		if limited.Load() && n > maxAfter.Load() {
			maxAfter.Store(n)
		}
		time.Sleep(20 * time.Millisecond)
		if strings.Contains(text, "number 1.") && !limited.Swap(true) {
			return provider.Speech{}, &httpx.StatusError{Host: "tts", Code: 429, Status: "429 Too Many Requests"}
		}
		return provider.Speech{PCM: make([]byte, 4800)}, nil
	})
	c := speakCore(t, tts, 3)
	j, sid := newTestJob(c, t)
	segs, _, _, fail := c.speak(context.Background(), j, sid, spansOf(10))
	if fail == nil {
		t.Fatalf("the rate-limited chunk should have failed: %d chunks made", len(segs))
	}
	if maxAfter.Load() > 2 { // calls already running when it was limited may overlap with one more
		t.Errorf("after the rate limit %d calls ran at once", maxAfter.Load())
	}
}

type ttsFunc func(context.Context, string) (provider.Speech, error)

func (f ttsFunc) Synth(ctx context.Context, text string) (provider.Speech, error) {
	return f(ctx, text)
}
