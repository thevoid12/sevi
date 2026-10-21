package core

import (
	"sync"
	"time"

	"sevi/internal/event"
)

type Status string

const (
	Running           Status = "running"
	Done              Status = "done"
	Failed            Status = "failed"
	NeedsConfirmation Status = "needs_confirmation"
)

// Part is one piece of input or output. Input types: url, confirm.
// Output types: audio, text.
type Part struct {
	Type      string `json:"type"`
	URL       string `json:"url,omitempty"`
	Text      string `json:"text,omitempty"`
	Decision  string `json:"decision,omitempty"` // confirm: yes | no
	MessageID string `json:"message_id,omitempty"`
	Seconds   int    `json:"seconds,omitempty"`
}

// Job is one unit of work started by a message. Adapters follow it by polling
// View with the last seq they saw.
type Job struct {
	ID        string
	SessionID string

	mu          sync.Mutex
	status      Status
	events      []event.Event
	parts       []Part
	costPending bool
	finishedAt  time.Time
	changed     chan struct{} // closed and replaced on every change
}

func newJob(id, sessionID string) *Job {
	return &Job{ID: id, SessionID: sessionID, status: Running, changed: make(chan struct{})}
}

// JobView is a consistent snapshot of a job.
type JobView struct {
	ID          string        `json:"job_id"`
	SessionID   string        `json:"session_id"`
	Status      Status        `json:"status"`
	Events      []event.Event `json:"events"`
	Parts       []Part        `json:"parts,omitempty"`
	CostPending bool          `json:"cost_pending"`
}

// View returns the job state with only the events whose seq is > after.
func (j *Job) View(after int) JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := JobView{ID: j.ID, SessionID: j.SessionID, Status: j.status, Parts: j.parts, CostPending: j.costPending, Events: []event.Event{}}
	for _, e := range j.events {
		if e.Seq > after {
			v.Events = append(v.Events, e)
		}
	}
	return v
}

// Changed returns a channel that is closed on the next change to the job.
func (j *Job) Changed() <-chan struct{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.changed
}

func (j *Job) Terminal() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status != Running
}

// locked: caller holds j.mu.
func (j *Job) notify() {
	close(j.changed)
	j.changed = make(chan struct{})
}

func (j *Job) emit(e event.Event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e.Seq = len(j.events) + 1
	j.events = append(j.events, e)
	j.notify()
}

func (j *Job) progress(msg string, step, total int) {
	j.emit(event.Event{Kind: event.Progress, Message: msg, Step: step, Total: total})
}

func (j *Job) setStatus(s Status, parts []Part) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = s
	if parts != nil {
		j.parts = parts
	}
	j.finishedAt = time.Now()
	j.notify()
}

func (j *Job) setCostPending(v bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.costPending = v
	if !v {
		j.finishedAt = time.Now()
	}
	j.notify()
}

// prunable: finished long ago and nothing more will be added.
func (j *Job) prunable(maxAge time.Duration) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status != Running && !j.costPending && time.Since(j.finishedAt) > maxAge
}
