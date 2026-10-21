package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"errors"

	"sevi/internal/httpx"
	"sevi/internal/reqid"
)

// seviClient talks to Core over its HTTP API only, like any outside adapter.
// Its types mirror the API's JSON; it imports nothing from core or store.
type seviClient struct {
	base string // http://127.0.0.1:8787
}

type part struct {
	Type      string   `json:"type"`
	URL       string   `json:"url,omitempty"`
	Decision  string   `json:"decision,omitempty"`
	MessageID string   `json:"message_id,omitempty"`
	Seconds   int      `json:"seconds,omitempty"`
	Title     string   `json:"title,omitempty"`
	Mime      string   `json:"mime,omitempty"`
	Text      string   `json:"text,omitempty"`
	Data      []byte   `json:"data,omitempty"`
	CostUSD   *float64 `json:"cost_usd,omitempty"`
}

type evt struct {
	Seq     int      `json:"seq"`
	Kind    string   `json:"kind"`
	Message string   `json:"message"`
	CostUSD *float64 `json:"cost_usd"`
	Ref     string   `json:"ref"`
}

type jobView struct {
	ID        string `json:"job_id"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Events    []evt  `json:"events"`
	Parts     []part `json:"parts"`
}

// outgoing is one message to Core. ThreadRef is this message's own handle and
// ReplyToRef the handle of the message it replies to (Telegram chat_id:message_id).
type outgoing struct {
	User       string `json:"user"`
	Adapter    string `json:"adapter"`
	ThreadRef  string `json:"thread_ref,omitempty"`
	ReplyToRef string `json:"reply_to_ref,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Parts      []part `json:"parts"`
}

func (c *seviClient) send(ctx context.Context, m outgoing) (jobView, error) {
	m.Adapter = adapterName
	body, _ := json.Marshal(m)
	var v jobView
	err := c.do(ctx, "POST", "/v1/messages", body, &v)
	return v, err
}

// job long-polls for events after seq `after`.
func (c *seviClient) job(ctx context.Context, id string, after, waitSec int) (jobView, error) {
	var v jobView
	q := url.Values{"after": {strconv.Itoa(after)}, "wait": {strconv.Itoa(waitSec)}}
	err := c.do(ctx, "GET", "/v1/jobs/"+id+"?"+q.Encode(), nil, &v)
	return v, err
}

func (c *seviClient) audio(ctx context.Context, messageID string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.base+"/v1/audio/"+messageID, nil)
	if err != nil {
		return nil, err
	}
	b, _, err := httpx.Do(ctx, req)
	return b, err
}

func (c *seviClient) session(ctx context.Context, id string) (sessionInfo, error) {
	var s sessionInfo
	err := c.do(ctx, "GET", "/v1/sessions/"+id, nil, &s)
	return s, err
}

type sessionInfo struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

func (c *seviClient) registerThread(ctx context.Context, adapter, threadRef, messageID string) error {
	body, _ := json.Marshal(map[string]string{"adapter": adapter, "thread_ref": threadRef, "message_id": messageID})
	return c.do(ctx, "POST", "/v1/threads", body, nil)
}

func (c *seviClient) do(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if id := reqid.From(ctx); id != "" {
		req.Header.Set("X-Request-Id", id) // one id from the Telegram update through Core
	}
	raw, _, err := httpx.Do(ctx, req)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// userMessage extracts the safe message from an API error body, if there is one.
func userMessage(err error) string {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Ref     string `json:"ref"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(se.Body), &e) == nil && e.Error.Message != "" {
			if e.Error.Ref != "" {
				return fmt.Sprintf("%s (ref %s)", e.Error.Message, e.Error.Ref)
			}
			return e.Error.Message
		}
	}
	return "I couldn't reach my own server. Try again in a moment."
}
