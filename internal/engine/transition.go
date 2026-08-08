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
}

// transitionPlan is what the engine should do next. Exactly one of Edge,
// Complete, Bound, or Diagnostic is meaningful.
type transitionPlan struct {
	// Edge is the eligible edge, nil when none was found.
	Edge *Edge
	// NextNodeID is the node the token moves to.
	NextNodeID string
	// Complete reports that the run ends here with no further node run.
	Complete bool
	// Bound is the loop bound that stopped the run.
	Bound *BoundExceeded
	// Diagnostic explains why no edge was eligible.
	Diagnostic string
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

	var guardFailures []string
	var matched *Edge

	for i := range in.Workflow.Edges {
		edge := &in.Workflow.Edges[i]
		if edge.FromNode != in.NodeID || edge.FromOutcome != in.Outcome {
			continue
		}
		if edge.Guard == nil {
			matched = edge
			break
		}
		ok, err := evaluateGuard(edge, in)
		if err != nil {
			guardFailures = append(guardFailures, fmt.Sprintf("%s -> %s: %v", edge.From, edge.To, err))
			continue
		}
		if ok {
			matched = edge
			break
		}
	}

	if matched == nil {
		// An end node produces the workflow result and has no outgoing edges
		// (the compiler refuses them), so reaching this point on one is the
		// run finishing, not a routing failure.
		if node.Kind == kindEnd {
			return transitionPlan{Complete: true}
		}
		return transitionPlan{Diagnostic: noEligibleEdgeDiagnostic(in, node, guardFailures)}
	}

	// §9.7 loop bounds, enforced before the next node run exists. Checking
	// after creating it would let a run cross the bound and then be told off
	// for it.
	if bound := checkBounds(in, matched.To); bound != nil {
		return transitionPlan{Edge: matched, NextNodeID: matched.To, Bound: bound}
	}

	return transitionPlan{Edge: matched, NextNodeID: matched.To}
}

const kindEnd = "end"

func checkBounds(in transitionInput, nextNodeID string) *BoundExceeded {
	limits := in.Workflow.Limits

	if limits.MaxTransitions > 0 && in.Transitions+1 > limits.MaxTransitions {
		return &BoundExceeded{
			Kind:   BoundTransitions,
			NodeID: nextNodeID,
			Limit:  strconv.Itoa(limits.MaxTransitions),
			Actual: strconv.Itoa(in.Transitions + 1),
		}
	}
	if limits.MaxVisitsPerNode > 0 && in.Visits[nextNodeID]+1 > limits.MaxVisitsPerNode {
		return &BoundExceeded{
			Kind:   BoundVisits,
			NodeID: nextNodeID,
			Limit:  strconv.Itoa(limits.MaxVisitsPerNode),
			Actual: strconv.Itoa(in.Visits[nextNodeID] + 1),
		}
	}
	if limits.MaxDuration > 0 && in.Elapsed > limits.MaxDuration {
		return &BoundExceeded{
			Kind:   BoundDuration,
			NodeID: nextNodeID,
			Limit:  limits.MaxDuration.String(),
			Actual: in.Elapsed.Round(time.Millisecond).String(),
		}
	}
	return nil
}

func evaluateGuard(edge *Edge, in transitionInput) (bool, error) {
	activation := map[string]any{
		celVarInput:   decodeActivation(in.RunInput),
		celVarOutput:  decodeActivation(in.Output),
		celVarOutcome: in.Outcome,
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
