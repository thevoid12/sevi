package core

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"sevi/internal/event"
)

// Each job runs in its own goroutine, so several blogs shared at once are
// spoken side by side, not one after another. (Encoding the MP3 with ffmpeg
// comes after the speech and is not part of what this measures.)
func TestSeveralBlogsAreSpokenInParallel(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	e.tts.delay = 300 * time.Millisecond // 2 chunks per blog = 0.6 s of speech each
	ctx := context.Background()

	start := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var spoken []time.Duration // when each blog finished being spoken
	var jobs []*Job
	for _, u := range []string{"https://blog.example/one", "https://blog.example/two", "https://blog.example/three"} {
		j, err := e.core.Handle(ctx, link(u))
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
		wg.Add(1)
		go func() {
			defer wg.Done()
			seen, done := 0, false
			for !done {
				ch := j.Changed()
				v := j.View(seen)
				for _, ev := range v.Events {
					seen = ev.Seq
					if strings.HasPrefix(ev.Message, "Putting the audio together") {
						mu.Lock()
						spoken = append(spoken, time.Since(start))
						mu.Unlock()
					}
				}
				if v.Status != Running {
					done = true
				} else {
					<-ch
				}
			}
		}()
	}
	wg.Wait()
	for _, j := range jobs {
		if v := j.View(0); v.Status != Done {
			t.Fatalf("job ended %s: %+v", v.Status, v.Events)
		}
	}
	if len(spoken) != 3 {
		t.Fatalf("saw %d blogs finish speaking, want 3", len(spoken))
	}
	var last time.Duration
	for _, d := range spoken {
		last = max(last, d)
	}
	// Serial speech would take 3 x 0.6 = 1.8 s before the last blog is spoken.
	if last > 1000*time.Millisecond {
		t.Errorf("the last blog finished speaking after %v: they look serialised", last)
	}
	if got := e.tts.calls.Load(); got != 6 {
		t.Errorf("tts calls = %d, want 6 (2 chunks x 3 blogs)", got)
	}
	t.Logf("3 blogs finished speaking after %v (serial would be about 1.8 s)", last.Round(10*time.Millisecond))
}

// Regression: the same link sent twice before the first finished was spoken and
// billed twice. The second request must wait, say so, and then reuse the audio.
func TestSameLinkTwiceAtOnceIsSpokenOnce(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	e.tts.delay = 200 * time.Millisecond
	ctx := context.Background()
	j1, err := e.core.Handle(ctx, link("https://blog.example/same"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the first take the lock
	j2, err := e.core.Handle(ctx, link("https://blog.example/same"))
	if err != nil {
		t.Fatal(err)
	}
	v1, v2 := waitJob(t, j1, false), waitJob(t, j2, false)
	if v1.Status != Done || v2.Status != Done {
		t.Fatalf("both should finish: %s %s", v1.Status, v2.Status)
	}
	if got := e.tts.calls.Load(); got != 2 {
		t.Errorf("speech calls = %d, want 2 (one blog's worth, not two)", got)
	}
	var waited, free bool
	for _, ev := range v2.Events {
		waited = waited || (ev.Kind == event.Progress && strings.Contains(ev.Message, "already being made"))
		free = free || strings.Contains(ev.Message, "No cost")
	}
	if !waited || !free {
		t.Errorf("the second request should say it waited and then cost nothing: %+v", v2.Events)
	}
	sc, _ := e.st.SessionCost(ctx, v2.SessionID)
	if sc.Calls != 0 {
		t.Errorf("the waiting request must not be billed: %+v", sc)
	}
}

// A different voice is a different file, so it is not blocked by the first.
func TestDifferentBlogsDoNotBlockEachOther(t *testing.T) {
	e := newEnv(t, 100, 0.014)
	e.tts.delay = 200 * time.Millisecond
	ctx := context.Background()
	start := time.Now()
	j1, _ := e.core.Handle(ctx, link("https://blog.example/a"))
	j2, _ := e.core.Handle(ctx, link("https://blog.example/b"))
	waitJob(t, j1, false)
	waitJob(t, j2, false)
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Errorf("two different blogs took %v", took)
	}
}
