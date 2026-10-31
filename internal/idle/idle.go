// Package idle decides when the process may exit so Cloud Run can scale to
// zero, and keeps the instance up until then. Every response is tagged:
//
//	blog    audio just delivered. Nobody knows when it will be listened to, so
//	        the window is short (BlogWindow); a late reply cold-starts, which is accepted.
//	answer  a follow-up was answered. The first answer opens FirstAnswer; each
//	        further answer keeps the window open at least Extend from now (an
//	        answer at minute 8 of 10 closes it at minute 13; one at minute 2
//	        changes nothing, since 10 is already later than 7).
//	prompt  a Yes/No cost question is waiting: FirstAnswer.
//
// The window never ends more than Cap from now, and the process never exits
// while a job, an HTTP request or an incoming update is being handled, however
// late the deadline: a new question that arrives after the window would have
// closed is still served as long as it arrives before the process actually exits.
package idle

import (
	"context"
	"crypto/subtle"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Tags passed to JobEnded. Anything else (a failed or cancelled job) extends nothing.
const (
	TagBlog   = "blog"
	TagAnswer = "answer"
	TagPrompt = "prompt"
)

type Config struct {
	Initial     time.Duration // window at boot, so a request that starts no job (spam, /start) exits quickly
	BlogWindow  time.Duration
	FirstAnswer time.Duration
	Extend      time.Duration
	Cap         time.Duration               // the window never ends later than now+Cap
	Tick        time.Duration               // how often the exit check runs
	PingEvery   time.Duration               // keep-warm interval while the window is open
	Ping        func(context.Context) error // keep-warm request; nil disables it
	Now         func() time.Time            // tests only
}

// SetBusy registers an extra "work is in progress" check, for work the manager
// cannot see itself (an update being handled after its webhook was answered).
func (m *Manager) SetBusy(f func() bool) {
	m.mu.Lock()
	m.busy = f
	m.mu.Unlock()
}

type Manager struct {
	cfg Config
	log *zap.SugaredLogger

	mu       sync.Mutex
	deadline time.Time
	answers  int // answers since the last blog
	jobs     int // running jobs
	reqs     int // HTTP requests in flight
	lastPing time.Time
	busy     func() bool
}

func New(c Config, log *zap.SugaredLogger) *Manager {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Tick == 0 {
		c.Tick = 5 * time.Second
	}
	m := &Manager{cfg: c, log: log}
	m.deadline = c.Now().Add(c.Initial)
	return m
}

func (m *Manager) JobStarted() {
	m.mu.Lock()
	m.jobs++
	m.mu.Unlock()
}

// JobEnded records that a job finished and extends the window by its tag.
func (m *Manager) JobEnded(tag string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobs > 0 {
		m.jobs--
	}
	now := m.cfg.Now()
	switch tag {
	case TagBlog:
		m.answers = 0
		m.deadline = later(m.deadline, now.Add(m.cfg.BlogWindow))
	case TagAnswer:
		m.answers++
		if m.answers == 1 {
			m.deadline = later(m.deadline, now.Add(m.cfg.FirstAnswer))
		} else {
			m.deadline = later(m.deadline, now.Add(m.cfg.Extend))
		}
	case TagPrompt:
		m.deadline = later(m.deadline, now.Add(m.cfg.FirstAnswer))
	default:
		return
	}
	if limit := now.Add(m.cfg.Cap); m.deadline.After(limit) {
		m.deadline = limit
	}
	m.log.Infow("idle window", "tag", tag, "answers", m.answers, "closes_in", m.deadline.Sub(now).Round(time.Second).String())
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Wrap counts requests in flight so none is cut off by an exit.
func (m *Manager) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.reqs++
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			m.reqs--
			m.mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

// Expired reports whether the window is over and nothing is running.
func (m *Manager) Expired() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs == 0 && m.reqs == 0 && (m.busy == nil || !m.busy()) && !m.cfg.Now().Before(m.deadline)
}

// Run checks every Tick: while the window is open it sends the keep-warm ping,
// and once it is over (and nothing is running) it calls stop and returns.
func (m *Manager) Run(ctx context.Context, stop func()) {
	t := time.NewTicker(m.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if m.Expired() {
			m.log.Infow("idle window over, shutting down so the instance can scale to zero")
			stop()
			return
		}
		m.maybePing(ctx)
	}
}

func (m *Manager) maybePing(ctx context.Context) {
	if m.cfg.Ping == nil {
		return
	}
	m.mu.Lock()
	now := m.cfg.Now()
	due := now.Sub(m.lastPing) >= m.cfg.PingEvery
	if due {
		m.lastPing = now
	}
	m.mu.Unlock()
	if !due {
		return
	}
	go func() {
		if err := m.cfg.Ping(ctx); err != nil && ctx.Err() == nil {
			m.log.Warnw("keep-warm ping failed", "err", err)
		}
	}()
}

// KeepaliveHandler answers POST /keepalive: it checks the shared secret header
// in constant time, optionally runs a database ping (so Neon stays awake) and
// replies 204.
func KeepaliveHandler(secret string, dbPing func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Telegram-Bot-Api-Secret-Token")), []byte(secret)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if dbPing != nil {
			if err := dbPing(r.Context()); err != nil {
				http.Error(w, "database unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
