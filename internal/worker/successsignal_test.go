package worker_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners"
)

// Success-signal evaluation (task t18, issue #37; PRD §10.2's success_signal
// record, §10.10's mechanical checks).
//
// A success_signal is a pre-announced acceptance condition proposed into the
// run's ledger. The evaluator under test here
// (internal/worker/successsignal.go) runs at the same seam the mechanical
// acceptance verdict does — after a code dispatch's completion has committed
// — and its contract has three legs the three tests below pin down:
//
//  1. a mechanical:true signal whose check kind this build can evaluate gets
//     exactly one derived, validator-origin evaluation record with
//     provenance back to the signal (and to the evidence the verdict was
//     computed against), even when several completions follow it;
//  2. a mechanical:false signal gets NO evaluation record — it is honestly
//     not machine-checkable, and saying so is the whole point of the flag;
//  3. the signal record itself is never promoted: it stays exactly the
//     proposed record its author appended, and the verdict lives in a
//     separate derived record referencing it (PRD §10.4 — no actor, and no
//     validator either, rewrites a proposal).
//
// Like code_test.go, everything runs against a real PostgreSQL, the real
// engine, and the real Worker loop; the scripted runner is the only fake.

// proposeSuccessSignal appends a proposed, agent-origin success_signal to the
// run — the same envelope an actor's completion delta would land it under.
// The tests call it from inside the scripted runner so the record is
// committed before the dispatch's own completion, which is when the
// evaluator reads the run. It reports through t.Errorf, not t.Fatalf,
// because it runs on the worker's goroutine.
func proposeSuccessSignal(t *testing.T, h *codeHarness, runID, data string) {
	t.Helper()
	_, err := h.ledgerRun.Append(context.Background(), ledger.Record{
		RecordType: ledger.RecordSuccessSignal,
		RunID:      runID,
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: h.runnerID},
		Authority:  ledger.AuthorityProposed,
		Data:       json.RawMessage(data),
	})
	if err != nil {
		t.Errorf("append success_signal: %v", err)
	}
}

func (h *codeHarness) successSignalRecords(runID string) []ledger.Record {
	h.t.Helper()
	var out []ledger.Record
	for _, rec := range h.records(runID) {
		if rec.RecordType == ledger.RecordSuccessSignal {
			out = append(out, rec)
		}
	}
	return out
}

func containsRef(refs []string, want string) bool {
	for _, ref := range refs {
		if ref == want {
			return true
		}
	}
	return false
}

