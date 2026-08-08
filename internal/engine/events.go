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
