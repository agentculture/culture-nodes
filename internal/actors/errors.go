package actors

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// ErrorClass is one of PRD §13.5's nine adapter error classes.
type ErrorClass string

// The §13.5 classes, in the order that section lists them.
const (
	// ClassRetryableTransport is a network-level failure that says nothing
	// about the actor: a refused connection, a reset, a truncated body.
	ClassRetryableTransport ErrorClass = "retryable_transport"
	// ClassRateLimited is the actor asking for less traffic (429).
	ClassRateLimited ErrorClass = "rate_limited"
	// ClassActorUnavailable is the actor being reachable but not currently
	// able to serve (502, 503, 504, or a missing invocation endpoint).
	ClassActorUnavailable ErrorClass = "actor_unavailable"
	// ClassActorRejectedInput is the actor refusing the request body (400,
	// 422). The input will not become acceptable by being sent again.
	ClassActorRejectedInput ErrorClass = "actor_rejected_input"
	// ClassAuthOrPolicy is a credential or policy refusal (401, 403).
	ClassAuthOrPolicy ErrorClass = "auth_or_policy"
	// ClassContract is a response the protocol cannot accept: an
	// undecodable body, a missing outcome, a status the protocol does not
	// define, or a conflict against an idempotency key (409).
	ClassContract ErrorClass = "contract"
	// ClassExecution is the actor accepting the work, running it, and
	// failing (500, or a `failed` callback with no better classification).
	ClassExecution ErrorClass = "execution"
	// ClassTimeout is the invocation exceeding its deadline (408, or a
	// client-side deadline).
	ClassTimeout ErrorClass = "timeout"
	// ClassCancelled is the invocation being cancelled (§13.6), including a
	// caller-cancelled context.
	ClassCancelled ErrorClass = "cancelled"
)

// ErrorClasses returns the §13.5 classes in the order that section lists
// them.
func ErrorClasses() []ErrorClass {
	return []ErrorClass{
		ClassRetryableTransport, ClassRateLimited, ClassActorUnavailable,
		ClassActorRejectedInput, ClassAuthOrPolicy, ClassContract,
		ClassExecution, ClassTimeout, ClassCancelled,
	}
}

// Valid reports whether c is one of §13.5's classes.
func (c ErrorClass) Valid() bool {
	for _, known := range ErrorClasses() {
		if c == known {
			return true
		}
	}
	return false
}

// Retryable implements §13.5's closing rule: "only explicitly retryable
// categories use automatic retry policy."
//
// Four classes qualify, and each for a stated reason:
//
//   - retryable_transport — the request may not have reached the actor at
//     all; the same Idempotency-Key makes resending safe even if it did.
//   - rate_limited — the actor asked for less traffic now, not never.
//   - actor_unavailable — a gateway or a restarting instance; the actor
//     identity is fine, the instance is not.
//   - timeout — the deadline was this client's, not a verdict on the work.
//
// The other five are refusals of *this* request as sent. Resending an input
// the actor rejected, a credential it refused, a body the protocol cannot
// parse, a failure it already ran, or a cancellation, would produce the same
// answer and burn an attempt doing it.
//
// Note what this predicate is and is not: it governs the client's own
// bounded, same-key retry inside one attempt. Whether the *engine* dispatches
// another attempt is a separate decision made by the node's declared retry
// policy against the recorded technical status (see TechStatusFor).
func (c ErrorClass) Retryable() bool {
	switch c {
	case ClassRetryableTransport, ClassRateLimited, ClassActorUnavailable, ClassTimeout:
		return true
	default:
		return false
	}
}

// TechStatusFor maps a §13.5 error class onto the §3.4 technical status the
// engine records for the attempt.
//
// The mapping is by truth, not by convenience. A class that means "the
// credential or policy said no" becomes policy_denied, which the engine never
// retries — retrying a denial would just be denied again. A class that means
// "the actor refused the payload" becomes contract_rejected, because that is
// exactly what happened: a contract was not satisfied. Everything the engine
// has no more specific word for becomes failed.
//
// Nothing here ever returns succeeded: a technical failure has no domain
// answer to route, and §3.4 exists to stop those two being confused.
func TechStatusFor(c ErrorClass) engine.TechStatus {
	switch c {
	case ClassTimeout:
		return engine.StatusTimedOut
	case ClassCancelled:
		return engine.StatusCancelled
	case ClassAuthOrPolicy:
		return engine.StatusPolicyDenied
	case ClassActorRejectedInput, ClassContract:
		return engine.StatusContractRejected
	default:
		// retryable_transport, rate_limited, actor_unavailable, execution,
		// and anything an actor invented.
		return engine.StatusFailed
	}
}

// maxCapturedBodyBytes bounds how much of an error response body is kept on
// an InvocationError. An actor is entitled to return a large error document;
// this package is not entitled to carry all of it into a diagnostic that ends
// up in an event payload (internal/events forbids unbounded payloads).
const maxCapturedBodyBytes = 2048

