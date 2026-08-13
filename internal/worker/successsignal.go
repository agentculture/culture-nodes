package worker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners"
)

// Mechanical evaluation of a run's pre-announced success signals (task t18,
// issue #37; PRD §10.2's success_signal record, §10.10's mechanical checks).
//
// A success_signal is a proposed record — "here is the condition under which
// this work counts as done, and here is the mechanical check for it"
// (schemas/ledger/success_signal.schema.json: statement, check.kind,
// mechanical). This file runs at the same seam acceptance.go does — after a
// code dispatch's completion has committed, against the exact runners.Result
// that completion's own evidence was built from — and turns each evaluable
// signal into a derived, validator-origin evaluation record beside it.
//
// # What counts as evaluable
//
// Only a signal that says mechanical:true AND whose check.kind is in
// runners.MechanicallyEvaluableChecks. Both halves matter:
//
//   - mechanical:false is the author's own statement that the condition is
//     not machine-checkable yet, and the evaluator believes them — no
//     evaluation record is ever appended, whatever the check block says.
//     Render surfaces show such a signal as not-machine-checkable (see
//     web/src/domain/success-signal.ts); manufacturing a verdict for it
//     would be exactly the fabricated coverage the flag exists to prevent.
//   - a mechanical:true signal whose kind this build has no evaluator for is
//     also left honestly unevaluated. The gate is the same registry the
//     acceptance evaluator grows through (see internal/runners/acceptance.go
//     — process_exit and workspace_diff today), and the check payload is
//     evaluated through the same runners.EvaluateAcceptance dispatch, so a
//     kind added there becomes evaluable here with no change to this file.
//
// # Once, and never a promotion
//
// A signal is evaluated once — the first completion whose read finds it
// still unevaluated appends the verdict; later completions see the derived
// review referencing it and skip. (Two workers completing concurrently can
// in principle both pass that read; the duplicate is a benign, auditable
// derived record, each naming the evidence its own verdict was computed
// from, not a correctness problem — the same best-effort discipline every
// second write in this package accepts.)
//
// The signal record itself is NEVER touched: it stays exactly the proposed
// record its author appended, and the verdict is a separate record with
// SubjectRef and provenance pointing at it. Promotion to confirmed is a
// human review transaction (ledger.CommitReview) and nothing here goes near
// it — the same §10.4 discipline acceptance.go states for its own verdicts.
//
// A failure anywhere is reported through Options.OnError and never unwinds
// the already-committed completion.

// evaluateSuccessSignals evaluates the run's live, proposed, mechanically
// checkable success_signal records against res and appends one derived
// evaluation record per newly evaluated signal. A run with no success_signal
// records is untouched: the read finds nothing to evaluate and nothing is
// written.
func (w *Worker) evaluateSuccessSignals(ctx context.Context, res runners.Result, completion engine.CompletionResult) {
	records, err := w.ledger.Records(ctx, completion.RunID)
	if err != nil {
		w.report(fmt.Errorf("worker: read run %s records for success-signal evaluation: %w", completion.RunID, err))
		return
	}

	live := ledger.Live(records)
	evaluated := derivedReviewSubjects(live)
	evidenceID := evidenceRecordID(completion.LedgerRecords)

	for _, signal := range live {
		if signal.RecordType != ledger.RecordSuccessSignal || signal.Authority != ledger.AuthorityProposed {
			continue
		}
		if evaluated[signal.ID] {
			continue
		}
		data, err := signal.DataMap()
		if err != nil {
			w.report(fmt.Errorf("worker: decode success_signal %s: %w", signal.ID, err))
			continue
		}
		check, ok := mechanicallyCheckable(data)
		if !ok {
			// mechanical:false, or a kind this build cannot evaluate: the
			// signal stays honestly unevaluated — no record at all.
			continue
		}

		verdict := runners.EvaluateAcceptance([]map[string]any{check}, res)
		payload, err := json.Marshal(struct {
			Verdict   string                          `json:"verdict"`
			Reason    string                          `json:"reason"`
			SignalID  string                          `json:"signal_id"`
			Statement string                          `json:"statement,omitempty"`
			Checks    []runners.AcceptanceCheckResult `json:"checks"`
		}{
			Verdict:   acceptanceVerdictLabel(verdict.Passed),
			Reason:    fmt.Sprintf("mechanical evaluation of proposed success_signal %s (PRD §10.2, §10.10)", signal.ID),
			SignalID:  signal.ID,
			Statement: dataString(data, "statement"),
			Checks:    verdict.Checks,
		})
		if err != nil {
			w.report(fmt.Errorf("worker: encode success-signal evaluation for %s: %w", signal.ID, err))
			continue
		}

		provenance := []string{signal.ID}
		if evidenceID != "" {
			provenance = append(provenance, evidenceID)
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
			Authority:      ledger.AuthorityDerived,
			SubjectRef:     ledger.NullableID(signal.ID),
			ProvenanceRefs: provenance,
			Data:           payload,
		}
		if _, err := w.ledger.Append(ctx, record); err != nil {
			w.report(fmt.Errorf("worker: append success-signal evaluation for %s: %w", signal.ID, err))
		}
	}
}

// mechanicallyCheckable reports whether a success_signal payload asks for a
// mechanical check this build can actually run, returning the check block to
// evaluate when it does. The kind gate is runners.MechanicallyEvaluableChecks
// — the acceptance check registry — so new kinds plug in there, not here.
func mechanicallyCheckable(data map[string]any) (map[string]any, bool) {
	if !dataBool(data, "mechanical") {
		return nil, false
	}
	check, _ := data["check"].(map[string]any)
	kind, _ := check["kind"].(string)
	if !runners.MechanicallyEvaluableChecks[runners.AcceptanceCheckKind(kind)] {
		return nil, false
	}
	return check, true
}

// derivedReviewSubjects returns the ids every live derived review record
// points at through SubjectRef — for a success_signal, the mark that it has
// already been evaluated.
func derivedReviewSubjects(live []ledger.Record) map[string]bool {
	out := make(map[string]bool)
	for _, rec := range live {
		if rec.RecordType != ledger.RecordReview || rec.Authority != ledger.AuthorityDerived {
			continue
		}
		if id := rec.SubjectRef.String(); id != "" {
			out[id] = true
		}
	}
	return out
}

// dataString and dataBool mirror internal/ledger's unexported helpers of the
// same names: read a top-level payload field, defaulting to the zero value
// when it is absent or mistyped — a loose Phase-0 payload is data to read
// defensively, not to error on.
func dataString(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

func dataBool(data map[string]any, key string) bool {
	if v, ok := data[key].(bool); ok {
		return v
	}
	return false
}
