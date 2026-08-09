package runners

import (
	"strings"
	"time"
)

// This file is the runner-service wire contract in Go: the paths, headers and
// envelopes an implementation must speak. The prose specification is
// api/runner-protocol/README.md, and protocol_test.go asserts the two agree —
// a constant changed here without the document changing fails the build.
//
// What is deliberately *not* here: the operation and the result. Those are
// schemas/runner/{operation,result}.schema.json, already runner-neutral, and
// they cross the wire verbatim. The envelopes below add only what HTTP needs
// to correlate a request with an answer — which operation, and whether it has
// finished. Nothing in this file may restate, extend, or narrow a claim the
// schemas already make.
//
// # Why the protocol is asynchronous only
//
// Execution happens in another process, usually on another machine, possibly
// for ten minutes. A worker that held an HTTP connection open for the whole
// operation would hold a lease and a goroutine with it, and the runtime's
// cost would then scale with how long work takes rather than with how many
// runners exist. So dispatch answers 202 and returns; the worker parks the
// item as waiting_external and learns the outcome by sampling the status
// endpoint. Polling is the runtime's responsibility and is sufficient on its
// own. A completion callback is strictly optional — it tightens latency and
// nothing else, and a runner that never calls back is fully conformant.

// ProtocolVersion is the runner-protocol version a caller declares in
// ProtocolVersionHeader. It is a string, not a number, matching the actor
// protocol: a protocol version is an identifier, not an arithmetic quantity.
const ProtocolVersion = "1.0"

// OperationsPath is the path an operation is submitted to, appended to a
// registered service endpoint. Status and cancellation hang off it by
// operation id.
const OperationsPath = "/v1/operations"

// The headers the protocol fixes. Everything the runner needs that the
// operation document does not carry rides here, because the operation schema
// sets additionalProperties:false and crosses the wire verbatim — there is no
// room in the body for transport concerns, and inventing one would fork the
// schema.
const (
	// AuthorizationHeader carries the caller's credential as
	// "Bearer <secret>". It is mandatory on every request, including status
	// reads: a runner service that answers an unauthenticated request is a
	// remote-code-execution surface for anyone who can reach it.
	AuthorizationHeader = "Authorization"
	// IdempotencyKeyHeader carries the operation id, which the operation
	// schema already names as "the idempotency key for dispatch". Re-sending
	// the same key must return the acceptance the runner already issued, not
	// start the work a second time.
	IdempotencyKeyHeader = "Idempotency-Key"
	// ProtocolVersionHeader declares the version of this contract the caller
	// speaks. A runner that does not implement the declared major version
	// refuses the operation rather than guessing.
	ProtocolVersionHeader = "Nodes-Protocol-Version"
	// CallbackURLHeader offers a URL the runner may POST a
	// CallbackNotification to when an operation reaches a terminal state.
	// Optional in both directions: the caller need not send it and the runner
	// need not honour it.
	CallbackURLHeader = "Nodes-Callback-Url"
	// CallbackTokenHeader carries the bearer token the runner must present on
	// that callback, so the control plane can refuse a forged notification.
	CallbackTokenHeader = "Nodes-Callback-Token"
	// TraceparentHeader carries W3C trace context, as the actor protocol does.
	TraceparentHeader = "Traceparent"
)

// The embedded schema paths that are this protocol's wire payloads. They are
// stated as constants so the protocol document, the code, and the files the
// binary actually embeds are one fact; protocol_test.go proves all three
// agree and that these equal internal/contracts' schema identifiers.
const (
	// OperationSchemaPath is the request body of an execute call, verbatim.
	OperationSchemaPath = "runner/operation.schema.json"
	// ResultSchemaPath is the document a terminal status carries, verbatim.
	ResultSchemaPath = "runner/result.schema.json"
)

// Protocol timing defaults. A deployment tuning sampling behaviour should be
// changing something with a name rather than a number found by grep.
const (
	// DefaultPollInterval is how often the runtime samples an operation's
	// status when the runner declares no preference of its own. Sampling load
	// scales with runners and interval, never with how long an operation runs.
	DefaultPollInterval = 5 * time.Second
	// MinStatusRetention is the shortest terminal-status retention this
	// protocol accepts. A runner that forgets an operation the moment it
	// finishes cannot be polled for the outcome, which would make the
	// authoritative completion path unusable — so a shorter declared
	// retention is refused at dispatch instead of discovered at the first
	// missed completion.
	MinStatusRetention = time.Hour
)

