package actors

import (
	"encoding/json"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// The wire types below mirror PRD §13.1–13.4 field for field, including field
// names and JSON casing. They are deliberately a separate declaration from
// anything internal — an adapter author reads this file to learn the
// protocol, so a field here that the PRD does not name, or a name that does
// not match, would be a lie about the contract.

// ProtocolVersion is the value every invocation carries in
// `protocol_version`. It is a string, not a number, because §13.1 shows
// "1.0" and a protocol version is an identifier rather than an arithmetic
// quantity.
const ProtocolVersion = "1.0"

// InvocationPath is the path §13.1 fixes for an invocation, appended to an
// actor's base URL.
const InvocationPath = "/v1/invocations"

// CallbackEventsPathFormat builds the callback URL §13.1 hands the actor,
// relative to the control plane's public base URL. §13.1's example is
// "https://nodes.example/v1/attempts/att_01J/events".
const CallbackEventsPathFormat = "/v1/attempts/%s/events"

// IdempotencyKeyHeader carries the attempt key §13.1 requires on every
// invocation. It is what makes a transport-level retry of the same attempt
// safe (§20.3): an actor that has already accepted this key must return the
// result it already produced rather than start the work again.
const IdempotencyKeyHeader = "Idempotency-Key"

// TraceparentHeader carries W3C trace context (§13.1, §15.2).
const TraceparentHeader = "Traceparent"

// WorkflowRef identifies the immutable definition a run pinned.
type WorkflowRef struct {
	Name          string `json:"name"`
	VersionDigest string `json:"version_digest"`
}

// NodeRef identifies the node being executed and the contract it must
// satisfy. ContractDigest is content-addressed, so an actor can cache a
// compiled contract by digest and know it can never go stale.
type NodeRef struct {
	ID             string `json:"id"`
	ContractDigest string `json:"contract_digest"`
}

// Callback is where the actor sends §13.4 events, and the short-lived
// attempt-scoped credential that authorizes it to. The token is scoped to one
// attempt and expires: an actor that leaks it leaks the ability to report on
// one attempt for a few minutes, not the ability to write to the control
// plane.
type Callback struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// InvocationRequest is the §13.1 request body.
type InvocationRequest struct {
	ProtocolVersion string          `json:"protocol_version"`
	RunID           string          `json:"run_id"`
	TokenID         string          `json:"token_id"`
	NodeRunID       string          `json:"node_run_id"`
	AttemptID       string          `json:"attempt_id"`
	Attempt         int             `json:"attempt"`
	Workflow        WorkflowRef     `json:"workflow"`
	Node            NodeRef         `json:"node"`
	Input           json.RawMessage `json:"input"`
	ArtifactRefs    []string        `json:"artifact_refs"`
	ContextRefs     []string        `json:"context_refs"`
	Deadline        *time.Time      `json:"deadline,omitempty"`
	Callback        Callback        `json:"callback"`
}

// Usage is the §13.2 telemetry block. Cost and Currency are pointers because
// §13.2 shows both as nullable: an actor that does not price its work says so
// with null rather than with a zero that reads as "free".
type Usage struct {
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	Cost         *float64 `json:"cost"`
	Currency     *string  `json:"currency"`
}

// ToEngine converts this §13.2 wire Usage block into the engine's own copy
// of it (engine.CompletionRequest.Usage, engine.Attempt.Usage), which is how
// it reaches storage: the engine cannot import this package (this package
// already imports engine, for the Completer interface and CompletionRequest
// its callback commit path uses), so the engine declares an independent
// Usage type and this is the one seam that translates into it. Both
// completion paths that carry a Usage block convert through it —
// internal/worker/dispatch.go's synchronous completeFromResult (on an
// InvocationResult.Usage) and this package's own commitTerminal, by way of
// completionFor (on a CompletedPayload.Usage) — so a nil Usage always
// becomes a nil engine.Usage, never a fabricated zero block.
func (u *Usage) ToEngine() *engine.Usage {
	if u == nil {
		return nil
	}
	return &engine.Usage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		Cost:         u.Cost,
		Currency:     u.Currency,
	}
}

// LedgerDelta is what the actor proposes to write to the work ledger. It is
// a proposal: §10.4 lets an agent-origin record be `proposed` and nothing
// stronger, and the engine re-checks that on append regardless of what the
// records here claim.
type LedgerDelta struct {
	Records []ledger.Record `json:"records"`
}

// InvocationResult is the §13.2 synchronous result body (HTTP 200).
type InvocationResult struct {
	Outcome         string          `json:"outcome"`
	Output          json.RawMessage `json:"output"`
	LedgerDelta     *LedgerDelta    `json:"ledger_delta"`
	ArtifactRefs    []string        `json:"artifact_refs"`
	ContinuationRef *string         `json:"continuation_ref"`
	Usage           *Usage          `json:"usage"`
}

// Records returns the proposed ledger records, or nil when the result carried
// no delta. It exists so callers do not have to nil-check a pointer to get at
// a slice that is empty either way.
func (r *InvocationResult) Records() []ledger.Record {
	if r == nil || r.LedgerDelta == nil {
		return nil
	}
	return r.LedgerDelta.Records
}

