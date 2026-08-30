package actors

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// completionFor turns a terminal §13.4 event into the engine completion it
// claims. The second return value is non-empty when the payload cannot be
// acted on at all, in which case no completion is attempted; the third is an
// infrastructure failure that left the payload undecided, which is a
// compensated delivery failure rather than a verdict on the payload.
//
// Note what is *not* checked here: whether the outcome is one the node
// declares, and whether the output satisfies its schema. Both are the
// engine's job (§12.5 step 2), and duplicating them would create a second
// place for the answer to differ from the authoritative one.
func completionFor(
	ctx context.Context, store CallbackStore, inv PendingInvocation, ev CallbackEvent,
) (engine.CompletionRequest, string, error) {
	req := engine.CompletionRequest{
		WorkID:       inv.WorkID,
		WorkerID:     inv.WorkerID,
		FencingToken: inv.FencingToken,
		Attempt:      inv.Attempt,
		ActorID:      inv.ActorID,
	}

	switch ev.Kind {
	case EventCompleted, EventBlocked:
		var payload CompletedPayload
		if len(ev.Payload) > 0 {
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				return req, fmt.Sprintf("%s event %s carries a payload that is not a §13.2 result body: %v",
					ev.Kind, ev.EventID, err), nil
			}
		}
		outcome := payload.Outcome
		if outcome == "" && ev.Kind == EventBlocked {
			// §13.4 names `blocked` as an event kind; the domain outcome it
			// routes as is `blocked` unless the actor said otherwise. Whether
			// the node declares that outcome is the engine's call.
			outcome = "blocked"
		}
		if outcome == "" {
			return req, fmt.Sprintf("completed event %s declares no domain outcome", ev.EventID), nil
		}
		req.TechStatus = engine.StatusSucceeded
		req.Outcome = outcome
		req.Output = MergeWorkspaceMeasured(payload.Output, payload.WorkspaceMeasured)
		if payload.LedgerDelta != nil {
			req.LedgerDelta = append([]ledger.Record(nil), payload.LedgerDelta.Records...)
		}
		req.Usage = payload.Usage.ToEngine()
		req.TerminationReason = payload.TerminationReason
		req.ContinuationRef = payload.ContinuationRef
		detail, err := originActorMismatch(ctx, store, req.LedgerDelta, inv.ActorID)
		if err != nil {
			// Custody is undecided, not lost. Refusing here would commit
			// contract_rejected and set RetryRefusal on a completion the
			// store simply could not check, and no redelivery could ever
			// undo that (Qodo on PR #264).
			return req, "", err
		}
		if detail != "" {
			req.TechStatus = engine.StatusContractRejected
			req.Outcome = ""
			req.Output = identityDiagnostic(detail)
			req.LedgerDelta = nil
			req.RetryRefusal = detail
			return req, "", nil
		}
		// The handle for continuing this conversation, reported by an actor
		// that finished late. Persisting it here is what makes continuation
		// reachable on the path long sessions actually take (ADR 0010 §2);
		// absent stays absent, and nothing invents one.
		return req, "", nil

	case EventFailed:
		var payload FailedPayload
		if len(ev.Payload) > 0 {
			_ = json.Unmarshal(ev.Payload, &payload)
		}
		class := payload.Class
		if !class.Valid() {
			class = ClassExecution
		}
		req.TechStatus = TechStatusFor(class)
		if req.TechStatus == engine.StatusTimedOut {
			// A §13.4 terminal event is the actor reporting that ITS
			// invocation is over — the one timeout origin that leaves no
			// session to fence a retry against (task t10). Every other
			// producer of timed_out either is the control plane's own
			// wall-clock verdict or cannot say, and neither is retried.
			req.TimeoutOrigin = engine.TimeoutOriginActor
		}
		// The failure diagnostic and the workspace measurement are different
		// facts about the same attempt: the session failed, AND the bridge
		// measured (or honestly could not measure) what it left behind.
		req.Output = MergeWorkspaceMeasured(
			failureOutput(class, payload.Message, payload.Detail), payload.WorkspaceMeasured)
		req.Usage = payload.Usage.ToEngine()
		// The provider's own reason for ending the turn, which is not the
		// §13.5 class the control plane assigned it and can arrive with no
		// usage block at all (ADR 0009).
		req.TerminationReason = payload.TerminationReason
		// The bridge's own report of what preserve-on-failure did (task
		// t25/t26), nil unless it actually committed a branch — see
		// Preserve.ToEngine's gating.
		req.Preserve = payload.Preserve.ToEngine()
		return req, "", nil
	}

	return req, fmt.Sprintf("event kind %q is not terminal", ev.Kind), nil
}
