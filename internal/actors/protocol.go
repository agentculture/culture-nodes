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
//
// ContinuationRef extends §13.1 additively
// (docs/adr/0010-continuation-ref-on-request.md, on ADR 0008's and ADR
// 0009's precedent for §13.2): it is the handle a PRIOR attempt returned in
// InvocationResult.ContinuationRef, handed back so this turn can continue
// that conversation instead of starting a fresh one. §8 ("Explicit
// continuation") is the reason it is a wire field at all — a session is
// passed explicitly or it does not exist, and there is no invisible shared
// conversation for a bridge to find on its own.
//
// It carries §13.2's own field name on purpose. There is one PRD vocabulary
// word for one fact — the provider-side conversation a turn belongs to — and
// the direction comes from the message rather than a second name: on a
// request it is the ref to continue FROM, on a result the ref the actor
// offers to continue WITH.
//
// Absent stays absent: the key is omitted entirely when there is no prior
// ref, never sent as null or "". A bridge's "was I given a session" check is
// key presence, and an empty string is a value a bridge could mistake for a
// handle.
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
	ContinuationRef *string         `json:"continuation_ref,omitempty"`
	Deadline        *time.Time      `json:"deadline,omitempty"`
	Callback        Callback        `json:"callback"`
}

// Usage is the §13.2 telemetry block. Cost and Currency are pointers because
// §13.2 shows both as nullable: an actor that does not price its work says so
// with null rather than with a zero that reads as "free".
//
// CachedInputTokens, ReasoningTokens, Model, and ThreadID extend §13.2
// additively (docs/adr/0009-usage-telemetry-extension.md, on ADR 0008's
// precedent). Every one of them is a pointer for Cost and Currency's exact
// reason: they are independently absent-able *within* a reported block. An
// actor whose contract exposes no cache telemetry at all reports null, which
// means "unmeasurable", where a 0 would claim a measured 0% cache ratio. The
// two token-count fields keep §13.2's own casing convention
// (`cached_input_tokens`, `reasoning_tokens`).
//
// InputTokens/OutputTokens stay plain int64: an actor that reports a usage
// block at all reports those two, and `usage_input_tokens IS NOT NULL` is
// what "this attempt reported usage" means downstream
// (migrations/0012_attempt_usage.sql). Nothing here may be used as a
// presence check for the block as a whole.
//
// The termination reason is deliberately NOT a field of this block — see
// InvocationResult.TerminationReason.
type Usage struct {
	InputTokens       int64    `json:"input_tokens"`
	OutputTokens      int64    `json:"output_tokens"`
	Cost              *float64 `json:"cost"`
	Currency          *string  `json:"currency"`
	CachedInputTokens *int64   `json:"cached_input_tokens,omitempty"`
	ReasoningTokens   *int64   `json:"reasoning_tokens,omitempty"`
	// Model is the model that produced these counts. Tokens are neither
	// comparable nor priceable across models, so a rollup that sums them
	// without it is summing different units.
	Model *string `json:"model,omitempty"`
	// ThreadID is the provider-side thread or session the usage accrued on
	// — what makes "this workstream reused one warm session" measurable
	// rather than assumed. It is telemetry, not a resume handle:
	// InvocationResult.ContinuationRef is the handle the engine hands back
	// to a bridge, and neither field is derived from the other.
	ThreadID *string `json:"thread_id,omitempty"`
}

