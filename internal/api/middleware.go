package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"sevi/internal/auth"
	"sevi/internal/logx"
	"sevi/internal/reqid"
)

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// middleware gives every request an id (honouring X-Request-Id from the
// caller), returns it in the response header, and writes one access-log line.
func (a *API) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 64 {
			id = reqid.New()
		}
		w.Header().Set("X-Request-Id", id)
		r = r.WithContext(reqid.With(r.Context(), id))

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		logx.With(r.Context(), a.log).Infow("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

type adapterKey struct{}

// adapterFrom is the adapter a request was authenticated as ("" if none).
func adapterFrom(ctx context.Context) string {
	a, _ := ctx.Value(adapterKey{}).(string)
	return a
}

// authenticate refuses any request that does not carry a valid adapter token,
// before it reaches a handler. The caller is told only "Unauthorized"; the real
// reason (expired, bad signature, unknown adapter...) goes to the log.
func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		}
		adapter, err := a.verifier.Verify(token)
		if err != nil {
			reason := err.Error()
			if errors.Is(err, auth.ErrUnauthorized) {
				reason = strings.TrimPrefix(reason, auth.ErrUnauthorized.Error()+": ")
			}
			logx.With(r.Context(), a.log).Warnw("refused an unauthenticated request", "reason", reason, "remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
			w.Header().Set("WWW-Authenticate", `Bearer realm="sevi"`)
			a.writeJSON(w, http.StatusUnauthorized, errorBody{Error: struct {
				Message string `json:"message"`
				Ref     string `json:"ref"`
			}{"Unauthorized", reqid.From(r.Context())}})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), adapterKey{}, adapter)))
	})
}
