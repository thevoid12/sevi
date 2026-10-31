// Package api exposes Core over HTTP on localhost: the contract every adapter
// (Telegram, Discord, web) uses. Handlers translate; they hold no logic.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sevi/internal/auth"
	"sevi/internal/core"
	"sevi/internal/event"
	"sevi/internal/logx"
	"sevi/internal/model"
	"sevi/internal/reqid"
	"sevi/internal/store"
)

// audioURLTTL is how long a presigned audio link stays valid. Links are made
// fresh on every read, so this only has to outlast one listening session.
const audioURLTTL = 12 * time.Hour

type API struct {
	*model.Service
	core     *core.Core
	verifier *auth.Verifier
}

func New(svc *model.Service, c *core.Core, v *auth.Verifier) *API {
	return &API{Service: svc, core: c, verifier: v}
}

// Handler returns the routes wrapped in request-id, access-log and authentication
// middleware: every route needs a valid adapter token.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", a.postMessage)
	mux.HandleFunc("GET /v1/jobs/{id}", a.getJob)
	mux.HandleFunc("GET /v1/sessions/{id}", a.getSession)
	mux.HandleFunc("GET /v1/sessions/{id}/cost", a.getSessionCost)
	mux.HandleFunc("POST /v1/threads", a.postThread)
	mux.HandleFunc("POST /v1/seen", a.postSeen)
	mux.HandleFunc("GET /v1/audio/{message_id}", a.getAudio)
	return a.middleware(a.authenticate(mux))
}

// maxMessageBytes bounds a request body: voice notes travel inside it as base64.
const maxMessageBytes = 40 << 20

// maxWait caps how long a request may block waiting for a job.
const maxWait = 10 * time.Minute

