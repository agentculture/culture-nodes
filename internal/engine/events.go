package engine

import (
	"encoding/json"

	"github.com/agentculture/culture-nodes/internal/events"
)

// Event types the engine emits.
//
// The ones PRD §15.1 names are reused from internal/events. The rest are new
// types this task introduces, declared here rather than in internal/events
// because §15.1's list is explicitly illustrative and these are the engine's
// vocabulary: a package that does not run workflows has no use for
// "run.bounded". They carry the required "dev.culture.nodes." prefix, so the
// envelope and the relay accept them like any other.
const (
	TypeRunCreated        = events.TypeRunCreated
	TypeNodeRunReady      = events.TypeNodeRunReady
	TypeAttemptCompleted  = events.TypeAttemptCompleted
	TypeContractRejected  = events.TypeContractRejected
	TypeLedgerAppended    = events.TypeLedgerRecordAppended
	TypeTokenTransitioned = events.TypeTokenTransitioned
	TypeRunCompleted      = events.TypeRunCompleted

	// TypeNodeRunFailed records a node run that will not be attempted again.
	TypeNodeRunFailed = "dev.culture.nodes.node-run.failed"
	// TypeAttemptRetryScheduled records a technical failure that bought
	// another attempt, with the time the re-enqueued work becomes claimable.
	TypeAttemptRetryScheduled = "dev.culture.nodes.attempt.retry-scheduled"
	// TypeRunFailed records a run that ended without producing its result.
	TypeRunFailed = "dev.culture.nodes.run.failed"
	// TypeRunCancelled records a run stopped on instruction. It is separate
	// from run.failed because cancellation is not a fault, and a consumer
	// counting failures should not have to read a payload to tell them apart.
	TypeRunCancelled = "dev.culture.nodes.run.cancelled"
	// TypeRunBounded records a run the engine stopped at a §9.7 loop bound.
	// It is a distinct type from run.failed on purpose: "this workflow is
	// looping" is an operational answer, and finding it inside a generic
	// failure event would mean parsing a message to learn it.
	TypeRunBounded = "dev.culture.nodes.run.bounded"
	// TypeHumanTaskCreated records an approval node's dispatch (PRD §9.9). It
	// is emitted instead of node-run.ready: unlike that event, this one names
	// no claimable work, and a consumer that treated it as one would be
	// signaling a work item that was never created.
	TypeHumanTaskCreated = "dev.culture.nodes.human-task.created"
	// TypeHumanTaskDecided records DecideHumanTask resolving a paused human
	// task: who decided, what outcome, and which review recorded it as
	// human authority in the ledger (PRD §9.9, §10.8).
	TypeHumanTaskDecided = "dev.culture.nodes.human-task.decided"

	// TypeTokenSplit records one parallel-node fan-out (issue #43): the
	// token group, its discovered cardinality, and the eligible edge list.
	// The per-branch token.transitioned/node-run.ready events follow it.
	TypeTokenSplit = "dev.culture.nodes.token.split"
	// TypeJoinArrived records one branch reaching a join barrier, with the
	// running arrival count against the group's cardinality.
	TypeJoinArrived = "dev.culture.nodes.join.arrived"
	// TypeEventPickedUp records an event route creating a token at its
	// target node (issue #43, design D9): which route, which delivered fact,
	// and the token/node run it produced. It is the provenance an event-born
	// token has instead of a parent token (review finding D4) — together with
	// tokens.origin_event_id, which carries the same fact as durable state
	// rather than only as an audit line.
	TypeEventPickedUp = "dev.culture.nodes.event.picked-up"
	// TypeEventPickupRefused records a route that matched but created no
	// token: its guard declined, or the pickup would have taken the run past
	// a §9.7 bound. It is deliberately NOT a run failure (design D13) — an
	// external event must not fail a healthy run — so this event is the only
	// trace, and an operator watching the run is meant to find it here.
	TypeEventPickupRefused = "dev.culture.nodes.event.pickup-refused"
	// TypeBranchCancelled records one sibling branch reaped explicitly —
	// the loser of an any/quorum barrier, or a live branch retired because
	// a terminal branch failure (or cancellation, or a bound) ended the run.
	TypeBranchCancelled = "dev.culture.nodes.branch.cancelled"
)

// event builds an EventInput, encoding data into the payload. Encoding cannot
// fail for the maps the engine builds — they hold strings, numbers, and
// json.RawMessage — so a failure degrades to an empty payload rather than
// aborting a committed transition over an audit detail.
//
// Every payload carries node_run_id when there is one: the outbox relay reads
// that field to build the queue.WorkRef it publishes, so an event about a
// node run that omitted it would produce a signal nobody could act on.
func event(eventType string, data map[string]any) EventInput {
	payload, err := json.Marshal(data)
	if err != nil {
		payload = json.RawMessage(`{}`)
	}
	return EventInput{Type: eventType, Data: payload}
}