// ToEngine converts this §13.2 wire Usage block into the engine's own copy
// of it (engine.CompletionRequest.Usage, engine.Attempt.Usage), which is how
// it reaches storage: the engine cannot import this package (this package
// already imports engine, for the Completer interface and CompletionRequest
// its callback commit path uses), so the engine declares an independent
// Usage type and this is the one seam that translates into it. Every
// completion path that carries a Usage block converts through it —
// internal/worker/dispatch.go's synchronous completeFromResult (on an
// InvocationResult.Usage) and this package's own commitTerminal, by way of
// completionFor (on a CompletedPayload.Usage or a FailedPayload.Usage) — so
// a nil Usage always becomes a nil engine.Usage, never a fabricated zero
// block.
func (u *Usage) ToEngine() *engine.Usage {
	if u == nil {
		return nil
	}
	return &engine.Usage{
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		Cost:              u.Cost,
		Currency:          u.Currency,
		CachedInputTokens: u.CachedInputTokens,
		ReasoningTokens:   u.ReasoningTokens,
		Model:             u.Model,
		ThreadID:          u.ThreadID,
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
//
// WorkspaceMeasured is the bridge-measured workspace block (issue #33a,
// amending §13.2 the same additive way ADR 0008 amended it for usage on the
// failed event): `{measured, repo, reason, branch, head_before, head_after,
// status_porcelain, changed_files, diffstat}`, produced by the bridges' own
// workspace.py from git subprocesses the bridge itself ran. It is kept as
// raw JSON rather than a typed struct so the block round-trips exactly as
// the bridge reported it — a bridge that could not measure sends
// `measured:false` with every fact null, and that shape must survive to the
// node's output unaltered, never re-rendered as an empty diff. It is
// actor-reported data, not observed evidence: it rides inside the node's
// output (see MergeWorkspaceMeasured), and nothing on this path writes an
// `observed`-authority ledger record from it.
//
// TerminationReason is how the turn ended as the provider reported it
// ("max_output_tokens", a context-window stop, a cancellation, ...). It sits
// BESIDE Usage rather than inside it, and the attempt column it lands in is
// named `termination_reason` rather than `usage_termination_reason`, for one
// reason (ADR 0009): a turn can know why it ended while holding no parseable
// usage at all. Carrying the reason inside the §13.2 block would have forced
// a usage block — and therefore fabricated zero token counts — onto exactly
// those attempts, which is the fabrication migration 0012 and ADR 0008 were
// written to make impossible. Absent stays absent: an actor that does not
// report a reason omits the key and the column stays NULL.
type InvocationResult struct {
	Outcome           string          `json:"outcome"`
	Output            json.RawMessage `json:"output"`
	LedgerDelta       *LedgerDelta    `json:"ledger_delta"`
	ArtifactRefs      []string        `json:"artifact_refs"`
	ContinuationRef   *string         `json:"continuation_ref"`
	Usage             *Usage          `json:"usage"`
	TerminationReason *string         `json:"termination_reason,omitempty"`
	WorkspaceMeasured json.RawMessage `json:"workspace_measured,omitempty"`
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
	// ContinuationRef is InvocationResult's field, arriving on §13.4's
	// terminal event (ADR 0010 §2). Its absence here was the sharper half
	// of the gap that ADR closes: a long session is precisely the one that
	// answers asynchronously, so a handle only the synchronous body could
	// carry was unreachable exactly where continuation is worth most.
	//
	// FailedPayload deliberately has no twin of this field: a ref is a
	// claim that a resumable conversation exists, and a bridge reporting a
	// failed turn is the least reliable position from which to make it.
	ContinuationRef *string `json:"continuation_ref,omitempty"`
	// TerminationReason is InvocationResult's field, for the same reason an
	// actor that finished late reports the same Usage block: the answer is
	// the same kind of answer whichever path carried it.
	TerminationReason *string `json:"termination_reason,omitempty"`
	// WorkspaceMeasured carries the same bridge-measured block
	// InvocationResult does — an actor that finished late measured its
	// workspace exactly like one that finished inline.
	WorkspaceMeasured json.RawMessage `json:"workspace_measured,omitempty"`
}

// FailedPayload is the body of a `failed` event. Class is one of §13.5's
// error classes as the actor classified it; an unrecognized or absent class
// is treated as an execution failure, which is the honest default for "the
// actor ran and something went wrong" .
//
// Usage is optional (ADR 0008's amendment to §13.2): a bridge that still
// holds a terminal result when the work fails reports the tokens it burned;
// a crash or timeout that left no result omits the block entirely, and the
// attempt's usage stays NULL rather than a fabricated zero.
//
// WorkspaceMeasured is optional for the same reason Usage is: the bridges
// attach the block on every terminal branch — a session that failed may
// still have changed the workspace, and that fact is worth exactly as much
// on a failure as on a success. A bridge that could not measure sends
// `measured:false`; an actor that reports nothing omits the key.
//
// TerminationReason is optional and is emphatically not a duplicate of
// Class: the class is how the CONTROL PLANE classifies the failure (§13.5,
// one of a fixed set that decides retry and routing), while the reason is
// what the PROVIDER said about how the turn ended. A failed turn is the
// case where the two differ most — and, per ADR 0009, the case where a
// reason most often exists with no usage block to carry it.
type FailedPayload struct {
	Class             ErrorClass      `json:"class"`
	Message           string          `json:"message"`
	Detail            string          `json:"detail,omitempty"`
	Usage             *Usage          `json:"usage,omitempty"`
	TerminationReason *string         `json:"termination_reason,omitempty"`
	WorkspaceMeasured json.RawMessage `json:"workspace_measured,omitempty"`
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
