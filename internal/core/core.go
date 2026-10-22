// Package core is the heart of sevi: it turns messages (a blog link, a
// confirmation) into jobs that produce audio, records every provider call in
// the ledger, and reports progress as user-facing events. It knows nothing
// about Telegram, Discord or HTTP.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"sevi/internal/blob"
	"sevi/internal/config"
	"sevi/internal/event"
	"sevi/internal/logx"
	"sevi/internal/pricing"
	"sevi/internal/provider"
	"sevi/internal/reqid"
	"sevi/internal/store"
)

// Deps are everything Core needs from outside, so tests can fake the network.
type Deps struct {
	Cfg      *config.Config
	Store    *store.Store
	Blob     blob.Store
	Log      *zap.SugaredLogger
	TTS      provider.TTS
	LLM      provider.LLM                                              // answers follow-ups; hears audio itself unless STT is set
	STT      provider.LLM                                              // optional: when set, voice notes are transcribed by this first
	Markdown func(ctx context.Context, pageURL string) (string, error) // page as markdown
	Prices   *pricing.Book
}

type Core struct {
	Deps
	ctx    context.Context // cancelled by Close; jobs run under it
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu   sync.Mutex
	jobs map[string]*Job

	// blogLocks makes the same blog (link + speech model + voice) be spoken once
	// at a time: a second request for it waits, then finds the audio ready.
	blogLocks keyedLock
	// sessionLocks keeps two questions in one conversation from summarising it at once.
	sessionLocks keyedLock
}

func New(d Deps) *Core {
	ctx, cancel := context.WithCancel(context.Background())
	return &Core{Deps: d, ctx: ctx, cancel: cancel, jobs: map[string]*Job{}, blogLocks: keyedLock{held: map[string]chan struct{}{}}, sessionLocks: keyedLock{held: map[string]chan struct{}{}}}
}

// Close cancels running jobs and waits briefly for them to stop.
func (c *Core) Close() {
	c.cancel()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		c.Log.Warnw("jobs still running at shutdown")
	}
}

// Message is the platform-neutral input every adapter sends.
//
// Adapter, ThreadRef and ReplyToRef give threading without Core knowing the
// platform: ThreadRef is the adapter's handle for this very message,
// ReplyToRef its handle for the message this one replies to (both only
// meaningful together with Adapter). SessionID continues a session explicitly
// (e.g. from a web UI) and takes priority over ReplyToRef.
type Message struct {
	User       string `json:"user"`
	Adapter    string `json:"adapter"`
	ThreadRef  string `json:"thread_ref"`
	ReplyToRef string `json:"reply_to_ref"`
	SessionID  string `json:"session_id"`
	Parts      []Part `json:"parts"`
}

// Handle routes a message and starts a job for it. Input mistakes come back as
// *event.UserError (the caller shows the message); the job itself reports
// progress and failure through its events.
func (c *Core) Handle(ctx context.Context, m Message) (*Job, error) {
	log := logx.With(ctx, c.Log)
	jobCtx := reqid.With(c.ctx, reqid.From(ctx))

	var pageURL, text string
	var confirm, voice *Part
	for i, p := range m.Parts {
		switch p.Type {
		case "confirm":
			confirm = &m.Parts[i]
		case "url":
			pageURL = strings.TrimSpace(p.URL)
		case "audio":
			voice = &m.Parts[i]
		case "text":
			t := strings.TrimSpace(p.Text)
			if strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://") {
				pageURL = t
			} else {
				text = t
			}
		}
	}

	switch {
	case confirm != nil:
		return c.handleConfirm(ctx, jobCtx, m.SessionID, confirm.Decision)
	case pageURL != "":
		u, err := url.Parse(pageURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, event.Wrap("That doesn't look like a valid web link.", fmt.Errorf("bad url %q", pageURL))
		}
		sess, err := c.Store.CreateSession(ctx, "blog", pageURL)
		if err != nil {
			return nil, event.Wrap("Something went wrong on my side.", err)
		}
		log.Infow("blog request", "session", sess.ID, "url", pageURL, "user", m.User, "thread_ref", m.ThreadRef)
		return c.start(jobCtx, sess.ID, func(ctx context.Context, j *Job) error {
			return c.blogJob(ctx, j, sess.ID, pageURL, false)
		}), nil
	case voice != nil || text != "":
		return c.handleTurn(ctx, jobCtx, m, text, voice)
	default:
		return nil, event.Wrap("Send me a blog link, or ask a question by voice or text.", errors.New("no supported part in message"))
	}
}

