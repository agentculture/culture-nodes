package actors

import (
	"context"
	"encoding/json"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/handover"
)

// The asynchronous half of the handover-evidence seam (task t10, issue #13).
// Its synchronous twin is internal/worker/handover.go, and the argument for
// both lives in internal/handover's package doc.
//
// This half is the one that matters in practice: every bridge under adapters/
// is deployed `always_async`, so a `completed` callback event is how a real
// dispatch's handover block reaches the control plane at all.
//
// It sits deliberately AFTER the §12.5 completion transaction and after the
// invocation row is closed, and its failure is never propagated. An
// observation is a second, independent fact about an attempt whose outcome is
// already durable; letting a fetch that could not happen turn a committed
// completion into an error the actor redelivers would trade a missing
// evidence row for a re-run.

// claimedHandoverRef reads the ONE field the control plane takes from a
// terminal event's handover block: the name of the ref to go and look for.
//
// A payload that carries no block, or a block reporting that no ref was
// created, yields no claim — which is the overwhelmingly common case, since
// only a dispatch that explicitly asked for a handover produces one.
func claimedHandoverRef(ev CallbackEvent) (string, bool) {
	if ev.Kind != EventCompleted || len(ev.Payload) == 0 {
		return "", false
	}
	var payload struct {
		Handover *Handover `json:"handover"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return "", false
	}
	return payload.Handover.ClaimedRef()
}

// observeHandover measures a claimed ref and records what it measured, on a
// completion that has already committed. It writes nothing at all when there
// is no claim, no observer, or no fetchable ref.
//
// The identifiers come from the engine's own CompletionResult rather than
// from PendingInvocation: ledger_records.attempt_id has a foreign key to
// attempts(id), and the attempt row a late completion writes is the engine's
// to name — the parked invocation's own attempt id is the handle the actor
// was dispatched under, which is not the same thing.
func (d CallbackDeps) observeHandover(ctx context.Context, completion engine.CompletionResult, ev CallbackEvent) {
	if d.Handover == nil || completion.AttemptID == "" {
		return
	}
	ref, ok := claimedHandoverRef(ev)
	if !ok {
		return
	}
	d.Handover.Observe(ctx, handover.Claim{
		RunID:     completion.RunID,
		NodeRunID: completion.NodeRunID,
		AttemptID: completion.AttemptID,
		Ref:       ref,
	})
}
