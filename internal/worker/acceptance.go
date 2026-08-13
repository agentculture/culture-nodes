package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners"
)

// Mechanical acceptance evaluation and the enforce policy (task t17, issue
// #37; PRD §10.10, docs/acceptance.md criterion 14).
//
// internal/runners.EvaluateAcceptance is the pure evaluator; this file wires
// it into the completion path in two phases:
//
//  1. BEFORE the completion commits, evaluateAcceptance computes the verdict
//     against the exact runners.Result the completion's own evidence was
//     built from, and acceptanceEvaluation.apply lets the node's declared
//     `acceptance.enforce` policy act on the routing:
//
//     - observe (also an omitted field): routing untouched — exactly the
//     pre-t17 behavior.
//     - route_technical: a failing check converts the completion to the
//     contract_rejected TECHNICAL status, which then composes with the
//     engine's ordinary failOrRetry — retried only per the node's own
//     declared retry policy, never a retry this file invents.
//     - route_outcome:<name>: a failing check re-routes the completion to
//     the named DOMAIN outcome, which the compiler guaranteed the node
//     declares, so planTransition follows a real edge; there is no retry,
//     because a domain answer is not an engine failure (PRD §3.4).
//
//  2. AFTER the completion commits, appendAcceptanceVerdict records the
//     evaluation — pass or fail, any mode — as a derived, validator-origin
//     ledger record, the same "second write after an already-committed
//     completion" shape hooks.go's appendAssuranceRejection uses (see that
//     file's doc for why the engine's own delta preparation refuses a
//     derived-authority record, so it can only be appended through the
//     ledger directly). A failure to append is reported through
//     Options.OnError, never allowed to unwind the committed completion.
//
// # The honest floor: unevaluated is unenforced
//
// A check this build cannot mechanically evaluate — an unknown kind, a
// malformed payload, or a fact the runner honestly reported unmeasured —
// produces no verdict, and nothing is enforced on a verdict nobody computed:
// routing proceeds exactly as if the policy were observe, and the derived
// record's `enforcement` field says so. The same floor covers an agent
// node's checks wholesale (appendUnevaluableAcceptance): an agent dispatch
// produces no runner-measured Result at all, so under a routing enforce
// policy the record states the non-evaluability and the agent's own outcome
// routes untouched — never a verdict fabricated from the agent's
// self-report, which is exactly the fabrication the runner boundary exists
// to prevent. (An agent node under plain observe keeps the pre-t17 behavior:
// no record. The async agent callback path — a §12.6 parked invocation
// completing through internal/api — carries no acceptance evaluation yet
// either; that gap is documented, not silently assumed closed.)
//
// A completion that already carries a technical status (a timeout, a refused
// dispatch) is not converted or re-routed either: there is no routed domain
// answer to guard, and masking the real status would lose the fact an
// operator needs.

// The enforce vocabulary, byte-for-byte what the compiler validated and the
// IR carries (internal/compiler/ledger.go). Kept in sync by hand, like
// runners.MechanicallyEvaluableChecks with the compiler's acceptanceKinds.
const (
	enforceObserve            = "observe"
	enforceRouteTechnical     = "route_technical"
	enforceRouteOutcomePrefix = "route_outcome:"
)

// acceptanceEvaluation is one pre-routing evaluation of a node's declared
// acceptance checks, carried from where it is computed (before the
// completion commits) to where it is recorded (after).
type acceptanceEvaluation struct {
	verdict runners.AcceptanceVerdict
	// enforce is the node's effective policy: the declared field, or
	// "observe" when it was omitted.
	enforce string
	// enforcement is filled by apply: what the policy actually did to the
	// routing, recorded verbatim in the derived record's payload.
	enforcement string
}

// evaluateAcceptance mechanically evaluates node's declared acceptance
// checks against res, before any completion is committed. It returns nil
// when the node declares no checks — evaluation is opt-in per node.
func evaluateAcceptance(node *nodeSpec, res runners.Result) *acceptanceEvaluation {
	if node.Acceptance == nil || len(node.Acceptance.Requires) == 0 {
		return nil
	}
	return &acceptanceEvaluation{
		verdict: runners.EvaluateAcceptance(node.Acceptance.Requires, res),
		enforce: effectiveEnforce(node),
	}
}

func effectiveEnforce(node *nodeSpec) string {
	if node.Acceptance == nil || node.Acceptance.Enforce == "" {
		return enforceObserve
	}
	return node.Acceptance.Enforce
}

// evaluatedFailure reports whether any check was actually evaluated AND
// failed. This — not verdict.Passed — is what enforcement triggers on:
// verdict.Passed is also false for a merely unevaluated check, and an
// unevaluated check is enforced by nobody (the honest floor above).
func (e *acceptanceEvaluation) evaluatedFailure() bool {
	for _, check := range e.verdict.Checks {
		if check.Evaluated && !check.Passed {
			return true
		}
	}
	return false
}

