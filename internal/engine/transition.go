package engine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/cel-go/common/types/ref"
)

// Edge selection and loop bounds are pure functions over the workflow and a
// few counters, deliberately holding no database handle. That is what makes
// PRD §9.7's "no loop may rely solely on an agent deciding when to stop"
// testable as a property — a generated looping workflow can be driven through
// planTransition thousands of times without a Postgres round trip, and the
// same function is what the committed transaction calls.

// transitionInput is everything edge selection and loop enforcement need.
type transitionInput struct {
	Workflow *Workflow
	// NodeID and Outcome are the completed node and the outcome to route.
	NodeID  string
	Outcome string
	// RunInput and Output are the CEL activation: the run's input and the
	// completed node's output for this outcome.
	RunInput json.RawMessage
	Output   json.RawMessage
	// Transitions is how many transitions the run has already taken, Visits
	// how many node runs each node already has, Elapsed the run's wall clock.
	Transitions int
	Visits      map[string]int
	Elapsed     time.Duration
	// ActiveTokens is how many tokens the run currently has active, read
	// inside the transaction (tokens_run_state_idx serves it). Only a
	// parallel node's fan-out consults it — the completion reads it exactly
	// then, so a sequential transition costs no extra query.
	ActiveTokens int
}

// transitionPlan is what the engine should do next. Exactly one of Targets,
// Complete, Bound, or Diagnostic is meaningful.
type transitionPlan struct {
	// Targets are the eligible edges in normalized order. len == 1 for every
	// node kind except parallel, where it is the full eligible set (design
	// D1: set selection is opt-in via the explicit kind; ordinary edges stay
	// first-match-wins).
	Targets []transitionTarget
	// Complete reports that the run ends here with no further node run.
	Complete bool
	// Bound is the loop bound that stopped the run.
	Bound *BoundExceeded
	// Diagnostic explains why no edge was eligible.
	Diagnostic string
}

// transitionTarget is one edge the plan follows.
type transitionTarget struct {
	Edge       *Edge
	NextNodeID string
}

// edge is the plan's first eligible edge and nextNodeID its target — the
// sequential view every non-parallel path reads, since only a parallel node's
// split ever puts more than one target in the plan. edge is nil when the plan
// selected nothing.
func (p transitionPlan) edge() *Edge {
	if len(p.Targets) == 0 {
		return nil
	}
	return p.Targets[0].Edge
}

func (p transitionPlan) nextNodeID() string {
	if len(p.Targets) == 0 {
		return ""
	}
	return p.Targets[0].NextNodeID
}

// planTransition performs §12.5 steps 8 and 9's decision half: it selects the
// eligible edge for a node's domain outcome and enforces the §9.7 loop bounds
// *before* anything is created. It never writes; the caller applies the plan.
//
// Edge selection is first-match-wins in normalized edge order. That order is
// the compiler's — source node, outcome, target, guard text — and it is
// deterministic across recompiles, so which edge wins is a property of the
// definition rather than of how the author happened to list the edges.
//
// A guard that fails to evaluate does not match. Guards are compiled and
// type-checked at publish time, so a runtime evaluation failure means the
// guard reached into data the payload did not carry; treating that as "no
// match" keeps the failure visible (it lands in the diagnostic when no edge
// matches at all) without wedging the run on an exception.
func planTransition(in transitionInput) transitionPlan {
	node := in.Workflow.Nodes[in.NodeID]
	if node == nil {
		return transitionPlan{Diagnostic: fmt.Sprintf("node %q is not declared in the pinned workflow", in.NodeID)}
	}

	// A parallel node's `split` outcome selects a SET: every edge whose
	// guard passes fires (design D1). Every other node kind — and a parallel
	// node's routed technical statuses — keeps first-match-wins.
	collectAll := node.Kind == kindParallel && in.Outcome == outcomeSplit

	var guardFailures []string
	var matched []transitionTarget

	for i := range in.Workflow.Edges {
		edge := &in.Workflow.Edges[i]
		if edge.FromNode != in.NodeID || edge.FromOutcome != in.Outcome {
			continue
		}
		if edge.Guard == nil {
			matched = append(matched, transitionTarget{Edge: edge, NextNodeID: edge.To})
			if !collectAll {
				break
			}
			continue
		}
		ok, err := evaluateGuard(edge, in)
		if err != nil {
			guardFailures = append(guardFailures, fmt.Sprintf("%s -> %s: %v", edge.From, edge.To, err))
			continue
		}
		if ok {
			matched = append(matched, transitionTarget{Edge: edge, NextNodeID: edge.To})
			if !collectAll {
				break
			}
		}
	}

	if len(matched) == 0 {
		// An end node produces the workflow result and has no outgoing edges
		// (the compiler refuses them), so reaching this point on one is the
		// run finishing, not a routing failure. A split that selects zero
		// edges is the same no-eligible-edge routing failure as any other
		// unrouted outcome (design §3.1).
		if node.Kind == kindEnd {
			return transitionPlan{Complete: true}
		}
		return transitionPlan{Diagnostic: noEligibleEdgeDiagnostic(in, node, guardFailures)}
	}

	// §9.7 loop bounds, enforced before the next node runs exist and over
	// the WHOLE eligible set: a K-way split is charged K transitions, each
	// target one visit, and K-1 net new active tokens — and a bound refuses
	// the entire split, never a partial fan-out (design D8).
	if bound := checkBounds(in, node, matched); bound != nil {
		return transitionPlan{Targets: matched, Bound: bound}
	}

	return transitionPlan{Targets: matched}
}