// Two operation states the *status* envelope adds to the five a result
// document declares. They exist only in transit: a result never carries them,
// and OperationStatus.Validate refuses a status that pairs one with a result.
const (
	// StateAccepted is "the runner has taken the operation and has not
	// started it yet" — the state an acceptance implies.
	StateAccepted State = "accepted"
	// StateRunning is "the operation is executing". It carries no claim about
	// progress: the runner is reporting that it holds the work, not how far
	// along it is.
	StateRunning State = "running"
)

// Terminal reports whether a state ends an operation. The terminal states are
// exactly the five schemas/runner/result.schema.json admits, which is what
// makes "the envelope's state equals the result's state" checkable.
func (s State) Terminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateTimedOut, StateCancelled, StateRejected:
		return true
	default:
		return false
	}
}

// Valid reports whether a state is one the status envelope admits: the five
// terminal states plus accepted and running.
func (s State) Valid() bool {
	return s.Terminal() || s == StateAccepted || s == StateRunning
}

// Acceptance is the HTTP 202 body an execute call returns. It is an
// acknowledgement, not an answer: it says the runner holds the operation and
// tells the caller how to ask about it later.
type Acceptance struct {
	// OperationID echoes the operation the runner accepted. A mismatch is a
	// contract failure, not a detail: the caller would otherwise poll a
	// status it never dispatched.
	OperationID string `json:"operation_id"`
	// PollAfterSeconds is the runner's requested minimum sampling interval.
	// Advisory — the cadence is the runtime's decision — but a runtime that
	// samples faster than a runner asked for is generating load the runner
	// said it did not want. Zero means "no preference".
	PollAfterSeconds int `json:"poll_after_seconds,omitempty"`
	// StatusRetentionSeconds is how long the runner promises to keep serving
	// this operation's terminal status after it finishes. It is a promise
	// about the completion path itself: forget sooner and the outcome becomes
	// unlearnable. Zero means the protocol minimum.
	StatusRetentionSeconds int `json:"status_retention_seconds,omitempty"`
	// SupportsCancellation declares whether the cancel path is implemented.
	// Cancellation is best-effort and durable in the control plane either
	// way, so this changes what the runtime attempts, never what it records.
	SupportsCancellation bool `json:"supports_cancellation"`
	// SupportsCallback declares whether the runner will POST a completion
	// notification when offered a callback URL. It is advisory: the runtime
	// polls regardless, and a false here costs latency, never correctness.
	SupportsCallback bool `json:"supports_callback"`
}

// Validate checks an acceptance against the operation it should be
// acknowledging. A malformed acceptance is a contract failure: the runner may
// well be executing something, but the caller cannot honestly say what.
func (a Acceptance) Validate(operationID string) error {
	switch {
	case a.OperationID == "":
		return refuse(ErrorContractFailure, nil, operationID, "",
			"the runner's 202 acceptance declares no operation id; there is nothing to sample status for")
	case operationID != "" && a.OperationID != operationID:
		return refuse(ErrorContractFailure, nil, operationID, "",
			"the runner accepted a different operation ("+a.OperationID+"); "+
				"polling it would report on work this attempt did not dispatch")
	case a.PollAfterSeconds < 0:
		return refuse(ErrorContractFailure, nil, operationID, "",
			"the acceptance declares a negative poll_after_seconds")
	case a.StatusRetentionSeconds < 0:
		return refuse(ErrorContractFailure, nil, operationID, "",
			"the acceptance declares a negative status_retention_seconds")
	case a.StatusRetentionSeconds > 0 && time.Duration(a.StatusRetentionSeconds)*time.Second < MinStatusRetention:
		return refuse(ErrorContractFailure, nil, operationID, "",
			"the acceptance declares a status_retention_seconds shorter than the protocol minimum of "+
				MinStatusRetention.String()+"; a runner that forgets an operation before it can be sampled "+
				"cannot report its completion")
	}
	return nil
}

// PollInterval is the sampling interval this acceptance asks for, or the
// protocol default when it asks for nothing.
func (a Acceptance) PollInterval() time.Duration {
	if a.PollAfterSeconds > 0 {
		return time.Duration(a.PollAfterSeconds) * time.Second
	}
	return DefaultPollInterval
}

