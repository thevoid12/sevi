// Package httpx is the one place we send HTTP requests, so every provider and
// fetcher gets the same timeout and the same error shape.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

var client = &http.Client{Timeout: 3 * time.Minute}

// StatusError is a non-2xx response.
type StatusError struct {
	Host   string
	Code   int
	Status string
	Body   string
}

func (e *StatusError) Error() string { return fmt.Sprintf("%s: %s: %s", e.Host, e.Status, e.Body) }

// Retryable reports whether trying again could help: network errors, 429 and 5xx.
// Other 4xx (bad key, bad request) will fail the same way every time.
func Retryable(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusTooManyRequests || se.Code >= 500
	}
	return true
}

// Do sends req with ctx and returns the body and headers. Non-2xx responses
// become an error carrying the host, status and the first 300 bytes of body.
func Do(ctx context.Context, req *http.Request) ([]byte, http.Header, error) {
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: read body: %w", req.URL.Host, err)
	}
	if resp.StatusCode >= 300 {
		body := string(b)
		if len(body) > 300 {
			body = body[:300]
		}
		return nil, nil, &StatusError{Host: req.URL.Host, Code: resp.StatusCode, Status: resp.Status, Body: body}
	}
	return b, resp.Header, nil
}