// Node kinds and kind-implied outcomes the engine branches on. Declared here
// rather than imported from the compiler for the reason the worker states:
// the dependency is on the IR's values, not on the authoring package.
const (
	kindEnd      = "end"
	kindParallel = "parallel"
	kindJoin     = "join"

	outcomeSplit  = "split"
	outcomeJoined = "joined"
)

func checkBounds(in transitionInput, node *Node, targets []transitionTarget) *BoundExceeded {
	limits := in.Workflow.Limits
	k := len(targets)

	if limits.MaxTransitions > 0 && in.Transitions+k > limits.MaxTransitions {
		return &BoundExceeded{
			Kind:   BoundTransitions,
			NodeID: targets[0].NextNodeID,
			Limit:  strconv.Itoa(limits.MaxTransitions),
			Actual: strconv.Itoa(in.Transitions + k),
		}
	}
	if limits.MaxVisitsPerNode > 0 {
		// Two split edges may target one node; each creates a node run there,
		// so the charge accumulates per target rather than being a flat +1.
		charged := make(map[string]int, k)
		for _, target := range targets {
			charged[target.NextNodeID]++
			if visits := in.Visits[target.NextNodeID] + charged[target.NextNodeID]; visits > limits.MaxVisitsPerNode {
				return &BoundExceeded{
					Kind:   BoundVisits,
					NodeID: target.NextNodeID,
					Limit:  strconv.Itoa(limits.MaxVisitsPerNode),
					Actual: strconv.Itoa(visits),
				}
			}
		}
	}
	if limits.MaxDuration > 0 && in.Elapsed > limits.MaxDuration {
		return &BoundExceeded{
			Kind:   BoundDuration,
			NodeID: targets[0].NextNodeID,
			Limit:  limits.MaxDuration.String(),
			Actual: in.Elapsed.Round(time.Millisecond).String(),
		}
	}
	// maxParallelTokens is charged only at a split: an ordinary transition
	// consumes one token and creates one, so the active count cannot move.
	// The source token is consumed in the same transaction, hence the -1
	// (design §5.1). A cardinality-1 split cannot raise the count either,
	// but the check still runs so the accounting stays one rule.
	if node.Kind == kindParallel && in.Outcome == outcomeSplit &&
		limits.MaxParallelTokens > 0 && in.ActiveTokens-1+k > limits.MaxParallelTokens {
		return &BoundExceeded{
			Kind:   BoundParallelTokens,
			NodeID: in.NodeID,
			Limit:  strconv.Itoa(limits.MaxParallelTokens),
			Actual: strconv.Itoa(in.ActiveTokens - 1 + k),
		}
	}
	return nil
}

func evaluateGuard(edge *Edge, in transitionInput) (bool, error) {
	activation := map[string]any{
		celVarInput:   decodeActivation(in.RunInput),
		celVarOutput:  decodeActivation(in.Output),
		celVarOutcome: in.Outcome,
		// A node-outcome transition has no event. The variable is still bound,
		// to an empty map, because the shared environment declares it: a guard
		// that reaches into it then reports "no such key" — a guard failure,
		// which does not match — instead of failing to evaluate at all.
		celVarEvent: map[string]any{},
	}
	out, _, err := edge.Guard.Eval(activation)
	if err != nil {
		return false, err
	}
	return truthy(out)
}

// decodeActivation turns a JSON payload into the plain Go values CEL's
// dynamic type handles. Undecodable or absent payloads become an empty map
// rather than nil, so a guard that reaches into a field of a missing payload
// reports "no such key" — a guard failure — instead of a nil dereference.
func decodeActivation(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return map[string]any{}
	}
	if value == nil {
		return map[string]any{}
	}
	return value
}

// truthy insists on an actual boolean. CEL has no truthiness coercion and
// neither does this: a guard that evaluates to a string or a number has not
// answered the yes/no question an edge asks, and quietly reading it as "no"
// would hide an authoring mistake the compiler could not catch.
func truthy(value ref.Val) (bool, error) {
	result, ok := value.Value().(bool)
	if !ok {
		return false, fmt.Errorf("guard produced %s, which is not a boolean", value.Type())
	}
	return result, nil
}

func noEligibleEdgeDiagnostic(in transitionInput, node *Node, guardFailures []string) string {
	detail := fmt.Sprintf("node %q produced outcome %q and no edge is eligible", in.NodeID, in.Outcome)
	if !node.declaresOutcome(in.Outcome) && !technicalStatus(in.Outcome) {
		return detail + fmt.Sprintf("; the node declares %v", node.Outcomes)
	}
	if len(guardFailures) > 0 {
		return detail + fmt.Sprintf("; every candidate edge's guard declined or failed: %v", guardFailures)
	}
	return detail + "; the workflow declares no edge from this outcome"
}

// technicalStatus reports whether name is one of the engine's own statuses
// (PRD §3.4). A workflow may route from one — that is how it sends a timeout
// or a contract rejection somewhere useful — even though no node contract
// declares it.
func technicalStatus(name string) bool {
	switch TechStatus(name) {
	case StatusSucceeded, StatusFailed, StatusTimedOut,
		StatusCancelled, StatusPolicyDenied, StatusContractRejected:
		return true
	default:
		return false
	}
}
