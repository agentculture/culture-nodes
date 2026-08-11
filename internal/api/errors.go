package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// apiError pairs the HTTP status this response reports with the JSON body
// every error response carries — see the package doc's "Error shape"
// section. It implements error so a handler can simply `return
// notFound(...)` and let (*Server).wrap render it.
type apiError struct {
	Status int
	Body   *clifmt.CliError
}

func (e *apiError) Error() string { return e.Body.Message }

func newAPIError(status, code int, message, remediation string) *apiError {
	return &apiError{Status: status, Body: &clifmt.CliError{Code: code, Message: message, Remediation: remediation}}
}

// badRequest, notFound, conflict, unprocessable, unavailable, and
// unauthorized build the user/domain-error bucket (clifmt.ExitUserError) at
// their respective HTTP statuses. internalError builds the environment-error
// bucket (clifmt.ExitEnvError) at 500 for a failure the caller did nothing to
// cause.
func badRequest(remediation, format string, args ...any) *apiError {
	return newAPIError(http.StatusBadRequest, clifmt.ExitUserError, fmt.Sprintf(format, args...), remediation)
}

// unauthorized builds a 401 — used only by the human-task decision endpoint
// (see (*Server).requireDecisionAuth): every other operation in this API is
// authless by phase-1 design (PRD spec decision c45).
func unauthorized(remediation, format string, args ...any) *apiError {
	return newAPIError(http.StatusUnauthorized, clifmt.ExitUserError, fmt.Sprintf(format, args...), remediation)
}

func notFound(remediation, format string, args ...any) *apiError {
	return newAPIError(http.StatusNotFound, clifmt.ExitUserError, fmt.Sprintf(format, args...), remediation)
}

func conflict(remediation, format string, args ...any) *apiError {
	return newAPIError(http.StatusConflict, clifmt.ExitUserError, fmt.Sprintf(format, args...), remediation)
}

func unprocessable(remediation, format string, args ...any) *apiError {
	return newAPIError(http.StatusUnprocessableEntity, clifmt.ExitUserError, fmt.Sprintf(format, args...), remediation)
}

func unavailable(remediation, format string, args ...any) *apiError {
	return newAPIError(http.StatusServiceUnavailable, clifmt.ExitEnvError, fmt.Sprintf(format, args...), remediation)
}

func internalError(err error) *apiError {
	return newAPIError(http.StatusInternalServerError, clifmt.ExitEnvError,
		fmt.Sprintf("internal error: %v", err),
		fmt.Sprintf("retry; if this persists, file a bug at %s", clifmt.IssuesURL))
}

// classify translates a domain error from internal/engine, internal/ledger,
// or internal/store/postgres into the apiError a handler should return. It
// is the one place the HTTP status for each domain condition is decided, so
// two handlers hitting the same underlying error report it the same way.
func classify(err error) *apiError {
	if err == nil {
		return nil
	}

	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr
	}

	switch {
	case errors.Is(err, engine.ErrNotFound), errors.Is(err, postgres.ErrNotFound),
		errors.Is(err, ledger.ErrRecordNotFound), errors.Is(err, ledger.ErrReviewNotFound):
		return notFound("check the id and try again", "%v", err)

	case errors.Is(err, engine.ErrContract):
		return badRequest("the payload must satisfy the declared contract", "%v", err)

	case errors.Is(err, ledger.ErrStaleReview), errors.Is(err, ledger.ErrReviewAlreadyCommitted):
		return conflict("re-read the current ledger version and, if still needed, submit a new review request", "%v", err)

	case errors.Is(err, engine.ErrTerminalRun), errors.Is(err, engine.ErrTerminalNodeRun):
		return conflict("the run has already reached a terminal state", "%v", err)

	case errors.Is(err, postgres.ErrDuplicateDigest):
		return conflict("fetch the existing version instead of publishing again", "%v", err)

	case errors.Is(err, engine.ErrHumanTaskAlreadyDecided):
		return conflict("re-read the task; it has already been decided and accepts no further decision", "%v", err)

	case errors.Is(err, engine.ErrOutcomeNotAllowed):
		return badRequest("choose one of the task's allowed_outcomes", "%v", err)
	}

	var reviewTargetErr *ledger.ReviewTargetError
	if errors.As(err, &reviewTargetErr) {
		return badRequest("the decision set must exactly cover the records the review named, no more, no less", "%v", err)
	}

	var authorityErr *ledger.AuthorityError
	if errors.As(err, &authorityErr) {
		return badRequest("the producer named in origin may not write this record's authority; see PRD §10.4", "%v", err)
	}

	var workflowErr *engine.WorkflowError
	if errors.As(err, &workflowErr) {
		return internalError(err)
	}

	return internalError(err)
}

// writeJSON encodes v as the response body with the given status. It never
// pretty-prints: SSE and log consumers alike are better served by compact,
// single-line JSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeAPIError renders err in the documented Error shape. Any error that is
// not (or does not wrap) an *apiError is treated as an unclassified
// environment failure — never a raw Go error string leaking to a caller
// without a remediation.
//
// It is also the one place every JSON error response passes through — both
// (*Server).wrap's handlerFunc results and handleStreamRunEvents' own two
// pre-stream failures (events.go) call it directly — so it is the central
// funnel this package's "give internal/api a logging facility" task hooks:
// a 5xx response logs one Error-level line carrying err's full unwrapped
// chain plus the request's method and path (see the package doc's
// "Logging" section). A 4xx is a domain/user outcome, not a failure this
// process needs paged on, so it is never logged here.
func (s *Server) writeAPIError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = internalError(err)
	}
	if ae.Status >= http.StatusInternalServerError {
		s.log.Error("api: request failed",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ae.Status,
			"error", ae.Error(),
		)
	}
	writeJSON(w, ae.Status, ae.Body)
}

// handlerFunc is one route's implementation. Returning a non-nil error — an
// *apiError from the helpers above, or classify(err) — is how a handler
// reports a failure; wrap renders it. A handler that writes a 2xx response
// itself (writeJSON, or the SSE handler writing frames directly) returns
// nil.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// wrap adapts a handlerFunc to http.HandlerFunc, rendering any returned
// error in the documented shape.
func (s *Server) wrap(h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			s.writeAPIError(w, r, err)
		}
	}
}