// Acceptance (1) and the once-only discipline: a mechanical process_exit
// signal proposed during the first code node's dispatch is evaluated at that
// node's completion — one derived, validator-origin record with provenance
// to the signal and the evidence — and the second code node's completion
// finds it already evaluated and appends nothing.
func TestMechanicalSuccessSignalGetsOneDerivedEvaluationRecord(t *testing.T) {
	var h *codeHarness
	h = newCodeHarness(t, func(op runners.Operation, call int) (runners.Result, error) {
		if call == 1 {
			proposeSuccessSignal(t, h, op.Context.RunID,
				`{"statement":"the test process exits 0","check":{"kind":"process_exit","equals":0},"mechanical":true}`)
		}
		return codeRunResult(op, 0), nil
	})

	run := h.createRun("code-success-signals.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}

	signals := h.successSignalRecords(run.ID)
	if len(signals) != 1 {
		t.Fatalf("run has %d success_signal records, want the 1 the test proposed", len(signals))
	}
	signal := signals[0]

	// Acceptance (3): the proposal itself is never promoted — the evaluator
	// appends its verdict beside the signal, it does not touch it.
	if signal.Authority != ledger.AuthorityProposed {
		t.Errorf("success_signal authority = %q, want proposed: the evaluator must never promote the proposal itself", signal.Authority)
	}

	// Exactly one evaluation across BOTH code completions: the verify node's
	// completion must find the signal already evaluated and append nothing.
	reviews := h.reviewRecords(run.ID)
	if len(reviews) != 1 {
		t.Fatalf("run has %d review records, want exactly one signal evaluation across both code completions", len(reviews))
	}
	eval := reviews[0]

	if eval.Authority != ledger.AuthorityDerived {
		t.Errorf("evaluation authority = %q, want derived (a mechanical computation, not a human decision)", eval.Authority)
	}
	if eval.Origin.Kind != ledger.OriginValidator {
		t.Errorf("evaluation origin kind = %q, want validator", eval.Origin.Kind)
	}
	if eval.Origin.ActorID != h.runnerID {
		t.Errorf("evaluation origin actor = %q, want the registered code-runner identity %q", eval.Origin.ActorID, h.runnerID)
	}
	if eval.SubjectRef.String() != signal.ID {
		t.Errorf("evaluation subject_ref = %q, want the signal record %q it evaluates", eval.SubjectRef.String(), signal.ID)
	}
	if !containsRef(eval.ProvenanceRefs, signal.ID) {
		t.Errorf("evaluation provenance = %v, want it to name the signal record %q", eval.ProvenanceRefs, signal.ID)
	}
	linkedEvidence := false
	for _, ev := range h.evidenceRecords(run.ID) {
		if containsRef(eval.ProvenanceRefs, ev.ID) {
			linkedEvidence = true
		}
	}
	if !linkedEvidence {
		t.Errorf("evaluation provenance = %v, want it to also name the evidence record the verdict was computed against", eval.ProvenanceRefs)
	}

	data, err := eval.DataMap()
	if err != nil {
		t.Fatalf("decode evaluation payload: %v", err)
	}
	if data["verdict"] != "confirm" {
		t.Errorf("evaluation payload = %v, want verdict=confirm for a matching exit code", data)
	}
	if data["signal_id"] != signal.ID {
		t.Errorf("evaluation payload signal_id = %v, want %q", data["signal_id"], signal.ID)
	}
	if data["statement"] != "the test process exits 0" {
		t.Errorf("evaluation payload statement = %v, want the signal's own statement carried verbatim", data["statement"])
	}
	checks, _ := data["checks"].([]any)
	if len(checks) != 1 {
		t.Fatalf("evaluation checks = %v, want exactly one (process_exit)", data["checks"])
	}
	check, _ := checks[0].(map[string]any)
	if check["kind"] != "process_exit" || check["passed"] != true || check["evaluated"] != true {
		t.Errorf("evaluation's process_exit check = %v, want kind=process_exit passed=true evaluated=true", check)
	}
}

// Acceptance (2): a mechanical:false signal gets NO evaluation record — not
// a failing one, not an "unevaluated" one, none. The flag is the author's own
// statement that the condition is not machine-checkable yet, and the honest
// runtime response is to leave it visibly unevaluated (render surfaces show
// it as not-machine-checkable) rather than to manufacture a verdict.
func TestNonMechanicalSuccessSignalGetsNoEvaluationRecord(t *testing.T) {
	var h *codeHarness
	h = newCodeHarness(t, func(op runners.Operation, call int) (runners.Result, error) {
		if call == 1 {
			// Even with an otherwise-evaluable check kind attached, the
			// mechanical:false flag wins: the author said this is not
			// machine-checkable, and the evaluator believes them.
			proposeSuccessSignal(t, h, op.Context.RunID,
				`{"statement":"the change reads well to a reviewer","check":{"kind":"process_exit","equals":0},"mechanical":false}`)
		}
		return codeRunResult(op, 0), nil
	})

	run := h.createRun("code-no-acceptance.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}

	if reviews := h.reviewRecords(run.ID); len(reviews) != 0 {
		t.Errorf("review records = %+v, want none: a mechanical:false signal is honestly not machine-checkable", reviews)
	}
	signals := h.successSignalRecords(run.ID)
	if len(signals) != 1 {
		t.Fatalf("run has %d success_signal records, want 1", len(signals))
	}
	if signals[0].Authority != ledger.AuthorityProposed {
		t.Errorf("success_signal authority = %q, want proposed", signals[0].Authority)
	}
}

// A mechanical:true signal whose check kind this build has no evaluator for
// is left unevaluated too — no record, never a guessed verdict. The gate is
// runners.MechanicallyEvaluableChecks, the same registry the acceptance
// evaluator grows through, so a kind added there (t17's extension) becomes
// evaluable here with no change to this file.
func TestMechanicalSignalWithUnevaluableKindGetsNoEvaluationRecord(t *testing.T) {
	var h *codeHarness
	h = newCodeHarness(t, func(op runners.Operation, call int) (runners.Result, error) {
		if call == 1 {
			proposeSuccessSignal(t, h, op.Context.RunID,
				`{"statement":"every claim in the run is confirmed","check":{"kind":"claims_confirmed"},"mechanical":true}`)
		}
		return codeRunResult(op, 0), nil
	})

	run := h.createRun("code-no-acceptance.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", state, h.workerErrors())
	}

	if reviews := h.reviewRecords(run.ID); len(reviews) != 0 {
		t.Errorf("review records = %+v, want none: this build has no mechanical evaluator for claims_confirmed", reviews)
	}
}
