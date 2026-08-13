package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Event routes: any-node pickup, including splits (issue #43 task t21; the
// parallel-tokens design §6.1, decisions D9/D10/D13).
//
// An `onEvent` edge in the pinned IR says "when event E is delivered to this
// run, create a token at node N". CreateRun materializes one durable
// event_routes row per such edge; a delivery matches those rows with one
// indexed SQL scan and calls PickUpEvent per match, inside the delivery's own
// transaction under the run's advisory lock.
//
// Two properties are load-bearing and both come from where the code sits:
//
//   - Pickup NEVER completes anything. It creates a token, a node run, and
//     the node's work (or its human task) — the entry-token shape CreateRun
//     already writes — and a fenced worker completes it through the ordinary
//     §12.5 transaction. That is the same single-writer discipline signal
//     delivery has followed since t10.
//   - Pickup runs THROUGH the engine, not in the store, because dispatching a
//     node is engine logic: an approval-node target parks on a human task
//     instead of a work item (PRD §9.9), and the design's own motivating
//     example — an agent node asking a human node for a reply — is exactly
//     that case. A store-side pickup would have had to reimplement
//     dispatchNode, or silently refuse the feature's headline scenario.
//
// Several edges may name one event: one delivery then calls PickUpEvent once
// per matching route, which is the pickup SPLIT (design D9) with no extra
// machinery — the set semantics fall out of the route rows.

// Event-route statuses. A route is retired only when its run reaches a
// terminal state — never on a match, because routes are multi-fire (D10): a
// cron-like emitter driving a run's node repeatedly is the loops-and-flows-
// are-one-thing model, and the protection against runaway is §9.7's bounds,
// not a one-shot route.
const (
	EventRouteActive  = "active"
	EventRouteRetired = "retired"
)

// EventRoute is one run-scoped durable pickup route, materialized from an
// `onEvent` edge of the run's pinned IR (migrations/0021).
type EventRoute struct {
	ID          string
	NamespaceID string
	RunID       string
	EventName   string
	TargetNode  string
	// Guard is the edge's CEL source, carried so an operator can read why a
	// delivery did or did not pick up. The authoritative program is rebuilt
	// from the pinned IR, never from this copy.
	Guard     string
	Status    string
	CreatedAt time.Time
}

// PickupEvent is the delivered fact a route is being matched against — the
// signal_events row, in the engine's own vocabulary so this package does not
// have to import the store's.
type PickupEvent struct {
	ID      string
	Name    string
	Emitter string
	Payload json.RawMessage
}

// Pickup refusal reasons (design D13). `guard` is the ordinary answer for a
// filtered route and is not a refusal an operator needs to act on; the bound
// kinds are.
const (
	PickupRefusedGuard       = "guard"
	PickupRefusedRunTerminal = "run_terminal"
)

// EventPickupResult is what one route did with one delivered fact.
type EventPickupResult struct {
	RunID     string
	RouteID   string
	Admitted  bool
	TokenID   string
	NodeRunID string
	NodeID    string
	// WorkID is set for a target that enqueued claimable work;
	// HumanTaskID for an approval-node target that parked on a human task.
	WorkID      string
	HumanTaskID string
	// Refusal names why no token was created — PickupRefusedGuard,
	// PickupRefusedRunTerminal, or a BoundKind — and Detail explains it.
	Refusal string
	Detail  string
}

// EventPickupRunner is the slice of the engine a delivery transaction needs.
// It is an interface so internal/store/postgres can call the engine without
// importing it for anything larger, and so a delivery configured without one
// simply does no route pickup (every pre-t21 caller and test).
type EventPickupRunner interface {
	PickUpEvent(ctx context.Context, tx Tx, route EventRoute, ev PickupEvent) (EventPickupResult, error)
}

var _ EventPickupRunner = (*Engine)(nil)

// PickUpEvent creates one token at a matched route's target node, or records
// why it did not.
//
// It must be called inside a transaction that already holds the run's
// advisory lock — the delivery transaction does — because it reads the run's
// bound counters and appends audit events, and both are only exact under it.
//
// Refusal semantics are D13's, and deliberately different from a split's
// (D8): a split that would cross a bound FAILS the run, because the run
// itself asked for one branch too many; an external event arriving while a
// run is at its cap must NOT fail a healthy run — the run did nothing, the
// world spoke at a busy moment. The pickup is skipped, the refusal is
// recorded as an audit event, the fact stays appended, and the run continues.
// There is no deferred retry: that would be admission-control scheduling,
// which D8 already declined to build, and the append-only fact table keeps it
// retrofittable if refusal ever proves too blunt (open item O4).
func (e *Engine) PickUpEvent(ctx context.Context, tx Tx, route EventRoute, ev PickupEvent) (EventPickupResult, error) {
	result := EventPickupResult{RunID: route.RunID, RouteID: route.ID, NodeID: route.TargetNode}

	run, err := tx.Run(ctx, route.RunID)
	if err != nil {
		return result, err
	}
	if run.State.Terminal() {
		// Routes are retired when a run goes terminal, so this is a race, not
		// a normal path — and a token created into a dead run would be exactly
		// the re-dispatch zombie issue #19 fixed.
		return e.refusePickup(ctx, tx, route, ev, result, PickupRefusedRunTerminal,
			fmt.Sprintf("run %s is %s; its routes no longer pick up", run.ID, run.State))
	}

	digest, ir, err := tx.WorkflowIR(ctx, run.WorkflowVersionID)
	if err != nil {
		return result, err
	}
	wf, err := e.Workflow(digest, ir)
	if err != nil {
		return result, err
	}
	edge := wf.eventEdgeFor(route)
	if edge == nil {
		// The route was materialized from this very IR at CreateRun, so a
		// route with no edge means the row and the pin disagree — corruption,
		// not a routing decision. Refusing loudly beats creating a token whose
		// provenance nothing can explain.
		return result, &WorkflowError{
			Digest: digest,
			Detail: fmt.Sprintf("event route %s (onEvent %q -> %q) matches no edge in the pinned definition",
				route.ID, route.EventName, route.TargetNode),
		}
	}
	node := wf.Nodes[route.TargetNode]
	if node == nil {
		return result, &WorkflowError{
			Digest: digest,
			Detail: fmt.Sprintf("event route %s targets node %q, which the pinned definition does not declare", route.ID, route.TargetNode),
		}
	}

	if edge.Guard != nil {
		ok, err := evaluatePickupGuard(edge, run.Input, ev)
		if err != nil || !ok {
			detail := fmt.Sprintf("the route's guard %q declined event %s", edge.When, ev.ID)
			if err != nil {
				detail = fmt.Sprintf("the route's guard %q failed to evaluate against event %s: %v", edge.When, ev.ID, err)
			}
			return e.refusePickup(ctx, tx, route, ev, result, PickupRefusedGuard, detail)
		}
	}

	visits, err := tx.NodeVisits(ctx, run.ID)
	if err != nil {
		return result, err
	}
	transitions, err := tx.TransitionCount(ctx, run.ID)
	if err != nil {
		return result, err
	}
	active, err := tx.ActiveTokenCount(ctx, run.ID)
	if err != nil {
		return result, err
	}
	if bound := checkPickupBounds(wf.Limits, route.TargetNode, transitions, visits[route.TargetNode], active); bound != nil {
		return e.refusePickup(ctx, tx, route, ev, result, string(bound.Kind), fmt.Sprintf(
			"picking up event %s would take the run past its %s bound (limit %s, would be %s)",
			ev.ID, bound.Kind, bound.Limit, bound.Actual))
	}

	now := e.now().UTC()
	token := Token{
		ID:          e.newID(),
		NamespaceID: run.NamespaceID,
		RunID:       run.ID,
		NodeID:      route.TargetNode,
		State:       TokenActive,
		// No parent and no group, and that is the honest record rather than a
		// gap — see review finding D4, answered by OriginEventID below.
		OriginEventID: ev.ID,
		CreatedAt:     now,
	}
	if err := tx.InsertToken(ctx, token); err != nil {
		return result, err
	}

	nodeRun := NodeRun{
		ID:          e.newID(),
		NamespaceID: run.NamespaceID,
		RunID:       run.ID,
		TokenID:     token.ID,
		NodeID:      route.TargetNode,
		State:       dispatchState(node.Kind),
		VisitCount:  visits[route.TargetNode] + 1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := tx.InsertNodeRun(ctx, nodeRun); err != nil {
		return result, err
	}

	// The edge that produced this node run is an event, not a node outcome —
	// so an approval-node target's human task says "event:<name>" put the run
	// here rather than naming a node that did not.
	workID, humanTaskID, err := e.dispatchNode(ctx, tx, node, run, nodeRun, "event:"+ev.Name, ev.ID, now)
	if err != nil {
		return result, err
	}

	result.Admitted = true
	result.TokenID = token.ID
	result.NodeRunID = nodeRun.ID
	result.WorkID = workID
	result.HumanTaskID = humanTaskID

	if _, err := tx.AppendEvent(ctx, run.ID, event(TypeEventPickedUp, map[string]any{
		"run_id":      run.ID,
		"node_run_id": nodeRun.ID,
		"node_id":     nodeRun.NodeID,
		"token_id":    token.ID,
		"route_id":    route.ID,
		"event_id":    ev.ID,
		"event_name":  ev.Name,
		"emitter":     ev.Emitter,
		"guard":       edge.When,
		"visit":       nodeRun.VisitCount,
	})); err != nil {
		return result, err
	}
	if humanTaskID != "" {
		_, err = tx.AppendEvent(ctx, run.ID, event(TypeHumanTaskCreated, map[string]any{
			"run_id":        run.ID,
			"node_run_id":   nodeRun.ID,
			"node_id":       nodeRun.NodeID,
			"token_id":      token.ID,
			"human_task_id": humanTaskID,
			"visit":         nodeRun.VisitCount,
		}))
		return result, err
	}
	_, err = tx.AppendEvent(ctx, run.ID, event(TypeNodeRunReady, map[string]any{
		"run_id":      run.ID,
		"node_run_id": nodeRun.ID,
		"node_id":     nodeRun.NodeID,
		"token_id":    token.ID,
		"work_id":     workID,
		"visit":       nodeRun.VisitCount,
	}))
	return result, err
}

// refusePickup records a pickup that did not happen and returns the
// unadmitted result. It is not an error: an operator watching the run sees
// the refusal in the audit stream, which is the "observable and reversible"
// bar the spec sets for engine decisions.
func (e *Engine) refusePickup(
	ctx context.Context, tx Tx, route EventRoute, ev PickupEvent,
	result EventPickupResult, reason, detail string,
) (EventPickupResult, error) {
	result.Refusal = reason
	result.Detail = detail
	_, err := tx.AppendEvent(ctx, route.RunID, event(TypeEventPickupRefused, map[string]any{
		"run_id":     route.RunID,
		"node_id":    route.TargetNode,
		"route_id":   route.ID,
		"event_id":   ev.ID,
		"event_name": ev.Name,
		"reason":     reason,
		"detail":     detail,
	}))
	return result, err
}

// checkPickupBounds applies §9.7 to a pickup. A pickup creates one token and
// consumes none, so the active-token charge is +1 — unlike a split's
// ActiveTokens-1+K, where the source token is consumed in the same
// transaction (design §5.1).
func checkPickupBounds(limits Limits, targetNode string, transitions, visits, activeTokens int) *BoundExceeded {
	if limits.MaxTransitions > 0 && transitions+1 > limits.MaxTransitions {
		return &BoundExceeded{
			Kind:   BoundTransitions,
			NodeID: targetNode,
			Limit:  strconv.Itoa(limits.MaxTransitions),
			Actual: strconv.Itoa(transitions + 1),
		}
	}
	if limits.MaxVisitsPerNode > 0 && visits+1 > limits.MaxVisitsPerNode {
		return &BoundExceeded{
			Kind:   BoundVisits,
			NodeID: targetNode,
			Limit:  strconv.Itoa(limits.MaxVisitsPerNode),
			Actual: strconv.Itoa(visits + 1),
		}
	}
	if limits.MaxParallelTokens > 0 && activeTokens+1 > limits.MaxParallelTokens {
		return &BoundExceeded{
			Kind:   BoundParallelTokens,
			NodeID: targetNode,
			Limit:  strconv.Itoa(limits.MaxParallelTokens),
			Actual: strconv.Itoa(activeTokens + 1),
		}
	}
	return nil
}

// evaluatePickupGuard evaluates an event edge's guard. `event` carries the
// delivered fact; `output` is empty, because a pickup has no producing node
// to have produced one, and `outcome` is empty for the same reason. Stating
// the absence as an empty value rather than omitting the binding keeps a
// guard that reaches for them reporting "no such key" instead of failing to
// evaluate at all.
func evaluatePickupGuard(edge *Edge, runInput json.RawMessage, ev PickupEvent) (bool, error) {
	activation := map[string]any{
		celVarInput:   decodeActivation(runInput),
		celVarOutput:  map[string]any{},
		celVarOutcome: "",
		celVarEvent: map[string]any{
			"name":    ev.Name,
			"emitter": ev.Emitter,
			"payload": decodeActivation(ev.Payload),
		},
	}
	out, _, err := edge.Guard.Eval(activation)
	if err != nil {
		return false, err
	}
	return truthy(out)
}

// eventEdgeFor finds the pinned edge a materialized route came from. Routes
// are created one per event edge, and an edge is identified by the triple the
// route row carries — event name, target, and guard text — because a workflow
// may legitimately declare two edges from one event to one node under
// different guards.
func (w *Workflow) eventEdgeFor(route EventRoute) *Edge {
	for i := range w.Edges {
		e := &w.Edges[i]
		if e.OnEvent == route.EventName && e.To == route.TargetNode && e.When == route.Guard {
			return e
		}
	}
	return nil
}

// materializeEventRoutes writes the run's durable pickup routes, one per
// `onEvent` edge of the pinned IR. It runs inside CreateRun's transaction, so
// a committed run always has exactly the routes its definition declares —
// there is no window in which a run exists but cannot be picked up.
func (e *Engine) materializeEventRoutes(ctx context.Context, tx Tx, wf *Workflow, run Run, now time.Time) error {
	for _, edge := range wf.EventEdges() {
		if err := tx.InsertEventRoute(ctx, EventRoute{
			ID:          e.newID(),
			NamespaceID: run.NamespaceID,
			RunID:       run.ID,
			EventName:   edge.OnEvent,
			TargetNode:  edge.To,
			Guard:       edge.When,
			Status:      EventRouteActive,
			CreatedAt:   now,
		}); err != nil {
			return err
		}
	}
	return nil
}