func (e *acceptanceEvaluation) unevaluatedCount() int {
	n := 0
	for _, check := range e.verdict.Checks {
		if !check.Evaluated {
			n++
		}
	}
	return n
}

// apply applies the enforce policy to a proposed completion's technical
// status and domain outcome, returning what should actually be committed and
// recording what it did in e.enforcement.
func (e *acceptanceEvaluation) apply(status engine.TechStatus, outcome string) (engine.TechStatus, string) {
	failing := e.evaluatedFailure()
	unevaluated := e.unevaluatedCount()

	switch {
	case e.enforce == enforceObserve:
		e.enforcement = "observe: verdict recorded, routing untouched"
		return status, outcome

	case status != engine.StatusSucceeded:
		e.enforcement = fmt.Sprintf(
			"%s declared but not applicable: the completion carries technical status %q, not a routed domain outcome; "+
				"the engine's own failure handling applies", e.enforce, status)
		return status, outcome

	case failing && e.enforce == enforceRouteTechnical:
		e.enforcement = fmt.Sprintf(
			"route_technical: a failing pre-announced check converted the completion (outcome %q) to %s; "+
				"the node's declared retry policy governs any retry", outcome, engine.StatusContractRejected)
		return engine.StatusContractRejected, ""

	case failing: // route_outcome:<name> — the only remaining routing mode.
		name := strings.TrimPrefix(e.enforce, enforceRouteOutcomePrefix)
		e.enforcement = fmt.Sprintf(
			"route_outcome: a failing pre-announced check re-routed the completion from outcome %q to declared outcome %q; "+
				"a domain answer is not retried", outcome, name)
		return status, name

	case unevaluated > 0:
		e.enforcement = fmt.Sprintf(
			"%s declared but not applied: %d of %d checks were not mechanically evaluated, and an unevaluated check "+
				"is enforced by nobody — routing proceeds as observe (the honest floor)",
			e.enforce, unevaluated, len(e.verdict.Checks))
		return status, outcome

	default:
		e.enforcement = e.enforce + ": all checks passed, routing untouched"
		return status, outcome
	}
}

// appendAcceptanceVerdict records eval as a derived review record against the
// committed completion. It is a no-op when eval is nil (no checks declared).
func (w *Worker) appendAcceptanceVerdict(
	ctx context.Context, node *nodeSpec, eval *acceptanceEvaluation, completion engine.CompletionResult,
) {
	if eval == nil {
		return
	}

	evidenceID := evidenceRecordID(completion.LedgerRecords)
	payload, err := json.Marshal(struct {
		Verdict     string                          `json:"verdict"`
		Reason      string                          `json:"reason"`
		Enforce     string                          `json:"enforce"`
		Enforcement string                          `json:"enforcement"`
		Checks      []runners.AcceptanceCheckResult `json:"checks"`
	}{
		Verdict:     acceptanceVerdictLabel(eval.verdict.Passed),
		Reason:      fmt.Sprintf("mechanical evaluation of node %q's declared acceptance.requires (PRD §10.10)", node.ID),
		Enforce:     eval.enforce,
		Enforcement: eval.enforcement,
		Checks:      eval.verdict.Checks,
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

// appendUnevaluableAcceptance is the agent-node honest floor: the node
// declared acceptance checks under a routing enforce policy, but an agent
// dispatch produces no runner-measured Result to evaluate them against. Each
// check is recorded honestly unevaluated, the enforcement field states the
// observe floor, and the caller's routing was never touched. Under plain
// observe (or no declared checks) it is a no-op — the pre-t17 behavior.
func (w *Worker) appendUnevaluableAcceptance(ctx context.Context, node *nodeSpec, completion engine.CompletionResult) {
	if node.Acceptance == nil || len(node.Acceptance.Requires) == 0 {
		return
	}
	enforce := effectiveEnforce(node)
	if enforce == enforceObserve {
		return
	}

	checks := make([]runners.AcceptanceCheckResult, 0, len(node.Acceptance.Requires))
	for _, requirement := range node.Acceptance.Requires {
		kind, _ := requirement["kind"].(string)
		checks = append(checks, runners.AcceptanceCheckResult{
			Kind:      kind,
			Evaluated: false,
			Passed:    false,
			Reason: "an agent dispatch produces no runner-measured result; this check is not mechanically " +
				"evaluable here, and no verdict is fabricated from the agent's self-report",
		})
	}
	eval := &acceptanceEvaluation{
		verdict: runners.AcceptanceVerdict{Passed: false, Checks: checks},
		enforce: enforce,
		enforcement: fmt.Sprintf(
			"%s declared but not applied: none of the %d checks is mechanically evaluable on an agent dispatch — "+
				"routing proceeds as observe (the honest floor)", enforce, len(checks)),
	}
	w.appendAcceptanceVerdict(ctx, node, eval, completion)
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
