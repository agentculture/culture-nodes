package actors

import (
	"context"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// §13.4 lateness: what a terminal callback leaves when it arrives too late to
// commit (task t11, docs/adr/0012-late-callback-supersession.md).
//
// Split out of callback.go rather than living beside commitTerminal because
// that file is at this repo's 1000-line hard limit and this is a distinct
// concern with its own record: the refusal is callback.go's, the record is
// here.

// SupersedingAttempt is the attempt record a late terminal callback left:
// the appended row, and the row it corrects.
//
// Supersedes is empty when the report corrected nothing already recorded —
// §13.4's other flavour of lateness, where a newer worker reclaimed the work
// item and no row was ever written under this dispatch's fencing tuple. That
// is a different fact from "the correction failed", and the diagnostic event
// says which happened rather than showing an operator an empty string.
type SupersedingAttempt struct {
	AttemptID  string
	Number     int
	Supersedes string
}

// late records §13.4's late diagnostic and closes the invocation, and — task
// t11 — appends the attempt record the report itself deserves.
//
// The §13.4 refusal is unchanged: a completion that arrives after its attempt
// was replaced or cancelled still commits no workflow state, follows no edge,
// and moves no work item. What changed is that the refusal no longer implies
// silence. The commonest way to get here is a node run whose deadline expired
// while its actor session kept running: the scheduler recorded a `timed_out`
// attempt, the cancel reached the bridge, and the bridge is now reporting the
// tokens it burned, the model that burned them, why the turn ended, and the
// branch it preserved the work onto. Every one of those is a fact about an
// attempt that really happened. Before this they lived only in the body of
// the diagnostic event below, which is to say nowhere any reader of the
// attempts table — the API, the run detail page, the usage rollup, per-actor
// statistics — could see them.
//
// The correction is appended, never merged into the record it corrects (PRD
// §10.4), and the superseded row drops out of every aggregate as the
// correction lands, so one deadline reconciliation still describes one
// attempt rather than inflating the actor's retry burn. ADR 0012 has the
// reasoning; migrations/0028 has the column.
//
// The append is the one step here that can fail loudly. The diagnostic event
// and the invocation close are both best-effort audit/bookkeeping, but this
// is durable state, and losing it is exactly the gap the task exists to
// close — so a failure is recorded as a commit failure and returned, letting
// the caller compensate and the actor's redelivery write it once.
func (d CallbackDeps) late(
	ctx context.Context, inv PendingInvocation, ev CallbackEvent, req engine.CompletionRequest, diagnostic string,
) (CallbackResult, error) {
	// ev.EventID is the record's idempotency key beside inv.AttemptID (ADR
	// 0012 §5): this method is reached again whenever a redelivery follows a
	// pass that failed after the append — the CloseInvocation below is such a
	// step — and the key is what makes the second pass find the record rather
	// than write a second one.
	recorded, err := d.Store.RecordSupersedingAttempt(ctx, inv, ev.EventID, req)
	if err != nil {
		d.commitFailed(ctx, inv, ev, StageSupersede, err)
		return CallbackResult{}, err
	}

	d.recordDetail(ctx, inv, TypeCallbackLate, ev, diagnostic, map[string]any{
		"superseding_attempt_id":     recorded.AttemptID,
		"superseding_attempt_number": recorded.Number,
		// Empty when the report corrected nothing already recorded — see
		// SupersedingAttempt.Supersedes. Reported as a distinct key rather
		// than an empty string so an operator can tell the two apart.
		"supersedes":          recorded.Supersedes,
		"supersedes_recorded": recorded.Supersedes != "",
	})
	if err := d.Store.CloseInvocation(ctx, inv.AttemptID, InvocationSuperseded); err != nil {
		return CallbackResult{}, err
	}
	return CallbackResult{
		AttemptID:            inv.AttemptID,
		Disposition:          DispositionLate,
		Diagnostic:           diagnostic,
		SupersedingAttemptID: recorded.AttemptID,
	}, nil
}
