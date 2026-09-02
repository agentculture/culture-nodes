package telemetry

import "go.opentelemetry.io/otel/attribute"

// This file is the reviewable allowlist task t19 requires: the complete set
// of attribute keys any span or metric this package emits may carry, and
// the only functions that construct one. A reviewer auditing what this
// instrumentation could possibly leak reads this file and nothing else --
// every call site in internal/engine, internal/worker, and internal/actors
// builds its attributes exclusively through the functions below.
//
// Four categories, per task t19's acceptance criteria, and nothing else:
// ids (run, node, attempt, actor), enum states/outcomes, a count, and a
// duration. In particular, never a run's input payload, an instruction
// string, or ledger record content -- those never have a constructor here,
// so an instrumented call site has no typed way to attach one even by
// mistake. filterAllowed (below) is the second, structural half of the
// enforcement: an attribute.KeyValue built by hand instead of through one
// of these functions -- bypassing the allowlist on purpose or by
// accident -- is silently dropped before it reaches a span or a metric.

// The allowed attribute keys, one constant per category.
const (
	// KeyRunID identifies the run (engine.Run.ID).
	KeyRunID = attribute.Key("run_id")
	// KeyNodeID identifies the workflow node (Node.ID / the node a dispatch
	// or callback belongs to).
	KeyNodeID = attribute.Key("node_id")
	// KeyAttemptID identifies the §13.1 protocol attempt.
	KeyAttemptID = attribute.Key("attempt_id")
	// KeyActorID identifies the actor a completion or dispatch is
	// attributed to -- the registered actor id where one is known
	// (CompletionRequest.ActorID), or the node's `uses` reference at
	// dispatch time, before an attempt exists to carry an actor id at all.
	KeyActorID = attribute.Key("actor_id")

	// KeyTechStatus is the engine's own §3.4 technical status
	// (succeeded/failed/timed_out/cancelled/policy_denied/contract_rejected).
	KeyTechStatus = attribute.Key("tech_status")
	// KeyOutcome is the domain outcome a completion routed, when it routed
	// one -- the port name a node contract declares, never the payload
	// behind it.
	KeyOutcome = attribute.Key("outcome")
	// KeyNodeRunState is the node run's state after a transition commits.
	KeyNodeRunState = attribute.Key("node_run_state")
	// KeyRunState is the run's state after a transition commits.
	KeyRunState = attribute.Key("run_state")
	// KeyDisposition is a §13.4 callback event's CallbackDisposition
	// (recorded/duplicate/out_of_order/committed/late/rejected).
	KeyDisposition = attribute.Key("disposition")

	// KeyAttemptNumber is the 1-based attempt count for a node run.
	KeyAttemptNumber = attribute.Key("attempt_number")

	// KeyDurationMs is an explicit duration attribute on a span, alongside
	// the same value's own histogram recording -- so a trace UI can show it
	// without a separate metrics query. Operation.End sets this
	// automatically; instrumented call sites never need to construct it.
	KeyDurationMs = attribute.Key("duration_ms")
	// KeyAuthRefusalReason is the closed refusal class, never credential data.
	KeyAuthRefusalReason = attribute.Key("auth_refusal_reason")
)

// AllowedAttributeKeys is every key an instrumented call site, or this
// package itself, may attach to a span or a metric. filterAllowed enforces
// it on every Operation.Start and Operation.End call.
var AllowedAttributeKeys = []attribute.Key{
	KeyRunID, KeyNodeID, KeyAttemptID, KeyActorID,
	KeyTechStatus, KeyOutcome, KeyNodeRunState, KeyRunState, KeyDisposition,
	KeyAttemptNumber,
	KeyDurationMs,
	KeyAuthRefusalReason,
}

// RunID, NodeID, AttemptID, and ActorID are the id-category constructors.
// An empty id is passed through rather than omitted: at the point a seam
// starts, some ids are not resolved yet (see dispatch.go/callback.go's
// call sites), and an attribute present with an empty value is a more
// honest span than one silently missing the key altogether.
func RunID(v string) attribute.KeyValue     { return KeyRunID.String(v) }
func NodeID(v string) attribute.KeyValue    { return KeyNodeID.String(v) }
func AttemptID(v string) attribute.KeyValue { return KeyAttemptID.String(v) }
func ActorID(v string) attribute.KeyValue   { return KeyActorID.String(v) }

// TechStatus, Outcome, NodeRunState, RunState, and Disposition are the
// enum-category constructors. Each takes the stringer/string form of an
// already-closed enum (engine.TechStatus, the domain outcome name a
// contract declares, engine.NodeRunState, engine.RunState,
// actors.CallbackDisposition) -- never free text.
func TechStatus(v string) attribute.KeyValue   { return KeyTechStatus.String(v) }
func Outcome(v string) attribute.KeyValue      { return KeyOutcome.String(v) }
func NodeRunState(v string) attribute.KeyValue { return KeyNodeRunState.String(v) }
func RunState(v string) attribute.KeyValue     { return KeyRunState.String(v) }
func Disposition(v string) attribute.KeyValue  { return KeyDisposition.String(v) }

// AttemptNumber is the count-category constructor.
func AttemptNumber(n int) attribute.KeyValue { return KeyAttemptNumber.Int(n) }

// AuthRefusalReason is the closed refusal-class constructor (t8): the value is
// one of the principal middleware's reason classes, never a token or subject.
func AuthRefusalReason(v string) attribute.KeyValue { return KeyAuthRefusalReason.String(v) }

// durationMs is the duration-category constructor. It is unexported:
// Operation.End is the only caller, because a duration is measured by
// Start/End's own clock, never supplied by an instrumented call site.
func durationMs(ms float64) attribute.KeyValue { return KeyDurationMs.Float64(ms) }

// allowedSet backs filterAllowed with an O(1) membership test.
var allowedSet = func() map[attribute.Key]struct{} {
	m := make(map[attribute.Key]struct{}, len(AllowedAttributeKeys))
	for _, k := range AllowedAttributeKeys {
		m[k] = struct{}{}
	}
	return m
}()

// filterAllowed drops every attribute whose key is not in
// AllowedAttributeKeys, preserving the order of the ones that remain. It is
// applied to every attribute this package hands to a span or a metric,
// regardless of whether it arrived through a typed constructor above or
// was built by hand -- so the allowlist is enforced structurally, not by
// convention.
func filterAllowed(attrs []attribute.KeyValue) []attribute.KeyValue {
	if len(attrs) == 0 {
		return attrs
	}
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		if _, ok := allowedSet[a.Key]; ok {
			out = append(out, a)
		}
	}
	return out
}
