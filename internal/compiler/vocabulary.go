package compiler

import "sort"

// Node kinds the MVP compiler understands (PRD §9.2). The authoring schema
// already closes this enum; the constants exist so kind-specific rules read as
// rules rather than as string literals.
const (
	KindAgent      = "agent"
	KindCode       = "code"
	KindActionHTTP = "action.http"
	KindDecision   = "decision"
	KindApproval   = "approval"
	KindWait       = "wait"
	KindParallel   = "parallel"
	KindJoin       = "join"
	KindEnd        = "end"
)

// technicalStatuses are the engine's own statuses (PRD §3.4). An edge may
// originate from one — that is how a workflow routes a timeout or a contract
// rejection somewhere useful — even though no node contract declares them.
//
// Note what is *not* here: `changes_required`, `passed`, `approved` and friends
// are domain outcomes, and `failed` appears on both lists only in the sense
// that a node may also *declare* an outcome named `failed` (the PRD §11.1 test
// node does exactly that). Declaring it is legal; the engine's own `failed`
// status simply also routes.
var technicalStatuses = map[string]bool{
	"succeeded":         true,
	"failed":            true,
	"timed_out":         true,
	"cancelled":         true,
	"policy_denied":     true,
	"contract_rejected": true,
}

// OutcomeBudgetExhausted is the reserved name a refused dispatch routes under
// (task t11, spec claim c6 / honesty h5).
//
// It sits in the same family as the technical statuses above and for the same
// reason: no node contract can declare it, because no ACTOR produces it. The
// control plane produces it, before dispatching, when the run's declared
// `spec.budget` cannot fund the next provider session. PRD §3.4's rule — do
// not use technical failure to represent an expected outcome — is exactly why
// it is not folded into `failed`: an author who declared a budget expects to
// be refused eventually, and wants that refusal to follow an edge (a cheaper
// actor, a human, a summarise-and-stop node) rather than to end the run.
//
// It is NOT a technical status: §3.4's list is closed and this is not on it.
// The engine records the refused attempt's technical status as
// `policy_denied` — a declared policy denied the dispatch — and routes the
// edge under this name (internal/engine/complete.go's failOrRetry).
const OutcomeBudgetExhausted = "budget_exhausted"

// budgetGuardedKinds are the kinds whose dispatch the economic budget can
// refuse, and therefore the only kinds an OutcomeBudgetExhausted edge may
// originate from. They are the kinds internal/worker/dispatch.go's
// dispatchActor handles: the enforcement site guards provider sessions, and
// an edge from a code or approval node's `budget_exhausted` would be a route
// that can never fire.
var budgetGuardedKinds = map[string]bool{
	KindAgent:      true,
	KindActionHTTP: true,
}

// impliedOutcomes are the outcomes a node kind offers without declaring them in
// a contract. An approval node's ports come from the human decision, not from
// an actor's output schema (PRD §9.9), so the PRD §11.1 example routes
// `human-review.approved` with no contract block on the node.
var impliedOutcomes = map[string][]string{
	KindApproval: {"approved", "expired", "rejected"},
	KindWait:     {"completed"},
	// A parallel node's `split` and a join node's `joined` are engine-shaped
	// routing outcomes (issue #43, parallel-tokens design §3.1/§4.1): the
	// nodes do no domain work and carry no contract block, so their one
	// outcome each is implied by the kind exactly like a wait's `completed`.
	KindParallel: {"split"},
	KindJoin:     {"joined"},
}

// ledgerProjections is the standard projection vocabulary (PRD §10.9). It is
// closed on purpose: a typo in a projection name should fail loudly rather than
// silently bind an agent to an empty view.
var ledgerProjections = map[string]bool{
	"current_scope":      true,
	"confirmed_claims":   true,
	"open_assumptions":   true,
	"open_questions":     true,
	"ready_tasks":        true,
	"active_tasks":       true,
	"verification_queue": true,
	"decision_history":   true,
	"evidence":           true,
	"delivery_summary":   true,
}

// ledgerRecordTypes is the MVP record-type vocabulary (PRD §10.2).
var ledgerRecordTypes = map[string]bool{
	"announcement":   true,
	"claim":          true,
	"assumption":     true,
	"question":       true,
	"task":           true,
	"decision":       true,
	"success_signal": true,
	"evidence":       true,
	"result":         true,
	"review":         true,
}

// acceptanceKinds are the mechanical acceptance checks the PRD names (§10.10,
// plus `workspace_diff` from the §11.1 example). An unknown kind is a warning,
// not an error: the check registry is a later task, and rejecting a check this
// compiler has not learned about yet would block authoring for no safety gain.
var acceptanceKinds = map[string]bool{
	"process_exit":                true,
	"workspace_diff":              true,
	"schema_valid":                true,
	"artifact_digest":             true,
	"dependency_delta":            true,
	"parity_fixtures":             true,
	"changed_paths_within_policy": true,
	"claims_confirmed":            true,
	"no_blocking_questions":       true,
}

// sortedKeys returns a map's keys in a stable order, so anything derived from a
// map (IR fields, diagnostic iteration) is reproducible.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