func (c *Core) handleConfirm(ctx, jobCtx context.Context, sessionID, decision string) (*Job, error) {
	if sessionID == "" {
		return nil, event.Wrap("Tell me which request you are answering.", errors.New("confirm without session_id"))
	}
	sess, err := c.Store.GetSession(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, event.Wrap("I don't know that request.", err)
	} else if err != nil {
		return nil, event.Wrap("Something went wrong on my side.", err)
	}
	pend, err := c.Store.OpenPending(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, event.Wrap("Nothing is waiting for an answer.", err)
	} else if err != nil {
		return nil, event.Wrap("Something went wrong on my side.", err)
	}
	switch strings.ToLower(decision) {
	case "yes":
		if err := c.Store.ResolvePending(ctx, pend.ID, "confirmed"); err != nil {
			return nil, event.Wrap("Something went wrong on my side.", err)
		}
		return c.start(jobCtx, sessionID, func(ctx context.Context, j *Job) error {
			return c.blogJob(ctx, j, sessionID, sess.Url.String, true)
		}), nil
	case "no":
		if err := c.Store.ResolvePending(ctx, pend.ID, "cancelled"); err != nil {
			return nil, event.Wrap("Something went wrong on my side.", err)
		}
		return c.start(jobCtx, sessionID, func(ctx context.Context, j *Job) error {
			j.emit(event.Event{Kind: event.Notice, Message: "Okay, cancelled. Nothing was spent."})
			j.setStatus(Done, nil)
			return nil
		}), nil
	default:
		return nil, event.Wrap(`Answer "yes" or "no".`, fmt.Errorf("bad decision %q", decision))
	}
}

// start registers a job and runs fn in the background. A returned error is
// logged in full (with the request id) and shown to the user as its safe
// message plus a ref.
func (c *Core) start(ctx context.Context, sessionID string, fn func(context.Context, *Job) error) *Job {
	j := newJob(uuid.NewString(), sessionID)
	c.mu.Lock()
	for id, old := range c.jobs {
		if old.prunable(time.Hour) {
			delete(c.jobs, id)
		}
	}
	c.jobs[j.ID] = j
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		log := logx.With(ctx, c.Log).With("session", sessionID, "job", j.ID)
		if err := fn(ctx, j); err != nil {
			log.Errorw("job failed", "err", err)
			j.emit(event.Event{Kind: event.Error, Message: event.SafeMessage(err), Ref: reqid.From(ctx)})
			j.setStatus(Failed, nil)
		}
	}()
	return j
}

// Job returns a job by id, or nil.
func (c *Core) Job(id string) *Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.jobs[id]
}

type pendingConfirm struct {
	Chars   int     `json:"chars"`
	USD     float64 `json:"usd"`
	Minutes float64 `json:"minutes"`
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// keyedLock is a mutex per key that gives up when the context ends.
type keyedLock struct {
	mu   sync.Mutex
	held map[string]chan struct{} // closed when the holder releases
}

// acquire waits for the key to be free and takes it. onWait, if set, is called
// once, only when the key was already held. The returned func releases it.
func (k *keyedLock) acquire(ctx context.Context, key string, onWait func()) (release func(), err error) {
	for {
		k.mu.Lock()
		busy, taken := k.held[key]
		if !taken {
			done := make(chan struct{})
			k.held[key] = done
			k.mu.Unlock()
			return func() {
				k.mu.Lock()
				delete(k.held, key)
				k.mu.Unlock()
				close(done)
			}, nil
		}
		k.mu.Unlock()
		if onWait != nil {
			onWait()
			onWait = nil
		}
		select {
		case <-busy:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