// StatusRetention is how long the runner promised to serve this operation's
// terminal status, or the protocol minimum when it promised nothing.
func (a Acceptance) StatusRetention() time.Duration {
	if a.StatusRetentionSeconds > 0 {
		return time.Duration(a.StatusRetentionSeconds) * time.Second
	}
	return MinStatusRetention
}

// OperationStatus is the HTTP 200 body of a status read: which operation,
// what state, and — once and only once it is terminal — the result document.
//
// The envelope is deliberately this thin. Everything a caller may conclude
// about an execution comes from the embedded Result, which is the schema's
// document verbatim, complete with its per-observation honesty declarations.
// The envelope adds no claim of its own.
type OperationStatus struct {
	OperationID string `json:"operation_id"`
	State       State  `json:"state"`
	// Result is present exactly when State is terminal. Absent is not
	// "unknown yet, assume failure": it is "this operation has not finished",
	// and the runtime keeps sampling.
	Result *Result `json:"result,omitempty"`
}

// Terminal reports whether the operation has finished.
func (s OperationStatus) Terminal() bool { return s.State.Terminal() }

// Validate checks a status envelope for the invariants a caller relies on
// before it reads anything out of it.
//
// Every failure here is ErrorContractFailure rather than a failed Result, and
// that is the whole point: a status the runtime cannot parse tells it nothing
// about the execution, so it has nothing honest to record about it. Inventing
// a "failed" result from a malformed status would put an unmeasured claim in
// the ledger.
func (s OperationStatus) Validate() error {
	if s.OperationID == "" {
		return refuse(ErrorContractFailure, nil, "", "",
			"the runner's status declares no operation id")
	}
	if !s.State.Valid() {
		return refuse(ErrorContractFailure, nil, s.OperationID, "",
			"state "+string(s.State)+" is not a runner-protocol operation state "+
				"(accepted, running, or one of the result schema's terminal states)")
	}
	if !s.Terminal() {
		if s.Result != nil {
			return refuse(ErrorContractFailure, nil, s.OperationID, "",
				"the status reports state "+string(s.State)+" yet carries a result document; "+
					"an operation that has not finished has nothing to report")
		}
		return nil
	}
	switch {
	case s.Result == nil:
		return refuse(ErrorContractFailure, nil, s.OperationID, "",
			"the status reports the terminal state "+string(s.State)+" but carries no result document; "+
				"a completion without evidence is a claim, not a result")
	case s.Result.State != s.State:
		return refuse(ErrorContractFailure, nil, s.OperationID, "",
			"the status envelope says "+string(s.State)+" while the result document it carries says "+
				string(s.Result.State)+"; the envelope disagrees with its own evidence")
	case s.Result.OperationID != "" && s.Result.OperationID != s.OperationID:
		return refuse(ErrorContractFailure, nil, s.OperationID, "",
			"the status carries a result for a different operation ("+s.Result.OperationID+")")
	}
	return nil
}

// CallbackNotification is the body of the optional completion callback.
//
// It carries no result, and that is deliberate. The callback is a latency
// hint: on receiving one the runtime samples the status endpoint it
// dispatched to, over the connection it authenticated, and learns the outcome
// there. Nothing a callback says is ever committed on the callback's word, so
// a forged or replayed notification costs at most one extra status read.
type CallbackNotification struct {
	OperationID string `json:"operation_id"`
	// State is advisory context for logs. The runtime re-reads the authoritative
	// state from the status endpoint regardless of what this says.
	State State `json:"state,omitempty"`
}

// ExecuteURL is where an operation is submitted for this identity.
//
// A registered endpoint may carry a base path (a runner mounted behind a
// gateway prefix), so the protocol path is appended rather than substituted.
// An endpoint that already ends in OperationsPath is used as-is, so an
// operator who registered the full submit URL is not punished for it.
func (s ServiceIdentity) ExecuteURL() string {
	base := strings.TrimRight(s.Endpoint, "/")
	if strings.HasSuffix(base, OperationsPath) {
		return base
	}
	return base + OperationsPath
}

// StatusURL is where this identity's operation status is sampled.
func (s ServiceIdentity) StatusURL(operationID string) string {
	return s.ExecuteURL() + "/" + operationID
}

// CancelURL is where a best-effort cancellation is requested. The runtime's
// own cancellation is already durable by the time this is called, so a runner
// that does not implement the path changes nothing about the run.
func (s ServiceIdentity) CancelURL(operationID string) string {
	return s.StatusURL(operationID) + "/cancel"
}
