// Package api exposes Core over HTTP on localhost: the contract every adapter
// (Telegram, Discord, web) uses. Handlers translate; they hold no logic.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"sevi/internal/blob"
	"sevi/internal/core"
	"sevi/internal/event"
	"sevi/internal/logx"
	"sevi/internal/reqid"
	"sevi/internal/store"
)

type API struct {
	core  *core.Core
	store *store.Store
	blob  blob.Store
	log   *zap.SugaredLogger
}

func New(c *core.Core, st *store.Store, bl blob.Store, log *zap.SugaredLogger) *API {
	return &API{core: c, store: st, blob: bl, log: log}
}

// Handler returns the routes wrapped in request-id and access-log middleware.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", a.postMessage)
	mux.HandleFunc("GET /v1/jobs/{id}", a.getJob)
	mux.HandleFunc("GET /v1/sessions/{id}", a.getSession)
	mux.HandleFunc("GET /v1/sessions/{id}/cost", a.getSessionCost)
	mux.HandleFunc("GET /v1/audio/{message_id}", a.getAudio)
	return a.middleware(mux)
}

// maxWait caps how long a request may block waiting for a job.
const maxWait = 10 * time.Minute

// POST /v1/messages        -> 202 {job_id, session_id, status, events...}
// POST /v1/messages?wait=1 -> blocks until the job stops running (done, failed,
// or needs_confirmation) and returns the same shape with the result.
func (a *API) postMessage(w http.ResponseWriter, r *http.Request) {
	var m core.Message
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&m); err != nil {
		a.fail(w, r, http.StatusBadRequest, event.Wrap("The request body is not valid JSON.", err))
		return
	}
	job, err := a.core.Handle(r.Context(), m)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if r.URL.Query().Get("wait") != "" {
		a.waitFor(r, job, maxWait)
		a.writeJSON(w, http.StatusOK, job.View(0))
		return
	}
	a.writeJSON(w, http.StatusAccepted, job.View(0))
}

// GET /v1/jobs/{id}?after=<seq>&wait=<seconds>
// Returns events with seq > after. With wait, blocks until something new
// arrives or the time is up (long polling).
func (a *API) getJob(w http.ResponseWriter, r *http.Request) {
	job := a.core.Job(r.PathValue("id"))
	if job == nil {
		a.fail(w, r, http.StatusNotFound, event.Wrap("I don't know that job.", errors.New("job not found")))
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	if secs, _ := strconv.Atoi(r.URL.Query().Get("wait")); secs > 0 {
		a.waitForChange(r, job, after, time.Duration(secs)*time.Second)
	}
	a.writeJSON(w, http.StatusOK, job.View(after))
}

// waitFor blocks until the job leaves Running or the time/client runs out.
func (a *API) waitFor(r *http.Request, job *core.Job, max time.Duration) {
	timeout := time.After(max)
	for {
		ch := job.Changed() // taken before the check so a change in between is not missed
		if job.Terminal() {
			return
		}
		select {
		case <-ch:
		case <-timeout:
			return
		case <-r.Context().Done():
			return
		}
	}
}

// waitForChange blocks until the job has events after seq, is completely
// finished (including its cost lookup), or time is up.
func (a *API) waitForChange(r *http.Request, job *core.Job, after int, max time.Duration) {
	timeout := time.After(max)
	for {
		ch := job.Changed()
		if v := job.View(after); len(v.Events) > 0 || (v.Status != core.Running && !v.CostPending) {
			return
		}
		select {
		case <-ch:
		case <-timeout:
			return
		case <-r.Context().Done():
			return
		}
	}
}

type sessionView struct {
	Session  store.Session   `json:"session"`
	Messages []store.Message `json:"messages"`
	Cost     costView        `json:"cost"`
}

type costView struct {
	TotalUSD     float64           `json:"total_usd"`
	Calls        int64             `json:"calls"`
	UnknownCalls int64             `json:"unknown_calls"`
	Ledger       []store.LedgerRow `json:"ledger,omitempty"`
}

func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess, err := a.store.GetSession(ctx, r.PathValue("id"))
	if err != nil {
		a.storeFail(w, r, err, "I don't know that session.")
		return
	}
	msgs, err := a.store.ListMessages(ctx, sess.ID)
	if err != nil {
		a.storeFail(w, r, err, "")
		return
	}
	cv, err := a.cost(r, sess.ID, false)
	if err != nil {
		a.storeFail(w, r, err, "")
		return
	}
	a.writeJSON(w, http.StatusOK, sessionView{Session: sess, Messages: msgs, Cost: cv})
}

func (a *API) getSessionCost(w http.ResponseWriter, r *http.Request) {
	sess, err := a.store.GetSession(r.Context(), r.PathValue("id"))
	if err != nil {
		a.storeFail(w, r, err, "I don't know that session.")
		return
	}
	cv, err := a.cost(r, sess.ID, true)
	if err != nil {
		a.storeFail(w, r, err, "")
		return
	}
	a.writeJSON(w, http.StatusOK, cv)
}

func (a *API) cost(r *http.Request, sessionID string, withLedger bool) (costView, error) {
	sc, err := a.store.SessionCost(r.Context(), sessionID)
	if err != nil {
		return costView{}, err
	}
	cv := costView{TotalUSD: sc.TotalUsd, Calls: sc.Calls, UnknownCalls: sc.UnknownCalls}
	if withLedger {
		if cv.Ledger, err = a.store.ListLedger(r.Context(), sessionID); err != nil {
			return costView{}, err
		}
	}
	return cv, nil
}

// GET /v1/audio/{message_id} streams the MP3 (supports range requests for seeking).
func (a *API) getAudio(w http.ResponseWriter, r *http.Request) {
	msg, err := a.store.GetMessage(r.Context(), r.PathValue("message_id"))
	if err != nil || !msg.AudioKey.Valid {
		a.fail(w, r, http.StatusNotFound, event.Wrap("There is no audio for that message.", err))
		return
	}
	b, err := a.blob.Get(r.Context(), msg.AudioKey.String)
	if err != nil {
		a.fail(w, r, http.StatusNotFound, event.Wrap("That audio file is no longer available.", err))
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	http.ServeContent(w, r, "audio.mp3", time.Unix(msg.CreatedOn, 0), bytes.NewReader(b))
}

type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Ref     string `json:"ref"`
	} `json:"error"`
}

// fail logs the full cause and sends only the safe message plus the request id.
func (a *API) fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	log := logx.With(r.Context(), a.log)
	if status >= 500 {
		log.Errorw("request failed", "status", status, "err", err)
	} else {
		log.Warnw("request rejected", "status", status, "err", err)
	}
	var b errorBody
	b.Error.Message = event.SafeMessage(err)
	b.Error.Ref = reqid.From(r.Context())
	a.writeJSON(w, status, b)
}

func (a *API) storeFail(w http.ResponseWriter, r *http.Request, err error, notFoundMsg string) {
	if errors.Is(err, store.ErrNotFound) {
		a.fail(w, r, http.StatusNotFound, event.Wrap(notFoundMsg, err))
		return
	}
	a.fail(w, r, http.StatusInternalServerError, event.Wrap("Something went wrong on my side.", err))
}

func (a *API) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		a.log.Warnw("write response failed", "err", err)
	}
}
