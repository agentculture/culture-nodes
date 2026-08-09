package worker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners"
)

// Mechanical acceptance.requires evaluation for a code node's own dispatch
// (task t17, PRD §10.10, docs/acceptance.md criterion 14).
//
// internal/runners.EvaluateAcceptance is the pure evaluator; this file is
// its one wiring point today: after a code node's completion has already
// committed (dispatchCode's success path in code.go), if the node declares
// `acceptance.requires`, its verdict is computed against the exact
// runners.Result the completion's own evidence was built from, and appended
// as a second, best-effort ledger record — the same "second write after an
// already-committed completion" shape hooks.go's appendAssuranceRejection
// already uses for post_run (see that file's package doc for the fuller
// argument: engine.CompleteAttempt's own delta preparation refuses a
// derived-authority record outright, so a derived record can only be
// appended through the ledger directly).
//
// # What this does not do
//
// It never changes the node's own routed domain outcome or technical
// status — evaluate runs strictly after w.complete has already committed
// one. It also does not, today, correlate the verdict with any `task`
// record's assurance_state: this reference workflow's code nodes propose no
// task records to correlate with (they only observe evidence), so "verifies
// or rejects the task" in the PRD's own prose (§24) is answered here at the
// level of the evidence a code node's dispatch produced, not by rewriting a
// task's lifecycle field. Closing that further gap is open work, named in
// docs/acceptance.md rather than silently assumed done.
//
// A failure to append is reported through Options.OnError, never allowed to
// unwind the already-committed completion — the same discipline every
// best-effort write in this package follows.

// evaluateAcceptance mechanically evaluates node's declared acceptance
// checks against res and appends the verdict as a derived review record. It
// is a no-op when the node declares no acceptance block.
func (w *Worker) evaluateAcceptance(ctx context.Context, node *nodeSpec, res runners.Result, completion engine.CompletionResult) {
	if node.Acceptance == nil || len(node.Acceptance.Requires) == 0 {
		return
	}

	verdict := runners.EvaluateAcceptance(node.Acceptance.Requires, res)
	evidenceID := evidenceRecordID(completion.LedgerRecords)

	payload, err := json.Marshal(struct {
		Verdict string                          `json:"verdict"`
		Reason  string                          `json:"reason"`
		Checks  []runners.AcceptanceCheckResult `json:"checks"`
	}{
		Verdict: acceptanceVerdictLabel(verdict.Passed),
		Reason:  fmt.Sprintf("mechanical evaluation of node %q's declared acceptance.requires (PRD §10.10)", node.ID),
		Checks:  verdict.Checks,
	})
	if err != nil {
		w.report(fmt.Errorf("worker: encode acceptance verdict payload for attempt %s: %w", completion.AttemptID, err))
		return
	}

	record := ledger.Record{
		RecordType: ledger.RecordReview,
		RunID:      completion.RunID,
		NodeRunID:  ledger.NullableID(completion.NodeRunID),
		AttemptID:  ledger.NullableID(completion.AttemptID),
		Origin: ledger.Origin{
			Kind:    ledger.OriginValidator,
			ActorID: w.codeRunnerActorID(),
		},
		Authority:  ledger.AuthorityDerived,
		SubjectRef: ledger.NullableID(evidenceID),
		Data:       payload,
	}
	if evidenceID != "" {
		record.ProvenanceRefs = []string{evidenceID}
	}

	if _, err := w.ledger.Append(ctx, record); err != nil {
		w.report(fmt.Errorf("worker: append acceptance verdict for attempt %s: %w", completion.AttemptID, err))
	}
}

// evidenceRecordID returns the id of the (at most one) evidence record a
// code node's completion appended, or "" if it appended none — a refused
// dispatch, or a node with no `observe` permission at all.
func evidenceRecordID(records []ledger.Record) string {
	for _, rec := range records {
		if rec.RecordType == ledger.RecordEvidence {
			return rec.ID
		}
	}
	return ""
}

// acceptanceVerdictLabel mirrors CommitReview's own verdict vocabulary
// (ledger.VerdictConfirm / ledger.VerdictReject) as the string this
// payload's `verdict` field carries. It is a plain string rather than
// ledger.Verdict itself because this is a derived mechanical computation,
// not a human review transaction, and the two must not be confused at the
// type level.
func acceptanceVerdictLabel(passed bool) string {
	if passed {
		return "confirm"
	}
	return "reject"
}