// InvocationError is a classified §13.5 failure of one invocation.
type InvocationError struct {
	// Class is the §13.5 classification.
	Class ErrorClass
	// StatusCode is the HTTP status, or 0 for a failure that never got one.
	StatusCode int
	// Op names what was being attempted ("invoke", "cancel").
	Op string
	// Message is a one-line human summary.
	Message string
	// Body is up to maxCapturedBodyBytes of the actor's response body, kept
	// because an adapter author debugging a rejection needs to see what the
	// actor actually said.
	Body string
	// RetryAfter is the delay the actor asked for (Retry-After), zero when it
	// asked for none.
	RetryAfter time.Duration
	// Usage is the §13.2 usage block the actor attached to its error body,
	// nil when it attached none. Issue #32: a failed session still burned
	// real tokens, and the bridges' 500 bodies carry the block alongside
	// `error` and `class` whenever a parseable terminal result existed —
	// the sync twin of ADR 0008's failed-event usage. It is decoded from
	// the full response body, not from the truncated Body capture, so a
	// large error document cannot cost the attempt its accounting. Nil
	// means unreported — the worker persists it as NULL, never as zeros.
	Usage *Usage
	// Requests is how many HTTP requests were spent before giving up.
	Requests int
	// Err is the underlying transport or decode error, if any.
	Err error
}

func (e *InvocationError) Error() string {
	var b strings.Builder
	b.WriteString("actors: ")
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	b.WriteString(string(e.Class))
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.StatusCode)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *InvocationError) Unwrap() error { return e.Err }

// Retryable reports whether §13.5's automatic retry policy applies to this
// failure.
func (e *InvocationError) Retryable() bool { return e.Class.Retryable() }

// Is lets callers match any classified failure with errors.Is(err,
// ErrInvocation) without caring which class it was.
func (e *InvocationError) Is(target error) bool { return target == ErrInvocation }

// ErrInvocation is the sentinel every InvocationError matches.
var ErrInvocation = errors.New("actors: invocation failed")

// ClassOf extracts the §13.5 class from an error, reporting false when err is
// not a classified invocation failure.
func ClassOf(err error) (ErrorClass, bool) {
	var invErr *InvocationError
	if errors.As(err, &invErr) {
		return invErr.Class, true
	}
	return "", false
}

// UsageOf extracts the §13.2 usage block from a classified invocation
// failure's error body, nil when err is not one or when its body carried
// none. It is ClassOf's shape for the usage field: the worker's
// completeFromInvocationError threads the result into the failure
// completion so a sync bridge failure with a parseable terminal result
// persists its burn on the failed attempt (issue #32, task t5).
func UsageOf(err error) *Usage {
	var invErr *InvocationError
	if errors.As(err, &invErr) {
		return invErr.Usage
	}
	return nil
}

// classifyStatus maps an HTTP status the actor returned onto a §13.5 class.
//
// Two mappings are worth stating because a reader will otherwise assume the
// obvious alternative:
//
//   - 404 is actor_unavailable, not a permanent error. A missing
//     /v1/invocations is usually a routing or rollout state, and the class
//     that says "the identity is right, this instance is not" is the honest
//     one. A genuinely permanent 404 is bounded by the node's retry policy
//     rather than by this classification.
//   - 500 is execution, not actor_unavailable. A 500 means the request
//     reached the actor's own handler and that handler failed, so the work
//     may well have partially happened; re-sending it automatically is the
//     one thing that should not be assumed safe. 502/503/504 come from in
//     front of the handler and are therefore retryable.
func classifyStatus(status int) ErrorClass {
	switch status {
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return ClassTimeout
	case http.StatusTooManyRequests:
		return ClassRateLimited
	case http.StatusUnauthorized, http.StatusForbidden:
		return ClassAuthOrPolicy
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return ClassActorRejectedInput
	case http.StatusConflict:
		return ClassContract
	case http.StatusNotFound, http.StatusBadGateway, http.StatusServiceUnavailable:
		return ClassActorUnavailable
	case http.StatusInternalServerError:
		return ClassExecution
	}
	switch {
	case status >= 500:
		return ClassActorUnavailable
	case status >= 400:
		return ClassActorRejectedInput
	default:
		// A 1xx/3xx, or a 2xx the protocol does not define. The response is
		// not something §13 can interpret, which is a contract failure.
		return ClassContract
	}
}

// classifyTransport maps a transport-level failure onto a §13.5 class. The
// caller's own context is consulted first: a cancelled or expired context is
// a statement about *us*, and reporting it as the actor being unreachable
// would blame the wrong side.
func classifyTransport(ctx context.Context, err error) ErrorClass {
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return ClassTimeout
		}
		return ClassCancelled
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout
	case errors.Is(err, context.Canceled):
		return ClassCancelled
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ClassTimeout
	}
	return ClassRetryableTransport
}

// parseRetryAfter reads a Retry-After header in either of the two forms RFC
// 9110 allows: delay-seconds, or an HTTP-date. An unparseable value yields
// zero rather than an error — a malformed hint is worth ignoring, not worth
// failing an already-failed request over.
func parseRetryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil {
		if seconds < 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(header); err == nil {
		if d := when.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