// AsyncAccepted is the §13.3 asynchronous acceptance body (HTTP 202).
//
// HeartbeatAfterSeconds is the actor's own promise about liveness, not a
// request: the control plane records it as a durable deadline (§12.6) and the
// actor is expected to send a §13.4 heartbeat at least that often.
type AsyncAccepted struct {
	InvocationID          string `json:"invocation_id"`
	HeartbeatAfterSeconds int    `json:"heartbeat_after_seconds"`
	SupportsCancellation  bool   `json:"supports_cancellation"`
}

// InvocationResponse is what one invocation produced: either a synchronous
// result or an asynchronous acceptance, never both.
//
// The two are a single return value rather than two methods because the
// choice belongs to the actor, not the caller — §9.5's actor manifest
// declares "supported sync/async behavior", and a caller that had to pick up
// front would be guessing at something only the actor can answer.
type InvocationResponse struct {
	// Async reports which half is populated.
	Async bool
	// Result is the §13.2 body, non-nil when Async is false.
	Result *InvocationResult
	// Accepted is the §13.3 body, non-nil when Async is true.
	Accepted *AsyncAccepted
	// StatusCode is the HTTP status the actor returned (200 or 202).
	StatusCode int
	// Requests is how many HTTP requests this invocation cost, including
	// retries of retryable classes. It is 1 for a first-try success.
	Requests int
}

// EventKind is one of §13.4's callback event kinds.
type EventKind string

// The §13.4 event kinds, in the order that section lists them.
const (
	// EventAccepted acknowledges the invocation and may carry the actor's
	// invocation id.
	EventAccepted EventKind = "accepted"
	// EventHeartbeat is a liveness signal.
	EventHeartbeat EventKind = "heartbeat"
	// EventProgress reports partial progress. It carries no committed state.
	EventProgress EventKind = "progress"
	// EventArtifact announces an artifact reference.
	EventArtifact EventKind = "artifact"
	// EventCompleted is terminal: the actor produced a domain outcome.
	EventCompleted EventKind = "completed"
	// EventFailed is terminal: the dispatch itself failed.
	EventFailed EventKind = "failed"
	// EventBlocked is terminal: the actor cannot proceed. It is routed as the
	// domain outcome `blocked`, which a node either declares or does not —
	// the engine decides, this package does not special-case it.
	EventBlocked EventKind = "blocked"
)

// Terminal reports whether an event ends the attempt. Only terminal events
// commit workflow state; the rest are liveness and telemetry.
func (k EventKind) Terminal() bool {
	switch k {
	case EventCompleted, EventFailed, EventBlocked:
		return true
	default:
		return false
	}
}

// Valid reports whether k is one of §13.4's kinds.
func (k EventKind) Valid() bool {
	switch k {
	case EventAccepted, EventHeartbeat, EventProgress, EventArtifact,
		EventCompleted, EventFailed, EventBlocked:
		return true
	default:
		return false
	}
}

// CallbackEvent is one §13.4 event.
//
// EventID and Sequence are both required and do different jobs: the event id
// makes a redelivery recognizable (idempotency), the sequence makes a
// reordering recognizable (monotonicity). An actor that supplied only one of
// them would leave the other failure mode undetectable.
type CallbackEvent struct {
	EventID  string          `json:"event_id"`
	Sequence int64           `json:"sequence"`
	Kind     EventKind       `json:"kind"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// CompletedPayload is the body of a `completed` (or `blocked`) event: the
// same shape §13.2 defines for a synchronous result, because an actor that
// finished late produced exactly the same kind of answer as one that finished
// inline.
type CompletedPayload struct {
	Outcome      string          `json:"outcome"`
	Output       json.RawMessage `json:"output"`
	LedgerDelta  *LedgerDelta    `json:"ledger_delta"`
	ArtifactRefs []string        `json:"artifact_refs"`
	Usage        *Usage          `json:"usage"`
}

// FailedPayload is the body of a `failed` event. Class is one of §13.5's
// error classes as the actor classified it; an unrecognized or absent class
// is treated as an execution failure, which is the honest default for "the
// actor ran and something went wrong" .
type FailedPayload struct {
	Class   ErrorClass `json:"class"`
	Message string     `json:"message"`
	Detail  string     `json:"detail,omitempty"`
}

// AcceptedPayload is the body of an `accepted` event. It may restate the
// invocation id, which is the only way an actor that answered 202 with an
// empty id can supply one later.
type AcceptedPayload struct {
	InvocationID          string `json:"invocation_id,omitempty"`
	HeartbeatAfterSeconds int    `json:"heartbeat_after_seconds,omitempty"`
}

// CancelRequest is the §13.6 cancellation body. Cancellation is durable in
// Culture Nodes and best-effort at the actor, so this body carries only what
// identifies the work — there is no acknowledgement field, because workflow
// state does not depend on one.
type CancelRequest struct {
	InvocationID string `json:"invocation_id"`
	Reason       string `json:"reason,omitempty"`
}