// POST /v1/messages        -> 202 {job_id, session_id, status, events...}
// POST /v1/messages?wait=1 -> blocks until the job stops running (done, failed,
// or needs_confirmation) and returns the same shape with the result.
func (a *API) postMessage(w http.ResponseWriter, r *http.Request) {
	var m core.Message
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMessageBytes))
	if err := dec.Decode(&m); err != nil {
		a.fail(w, r, http.StatusBadRequest, event.Wrap("The request body is not valid JSON.", err))
		return
	}
	// An adapter may only speak as itself, or its messages could be filed under
	// another adapter's threads.
	if caller := adapterFrom(r.Context()); m.Adapter == "" {
		m.Adapter = caller
	} else if m.Adapter != caller {
		a.fail(w, r, http.StatusForbidden, event.Wrap("An adapter may only send messages as itself.", fmt.Errorf("token for %q sent a message as %q", caller, m.Adapter)))
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

// waitForChange blocks until the job has events after seq, has finished, or
// time is up.
func (a *API) waitForChange(r *http.Request, job *core.Job, after int, max time.Duration) {
	timeout := time.After(max)
	for {
		ch := job.Changed()
		if v := job.View(after); len(v.Events) > 0 || v.Status != core.Running {
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

func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sess, err := a.Store.GetSession(ctx, r.PathValue("id"))
	if err != nil {
		a.storeFail(w, r, err, "I don't know that session.")
		return
	}
	msgs, err := a.Store.ListMessages(ctx, sess.ID)
	if err != nil {
		a.storeFail(w, r, err, "")
		return
	}
	cv, err := a.cost(r, sess.ID, false)
	if err != nil {
		a.storeFail(w, r, err, "")
		return
	}
	v := model.SessionView{ID: sess.ID, Kind: sess.Kind, URL: sess.Url.String, CreatedOn: sess.CreatedOn, Messages: []model.MessageView{}, Cost: cv}
	for _, m := range msgs {
		mv := model.MessageView{ID: m.ID, ParentID: m.ParentID.String, Role: m.Role, Format: m.Format.String, Text: m.Text.String, CreatedOn: m.CreatedOn}
		if m.AudioKey.Valid {
			mv.AudioURL = a.audioURL(ctx, m.AudioKey.String, m.ID)
		}
		v.Messages = append(v.Messages, mv)
	}
	a.writeJSON(w, http.StatusOK, v)
}

func (a *API) getSessionCost(w http.ResponseWriter, r *http.Request) {
	sess, err := a.Store.GetSession(r.Context(), r.PathValue("id"))
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

func (a *API) cost(r *http.Request, sessionID string, withLedger bool) (model.CostView, error) {
	sc, err := a.Store.SessionCost(r.Context(), sessionID)
	if err != nil {
		return model.CostView{}, err
	}
	cv := model.CostView{TotalUSD: sc.TotalUsd, Calls: sc.Calls, UnknownCalls: sc.UnknownCalls}
	if withLedger {
		rows, err := a.Store.ListLedger(r.Context(), sessionID)
		if err != nil {
			return model.CostView{}, err
		}
		for _, l := range rows {
			lv := model.LedgerView{Role: l.Role, Provider: l.Provider, Model: l.Model, GenerationID: l.GenerationID.String, Chars: l.Chars, CreatedOn: l.CreatedOn}
			if l.InputUnits.Valid {
				lv.InputUnits = &l.InputUnits.Int64
			}
			if l.OutputUnits.Valid {
				lv.OutputUnits = &l.OutputUnits.Int64
			}
			if l.CostUsd.Valid {
				lv.CostUSD = &l.CostUsd.Float64
			}
			cv.Ledger = append(cv.Ledger, lv)
		}
	}
	return cv, nil
}

// POST /v1/seen {ref}: an adapter asks "have I handled this inbound message
// before?" (a platform retry). The first call for a ref answers duplicate:false and
// records it; later calls answer duplicate:true. The adapter is taken from the token.
func (a *API) postSeen(w http.ResponseWriter, r *http.Request) {
	var req model.SeenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil || req.Ref == "" {
		a.fail(w, r, http.StatusBadRequest, event.Wrap("ref is required.", err))
		return
	}
	first, err := a.Store.MarkSeen(r.Context(), adapterFrom(r.Context()), req.Ref)
	if err != nil {
		a.storeFail(w, r, err, "")
		return
	}
	a.writeJSON(w, http.StatusOK, model.SeenResponse{Duplicate: !first})
}

// POST /v1/threads {adapter, thread_ref, message_id}: an adapter registers its
// handle for a message it sent (e.g. Telegram chat_id:message_id) so that a
// reply to that message finds its conversation.
func (a *API) postThread(w http.ResponseWriter, r *http.Request) {
	var t model.ThreadRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&t); err != nil || t.Adapter == "" || t.ThreadRef == "" || t.MessageID == "" {
		a.fail(w, r, http.StatusBadRequest, event.Wrap("adapter, thread_ref and message_id are required.", err))
		return
	}
	if caller := adapterFrom(r.Context()); t.Adapter != caller {
		a.fail(w, r, http.StatusForbidden, event.Wrap("An adapter may only register its own threads.", fmt.Errorf("token for %q registered a thread as %q", caller, t.Adapter)))
		return
	}
	if _, err := a.Store.GetMessage(r.Context(), t.MessageID); err != nil {
		a.storeFail(w, r, err, "I don't know that message.")
		return
	}
	if err := a.Store.PutThread(r.Context(), t.Adapter, t.ThreadRef, t.MessageID); err != nil {
		a.storeFail(w, r, err, "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// audioURL is a presigned link to the stored audio, made on demand (signing is
// local and free). Stores that cannot sign get the API's own streaming route.
func (a *API) audioURL(ctx context.Context, key, messageID string) string {
	u, err := a.Objects.PresignGet(ctx, key, audioURLTTL)
	if err != nil {
		return "/v1/audio/" + messageID
	}
	return u
}

// GET /v1/audio/{message_id} redirects to a presigned URL of the audio, or
// streams it (with range support for seeking) when the store has no URLs.
func (a *API) getAudio(w http.ResponseWriter, r *http.Request) {
	msg, err := a.Store.GetMessage(r.Context(), r.PathValue("message_id"))
	if err != nil || !msg.AudioKey.Valid {
		a.fail(w, r, http.StatusNotFound, event.Wrap("There is no audio for that message.", err))
		return
	}
	key := msg.AudioKey.String
	if u, err := a.Objects.PresignGet(r.Context(), key, audioURLTTL); err == nil {
		http.Redirect(w, r, u, http.StatusFound)
		return
	}
	b, err := a.Objects.Get(r.Context(), key)
	if err != nil {
		a.fail(w, r, http.StatusNotFound, event.Wrap("That audio file is no longer available.", err))
		return
	}
	mime := "audio/mpeg"
	if strings.HasSuffix(key, ".ogg") {
		mime = "audio/ogg"
	}
	w.Header().Set("Content-Type", mime)
	http.ServeContent(w, r, "audio.mp3", msg.CreatedOn, bytes.NewReader(b))
}

// fail logs the full cause and sends only the safe message plus the request id.
func (a *API) fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	log := logx.With(r.Context(), a.Log)
	if status >= 500 {
		log.Errorw("request failed", "status", status, "err", err)
	} else {
		log.Warnw("request rejected", "status", status, "err", err)
	}
	var b model.ErrorBody
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
		a.Log.Warnw("write response failed", "err", err)
	}
}
