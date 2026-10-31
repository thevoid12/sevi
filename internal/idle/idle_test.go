package idle

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTest() (*Manager, *clock) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	m := New(Config{
		Initial: 2 * time.Minute, BlogWindow: 6 * time.Minute, FirstAnswer: 10 * time.Minute,
		Extend: 5 * time.Minute, Cap: 90 * time.Minute, Now: c.now,
	}, zap.NewNop().Sugar())
	return m, c
}

func TestInitialWindowThenExpires(t *testing.T) {
	m, c := newTest()
	if m.Expired() {
		t.Fatal("expired at boot")
	}
	c.advance(2 * time.Minute)
	if !m.Expired() {
		t.Fatal("a boot with no job should exit after the initial window")
	}
}

func TestBlogWindowIsShortAndResetsAnswers(t *testing.T) {
	m, c := newTest()
	m.JobStarted()
	m.JobEnded(TagBlog)
	c.advance(5 * time.Minute)
	if m.Expired() {
		t.Error("blog window should be 6 min")
	}
	c.advance(1 * time.Minute)
	if !m.Expired() {
		t.Error("blog window should have closed at 6 min")
	}
}

func TestAnswerKeepsWindowOpenFiveMinutesFromNow(t *testing.T) {
	m, c := newTest()
	m.JobStarted()
	m.JobEnded(TagAnswer) // first answer: closes at minute 10
	c.advance(8 * time.Minute)
	m.JobStarted()
	m.JobEnded(TagAnswer)      // at minute 8: 8+5 = closes at minute 13
	c.advance(4 * time.Minute) // minute 12
	if m.Expired() {
		t.Error("should still be open at minute 12")
	}
	c.advance(1 * time.Minute) // minute 13
	if !m.Expired() {
		t.Error("should close at minute 13 (8+5), not later")
	}
}

func TestEarlyAnswerDoesNotChangeWindow(t *testing.T) {
	m, c := newTest()
	m.JobStarted()
	m.JobEnded(TagAnswer) // closes at minute 10
	c.advance(2 * time.Minute)
	m.JobStarted()
	m.JobEnded(TagAnswer)      // 2+5 = 7 is earlier than 10: unchanged
	c.advance(8 * time.Minute) // minute 10
	if !m.Expired() {
		t.Error("window should still close at minute 10")
	}
}

func TestNewJobAfterDeadlineIsNotCutOff(t *testing.T) {
	m, c := newTest()
	m.JobStarted()
	m.JobEnded(TagBlog)
	c.advance(10 * time.Minute) // window long over, but the process has not exited yet
	m.JobStarted()              // a new question arrives and starts a job
	if m.Expired() {
		t.Error("must not exit while a job runs")
	}
	c.advance(time.Hour)
	if m.Expired() {
		t.Error("a running job outlives any deadline")
	}
	m.JobEnded(TagAnswer)
	if m.Expired() {
		t.Error("finishing the answer opens a fresh window")
	}
}

func TestExternalBusyKeepsItUp(t *testing.T) {
	m, c := newTest()
	busy := true
	m.SetBusy(func() bool { return busy })
	c.advance(time.Hour)
	if m.Expired() {
		t.Error("expired while an update is being handled")
	}
	busy = false
	if !m.Expired() {
		t.Error("should expire once idle")
	}
}

func TestNeverExitsWhileBusy(t *testing.T) {
	m, c := newTest()
	m.JobStarted()
	c.advance(time.Hour)
	if m.Expired() {
		t.Error("expired with a job running")
	}
	m.JobEnded("") // failed job extends nothing
	if !m.Expired() {
		t.Error("should expire once the job ended and the window is over")
	}
	h := m.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		if m.Expired() {
			t.Error("expired with a request in flight")
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

func TestCap(t *testing.T) {
	m, c := newTest()
	for range 40 {
		m.JobStarted()
		m.JobEnded(TagAnswer)
	}
	c.advance(90 * time.Minute)
	if !m.Expired() {
		t.Error("window must never reach beyond the cap")
	}
}

func TestPromptAndLaterWindowsNeverShorten(t *testing.T) {
	m, c := newTest()
	m.JobStarted()
	m.JobEnded(TagAnswer) // closes at +10
	c.advance(2 * time.Minute)
	m.JobStarted()
	m.JobEnded(TagBlog)        // +6 from now = minute 8, shorter than minute 10: keep 10
	c.advance(7 * time.Minute) // minute 9
	if m.Expired() {
		t.Error("a blog must not shorten an open window")
	}
}

func TestKeepalive(t *testing.T) {
	h := KeepaliveHandler("s", nil)
	do := func(method, secret string) int {
		r := httptest.NewRequest(method, "/keepalive", nil)
		if secret != "" {
			r.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if do("POST", "s") != 204 || do("POST", "x") != 403 || do("POST", "") != 403 || do("GET", "s") != 405 {
		t.Error("keepalive checks wrong")
	}
}
