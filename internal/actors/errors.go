package actors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// ErrorClass is one of PRD §13.5's adapter error classes.
type ErrorClass string

// The §13.5 classes, in the order that section lists them, followed by one
// addition this repo has made beyond the PRD text: see
// ClassCapacityExhausted.
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
	// ClassCapacityExhausted is a provider-side quota, per-session limit, or
	// rate-window exhaustion — not one of the PRD's original nine (issue
	// #48's cascade diagnosis). It is deliberately NOT inferred from a
	// status code the way the other nine are: a quota exhaustion and
	// ordinary backpressure both show up as a 429 (sometimes a 5xx), so
	// classifyStatus keeps mapping those unchanged — 429 stays
	// rate_limited, full stop. The only path to this class is a bridge's
	// own error body declaring "class":"capacity_exhausted" (see
	// classifyBody below): the bridge already knows, from the provider's
	// own error shape, which kind of limit it hit, and this package trusts
	// that self-report the same way §13.5 already trusts a bridge's
	// auth_or_policy or actor_rejected_input declarations.
	ClassCapacityExhausted ErrorClass = "capacity_exhausted"
)

// ErrorClasses returns the §13.5 classes in the order that section lists
// them, followed by ClassCapacityExhausted.
func ErrorClasses() []ErrorClass {
	return []ErrorClass{
		ClassRetryableTransport, ClassRateLimited, ClassActorUnavailable,
		ClassActorRejectedInput, ClassAuthOrPolicy, ClassContract,
		ClassExecution, ClassTimeout, ClassCancelled, ClassCapacityExhausted,
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
// capacity_exhausted joins that non-retryable side deliberately, even though
// at a glance it looks like rate_limited's sibling. Retrying inside the
// attempt against a hard provider quota or session limit is exactly the
// cascade issue #48 diagnosed: the retry does not wait out backpressure, it
// burns another billable session against a wall that has not moved. The
// actor identity is not "fine, just slow" the way actor_unavailable's is —
// it is exhausted, and the right response lives one layer up, between
// attempts (the circuit breaker, task t9), not inside this one.
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
		// capacity_exhausted, and anything an actor invented.
		//
		// capacity_exhausted collapses into the same StatusFailed as its
		// siblings here — TechStatusFor's job is the §3.4 technical status,
		// and §3.4 has no dedicated status for "the actor's capacity is
		// exhausted". The class itself is not lost by that collapse: the
		// mapping is lossy on purpose (three classes already share
		// `failed`), and completeFromInvocationError's own comment in
		// internal/worker/dispatch.go explains why — the class rides along
		// in the attempt's persisted output precisely so an operator (or,
		// from task t9 on, the circuit breaker) can still tell a quota
		// exhaustion apart from a plain execution failure after the fact.
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
	// TerminationReason is the provider's own reason for ending the turn,
	// attached to the same error body Usage comes from, nil when the actor
	// attached none. It is carried separately from Usage for ADR 0009's
	// reason: an error body can name the reason ("max_output_tokens", a
	// cancellation) while holding no parseable usage block to attach it to.
	// It is not a second copy of Class — the class is this package's
	// §13.5 classification of the failure, the reason is the provider's
	// statement about the turn.
	TerminationReason *string
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

// TerminationReasonOf is UsageOf's sibling for the termination reason: it
// extracts the reason a classified invocation failure's error body carried,
// nil when err is not one or when its body named none. The worker threads it
// onto the failed attempt beside the usage, so a bridge that reported WHY
// its turn ended — including when it held no usage block to report — does
// not have that fact dropped at the sync failure path.
func TerminationReasonOf(err error) *string {
	var invErr *InvocationError
	if errors.As(err, &invErr) {
		return invErr.TerminationReason
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

// bodyDeclarableClasses is the set of §13.5 classes a bridge's own error
// body is trusted to declare directly, overriding whatever classifyStatus
// would have inferred from the HTTP status alone. It has exactly one member
// today: capacity_exhausted. Every other class stays status-derived — a
// bridge cannot talk classifyStatus's output for auth_or_policy, contract,
// or any of the rest into something else by putting a different value in
// its "class" field, because classifyBody only ever looks for THIS set.
// That asymmetry is deliberate: a provider quota, session limit, or
// rate-window exhaustion is genuinely indistinguishable from ordinary
// backpressure at the status-code level (both commonly answer 429, and
// providers are not consistent about even that), so the status heuristic
// has no honest opinion to defend here the way it does everywhere else.
var bodyDeclarableClasses = map[ErrorClass]bool{
	ClassCapacityExhausted: true,
}

// classifyBody looks for a bridge-declared class in a non-2xx response
// body, returning it only when the body names a class in
// bodyDeclarableClasses. An absent "class" field, an unparseable body, or a
// declared class this package does not trust a bridge to self-report all
// yield ok=false, leaving classifyStatus's status-based classification
// untouched — this function only ever narrows towards capacity_exhausted,
// it never invents a different status-based class.
//
// It reads the same raw payload usageFromErrorBody does (not the truncated
// Body capture), for the same reason: a large error document must not cost
// the attempt its classification any more than it costs it its accounting.
func classifyBody(payload []byte) (ErrorClass, bool) {
	var body struct {
		Class ErrorClass `json:"class"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return "", false
	}
	if bodyDeclarableClasses[body.Class] {
		return body.Class, true
	}
	return "", false
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
