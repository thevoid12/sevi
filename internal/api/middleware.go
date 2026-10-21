package api

import (
	"net/http"
	"time"

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
