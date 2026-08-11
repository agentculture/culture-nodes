package api

import (
	"bytes"
	"net/http"
	"strings"
)

// statusRecorder wraps a ResponseWriter to capture the status code and body
// a handler wrote, without changing what the real client receives — every
// byte still goes through to w. logCallbackFailures is the one caller: it
// needs to know, after the wrapped handler has already finished writing,
// whether the response was a 5xx and what body it sent, and neither is
// otherwise observable once ServeHTTP returns.
type statusRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Write implicitly answers 200 if the handler never called WriteHeader,
// mirroring the same default net/http itself applies.
func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// logCallbackFailures wraps the actor callback ingest route
// (actors.NewCallbackHandler, mounted in Handler) so a terminal-commit
// failure is logged at Error level with the attempt id — the log half of
// the spec's c2 (see the package doc's "Logging" section; the event half,
// a diagnostic event on the run's audit log, is a different task in
// internal/actors).
//
// The attempt id is read from r.PathValue("id"): callbackRoutePattern
// registers the route with a {id} wildcard, and http.ServeMux populates
// PathValue for every request it dispatches through that pattern regardless
// of how many handlers wrap the one actually given to Handle — so this
// middleware sees the same attempt id the wrapped handler does, without
// having to re-parse the URL itself (actors.attemptIDFromPath is
// unexported, and internal/actors is out of scope for this change).
//
// It only inspects the response after the wrapped handler has already
// produced it; nothing here changes what the caller receives.
func (s *Server) logCallbackFailures(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		if rec.status >= http.StatusInternalServerError {
			s.log.Error("api: actor callback ingest failed to commit",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"attempt_id", r.PathValue("id"),
				"error", strings.TrimSpace(rec.body.String()),
			)
		}
	})
}
