// Node lifecycle (task t12, spec c7/h40, c82/h55, c83/h56, c49/h33): a
// declaration_nodes row is the stop between an action being called and a
// trigger capturing the reaction. It stays visibly open until something
// closes it -- a reaction consumed it, its deadline expired, it was
// orphaned by an upgrade, or its reacting declaration hit its re-entry
// limit -- and every one of those closing paths is queryable afterward.
//
// This file hooks into Engine.Handle and Engine.evaluate (engine.go) at the
// smallest points that need it: deriving a reaction's start node from the
// landing node its parent firing opened (rather than trusting whatever the
// caller passed), and closing that node once the reaction is decided one
// way or the other. Every hook is reached through a NodeBackend type
// assertion on e.backend, the same optional-capability pattern Handle
// already uses for Dispatcher's PrepareOrigin: a Backend that does not
// implement it (the in-memory fakes the t9 unit tests use) runs the
// ordinary firing loop with no lifecycle side effects at all.
package declengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store"
)

// Node states (declaration_nodes.state, migrations/0060_firings.sql). This
// package only ever reads or writes 'open' and 'closed'; 'frozen' belongs to
// a different task's switch-freeze surface.
const (
	NodeStateOpen   = "open"
	NodeStateClosed = "closed"
)

// Node closing reasons (h40): "it closes with a recorded reason (orphaned,
// timed out, loop limit)". h40's prose says "timed out"; this package spells
// it "expired" to match c82/h55's own vocabulary for the deadline event
// (node.expired) -- the two name the same closing path.
const (
	NodeReasonConsumed          = "consumed"
	NodeReasonExpired           = "expired"
	NodeReasonOrphanedByUpgrade = "orphaned by upgrade"
	NodeReasonLoopLimited       = "loop-limited"
)

// action.* trigger kinds (c83/h56, internal/decl/kinds.Triggers). A
// declaration can take any of these as its own trigger, routing a failed
// action separately from the domain reaction the acting declaration was
// waiting for (PRD §3.4: domain outcome is not technical status).
const (
	ActionTriggerFailed            = "action.failed"
	ActionTriggerTimedOut          = "action.timed_out"
	ActionTriggerRejected          = "action.rejected"
	ActionTriggerCapacityExhausted = "action.capacity_exhausted"
)

// nodeLifecycleDeclarationID/Version name the reserved pseudo-declaration a
// node-lifecycle record is filed under on declaration_evaluations, whose
// declaration_id and declaration_version columns are NOT NULL. This mirrors
// PostgresMarkerStore.RecordMarkerRejection's "origin-marker" pseudo-id: a
// note this package writes describes the LIFECYCLE deciding something (a
// late reaction, an orphan), not any one declaration's own evaluation of an
// event.
const (
	nodeLifecycleDeclarationID = "node-lifecycle"
	nodeLifecycleVersion       = "v1"
)

// Node-lifecycle evaluation outcomes, recorded on declaration_evaluations
// under the reserved pseudo-declaration above.
const (
	OutcomeNodeClosed  = "node closed"
	OutcomeNodeOrphan  = "orphaned by upgrade"
	OutcomeNodeExpired = "node expired"
	// OutcomeParentNoLandingNode (task t38g, review finding A1): the event's
	// marker verified to a firing that never opened a landing node -- its
	// dispatch failed after the run was created, so Finish never ran, yet
	// the run may have executed and exposed the marker. There is no node
	// to arrive at, and the payload's `node` is never taken instead, so the
	// reaction is recorded and nothing fires.
	OutcomeParentNoLandingNode = "parent opened no landing node"
)

// OutcomeReservedEventRejected (task t38g, review finding A2) is Handle's
// refusal of a control-plane event name (kinds.ReservedEvent) that the
// control plane did not emit. It is filed under its own reserved
// pseudo-declaration, reservedEventDeclarationID, beside "origin-marker"
// and "node-lifecycle".
const (
	OutcomeReservedEventRejected = "reserved event rejected"
	reservedEventDeclarationID   = "reserved-event"
)

// NodeRecord is one queryable declaration_nodes row (h40): its open/closed
// state and, once closed, why. ReactorDeclarationID/Version are empty when
// no active declaration reacted to this node's name at open time.
type NodeRecord struct {
	ID, NamespaceID, OpeningFiringID, Name          string
	State, ClosedReason                             string
	Deadline                                        time.Time
	ReactorDeclarationID, ReactorDeclarationVersion string
	// OpeningDeclarationID is the declaration whose firing opened this node
	// -- the one whose per-subject slot frees when it closes (task t38's
	// drain-on-close seam, drainAfterClose). Empty from a backend that does
	// not join it (the in-memory test fakes).
	OpeningDeclarationID string
	// Host and ActorKind are the node's engine-derived types (task t38d,
	// nodetypes.go): empty when the engine recorded no such fact.
	Host, ActorKind string
}

// ExpiredNode is one node.expired emission ExpireDue produced.
type ExpiredNode struct {
	NamespaceID, NodeID, NodeName, EventID string
}

// ActionResultEvent is one action.* emission EmitActionResults produced.
type ActionResultEvent struct {
	NamespaceID, FiringID, NodeID, NodeName, EventID, Trigger, Class string
}

// NodeBackend is the node-lifecycle surface a Backend can optionally
// implement; see this file's package doc comment for how Engine detects it.
type NodeBackend interface {
	// NodeByFiring returns the node the given firing opened, when one
	// exists. found is false for a firing that never reached Finish (no
	// landing node was ever opened for it).
	NodeByFiring(ctx context.Context, namespaceID, firingID string) (NodeRecord, bool, error)
	// NodeStatus returns one node by its own id (h40's "queryable" half).
	NodeStatus(ctx context.Context, namespaceID, nodeID string) (NodeRecord, bool, error)
	// RecordReactor snapshots, at open time, which currently active
	// declaration (if any) reacts to the firing's landing node.
	RecordReactor(ctx context.Context, namespaceID, firingID, declarationID, declarationVersion string) error
	// CloseNode transitions nodeID from open to closed with reason,
	// atomically: it can only ever succeed once per node. ok is false when
	// the node was already closed (or does not exist) -- never an error.
	CloseNode(ctx context.Context, namespaceID, nodeID, reason string) (bool, error)
	// RecordNodeNote appends a declaration_evaluations row under the
	// reserved node-lifecycle pseudo-declaration: a late reaction, or the
	// detail of an orphan-by-upgrade closing.
	RecordNodeNote(ctx context.Context, namespaceID, eventID, outcome, reason string) error
	// ExpireDue closes every open node in namespaceID whose deadline is at
	// or before now, emitting one node.expired signal event per node
	// closed. now is caller-supplied (a fake clock in tests) rather than
	// read from time.Now() here, so expiry is deterministic and testable.
	ExpireDue(ctx context.Context, namespaceID string, now time.Time, limit int) ([]ExpiredNode, error)
	// EmitActionResults emits exactly one action.* signal event per failed
	// firing that has not already had one, mapped from the firing's run's
	// terminal §13.5 error class (internal/actors).
	EmitActionResults(ctx context.Context, namespaceID string, limit int) ([]ActionResultEvent, error)
}

// deriveNode resolves event.Node from the landing node the parent firing
// opened (h40: "a reaction to a firing's action starts from the landing
// node that firing opened"), when the backend supports node lifecycle and
// the event carries a verified parent. A fresh event with no verified
// parent keeps whatever Node the caller supplied -- there is nothing to
// derive it from.
//
// continue_ reports whether Handle should keep evaluating this event at
// all: a reaction against a node that has already closed is recorded (c82:
// "a late reaction after expiry is recorded, not fired") and Handle stops
// there, matching nothing. So is a reaction whose verified parent opened
// no landing node at all (task t38g): there is no node for it to arrive at.
func (e *Engine) deriveNode(ctx context.Context, event *Event, parent string) (node NodeRecord, found bool, continue_ bool, err error) {
	nb, ok := e.backend.(NodeBackend)
	if !ok || parent == "" {
		return NodeRecord{}, false, true, nil
	}
	// t38d: record the landing node's engine-derived types (a no-op once
	// set, or when the node is not open) before reading it for matching.
	if err := e.recordNodeTypes(ctx, event.NamespaceID, parent); err != nil {
		return NodeRecord{}, false, false, err
	}
	node, found, err = nb.NodeByFiring(ctx, event.NamespaceID, parent)
	if err != nil {
		return NodeRecord{}, false, false, err
	}
	if !found {
		// t38g (A1): before t38d the payload's node was taken here, and
		// t38d kept that as ClaimedNode -- which let anyone holding the
		// exposed marker of a firing that opened no node pick any start
		// node. The node is never taken from the payload.
		reason := fmt.Sprintf("verified parent firing %s opened no landing node; reaction recorded, not fired", parent)
		return NodeRecord{}, false, false, nb.RecordNodeNote(ctx, event.NamespaceID, event.ID, OutcomeParentNoLandingNode, reason)
	}
	event.Node = node.Name
	// c81: an event arriving for a frozen node is stored against it, not
	// matched/evaluated and not recorded as an ordinary late reaction --
	// freeze.go's ThawAndReplay is what eventually runs it through Handle,
	// once the node thaws. A backend with node lifecycle but no freeze
	// support (should not occur outside a hand-built test fake, since only
	// freeze.go ever produces this state) falls through to the same
	// already-closed handling below.
	if node.State == NodeStateFrozen {
		if fb, ok := nb.(FreezeBackend); ok {
			_, err := fb.StoreFrozenEvent(ctx, event.NamespaceID, node.ID, *event)
			return node, true, false, err
		}
	}
	if node.State != NodeStateOpen {
		reason := fmt.Sprintf("node %q already closed (%s); reaction recorded, not fired", node.Name, node.ClosedReason)
		return node, true, false, nb.RecordNodeNote(ctx, event.NamespaceID, event.ID, OutcomeNodeClosed, reason)
	}
	// The event legitimately arrived at this open node: start_from
	// declarations match against the types the engine recorded on it.
	event.arrival = nodeArrival{opened: true, host: node.Host, actorKind: node.ActorKind}
	return node, true, true, nil
}

// checkOrphan runs after Handle's matching loop found no active declaration
// for a reaction against an open, derived node (c49/h33). A node with no
// recorded reactor never had one to lose -- an ordinary terminal node, not
// an orphan. A node whose recorded reactor is still active, at the same
// version, simply did not match this particular event (a `with` filter, a
// different trigger kind); that is not an orphan either. Only a reactor
// that is gone or upgraded closes the node.
//
// closed reports whether THIS call closed the node, so Handle can drain the
// opening declaration's deferred subjects once the close has committed.
func checkOrphan(ctx context.Context, nb NodeBackend, active []ActiveDeclaration, namespaceID, eventID string, node NodeRecord) (closed bool, err error) {
	if node.ReactorDeclarationID == "" {
		return false, nil
	}
	currentVersion := ""
	for _, a := range active {
		if a.ID == node.ReactorDeclarationID {
			currentVersion = a.VersionID
			break
		}
	}
	if currentVersion == node.ReactorDeclarationVersion {
		return false, nil
	}
	now := currentVersion
	verb := "upgraded to " + now
	if now == "" {
		verb = "removed"
	}
	closed, err = nb.CloseNode(ctx, namespaceID, node.ID, NodeReasonOrphanedByUpgrade)
	if err != nil || !closed {
		return false, err
	}
	reason := fmt.Sprintf("reacting declaration %s was %s (was %s)", node.ReactorDeclarationID, verb, node.ReactorDeclarationVersion)
	return true, nb.RecordNodeNote(ctx, namespaceID, eventID, OutcomeNodeOrphan, reason)
}

// closeNodeOpenedBy closes the node parentFiring opened, when one exists,
// is still open, and the backend supports node lifecycle. It is a silent
// no-op otherwise: a node the caller never derived from (a fresh, parentless
// event; a memoryBackend test fake) has nothing here to close.
func (e *Engine) closeNodeOpenedBy(ctx context.Context, namespaceID, parentFiring, reason string) error {
	if parentFiring == "" {
		return nil
	}
	nb, ok := e.backend.(NodeBackend)
	if !ok {
		return nil
	}
	node, found, err := nb.NodeByFiring(ctx, namespaceID, parentFiring)
	if err != nil || !found || node.State != NodeStateOpen {
		return err
	}
	closed, err := nb.CloseNode(ctx, namespaceID, node.ID, reason)
	if err != nil || !closed {
		return err
	}
	return e.drainAfterClose(ctx, namespaceID, node)
}

// drainAfterClose is the t10 x t12 seam (task t38): a node closing frees one
// in-flight slot of the declaration whose firing opened it (SubjectInFlight
// counts exactly those open nodes), so that declaration's oldest per-subject
// deferral is replayed now rather than waiting for a new event that may
// never come. Every closing path calls it -- consumed and loop-limited
// (closeNodeOpenedBy), orphaned (Handle, after checkOrphan) and expired
// (ExpireDue) -- and only once CloseNode (or ExpireDue's per-node
// transaction) has committed, so the replay's own Handle call never runs
// under a lock the close still holds. A node closed by a concurrent caller
// is not this call's slot to spend: callers only drain when THEIR close won.
func (e *Engine) drainAfterClose(ctx context.Context, namespaceID string, node NodeRecord) error {
	if node.OpeningDeclarationID == "" {
		return nil
	}
	return e.DrainSubject(ctx, namespaceID, node.OpeningDeclarationID)
}

// recordReactorIfAny snapshots, right after a landing node opens, whichever
// currently active declaration (if any) reacts to it by name (c49). It
// re-reads Active rather than threading it through evaluate's signature,
// since evaluate is exercised directly (with its original 5-argument
// signature) by tests elsewhere in this package.
func (e *Engine) recordReactorIfAny(ctx context.Context, namespaceID, firingID, landingNodeName string) error {
	nb, ok := e.backend.(NodeBackend)
	if !ok {
		return nil
	}
	active, err := e.backend.Active(ctx, namespaceID)
	if err != nil {
		return err
	}
	for _, a := range active {
		if a.Declaration.StartNode.Name == landingNodeName {
			return nb.RecordReactor(ctx, namespaceID, firingID, a.ID, a.VersionID)
		}
	}
	return nil
}

// ExpireDue closes every open node in namespaceID whose deadline is at or
// before now (c82/h55), emitting node.expired exactly once per node --
// CloseNode's guarded transition is what makes "exactly once" true, not a
// check performed here -- and runs each emitted event back through Handle
// so any declaration that takes node.expired as its trigger can fire. now is
// caller-supplied so a scheduler (or a test) drives expiry with its own
// clock rather than this package reading time.Now() internally.
func (e *Engine) ExpireDue(ctx context.Context, namespaceID string, now time.Time, limit int) ([]ExpiredNode, error) {
	nb, ok := e.backend.(NodeBackend)
	if !ok {
		return nil, nil
	}
	expired, err := nb.ExpireDue(ctx, namespaceID, now, limit)
	if err != nil {
		return expired, err
	}
	var failures []error
	for _, ev := range expired {
		if err := e.Handle(ctx, Event{NamespaceID: namespaceID, ID: ev.EventID, Kind: "node.expired", Node: ev.NodeName, Emitter: DeclarationEngineActorID}); err != nil {
			failures = append(failures, err)
		}
		// nb.ExpireDue committed this node's close before returning it, so
		// its opening declaration's freed slot can be spent now (t38).
		node, found, err := nb.NodeStatus(ctx, namespaceID, ev.NodeID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if found {
			if err := e.drainAfterClose(ctx, namespaceID, node); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return expired, errors.Join(failures...)
}

// EmitActionResults emits exactly one action.* trigger event per firing
// whose dispatched run reached a terminal, non-succeeded technical status
// and has not already had one (c83/h56), then runs each through Handle so a
// declaration routing action.failed (etc.) can fire. Scope is deliberately
// runs.status = 'failed': a run the engine cancelled through its own
// governance (a barrier's losing branch, an operator cancel) is not an
// actor or runner reporting a bad outcome, and RunCancelled already has its
// own recorded reason on the run itself.
func (e *Engine) EmitActionResults(ctx context.Context, namespaceID string, limit int) ([]ActionResultEvent, error) {
	nb, ok := e.backend.(NodeBackend)
	if !ok {
		return nil, nil
	}
	results, err := nb.EmitActionResults(ctx, namespaceID, limit)
	if err != nil {
		return results, err
	}
	var failures []error
	for _, r := range results {
		if err := e.Handle(ctx, Event{NamespaceID: namespaceID, ID: r.EventID, Kind: r.Trigger, Node: r.NodeName, Emitter: DeclarationEngineActorID}); err != nil {
			failures = append(failures, err)
		}
	}
	return results, errors.Join(failures...)
}

// actionTriggerFor maps an internal/actors §13.5 error class (and, as a
// fallback when no class was recorded, the engine's own §3.4 technical
// status) onto its dedicated action.* trigger (c83/h56).
//
// The mapping is by what the four names mean, not by an attempt to give
// every one of §13.5's eleven classes its own bucket -- there are only four
// action.* kinds (internal/decl/kinds.go), so this is deliberately lossy the
// same way actors.TechStatusFor already collapses several classes onto one
// §3.4 status: capacity_exhausted gets its own trigger because it is the one
// class §13.5 itself carved out as needing a response above the attempt
// (actors.ClassCapacityExhausted's doc comment); timeout gets its own
// trigger because "the deadline passed" is exactly what a declaration
// reacting to it needs to know; actor_rejected_input, contract and
// auth_or_policy all become action.rejected because each is the actor or
// the protocol refusing the request as sent -- a credential, a body, a
// response shape -- rather than an execution that ran and failed; every
// other class (retryable_transport, rate_limited, actor_unavailable,
// execution, cancelled, credential_spent, and anything an actor invented)
// becomes action.failed, the catch-all a declaration can always route on
// even when it cannot distinguish the finer cause.
func actionTriggerFor(class, techStatus string) (trigger string) {
	switch actors.ErrorClass(class) {
	case actors.ClassCapacityExhausted:
		return ActionTriggerCapacityExhausted
	case actors.ClassTimeout:
		return ActionTriggerTimedOut
	case actors.ClassActorRejectedInput, actors.ClassContract, actors.ClassAuthOrPolicy:
		return ActionTriggerRejected
	case "":
		// No bridge-classified error body was recorded (a hook rejection,
		// a refused binding); fall back to the coarser technical status.
		switch techStatus {
		case "timed_out":
			return ActionTriggerTimedOut
		case "contract_rejected", "policy_denied":
			return ActionTriggerRejected
		}
	}
	return ActionTriggerFailed
}

// PostgresBackend node-lifecycle methods (declaration_nodes,
// migrations/0060_firings.sql and 0065_declaration_node_lifecycle.sql).

const nodeColumns = `id,namespace_id,opening_firing_id,node_name,state,COALESCE(closed_reason,''),
	deadline,COALESCE(reactor_declaration_id,''),COALESCE(reactor_declaration_version,''),
	COALESCE((SELECT f.declaration_id FROM declaration_firings f
	  WHERE f.namespace_id=declaration_nodes.namespace_id AND f.id=declaration_nodes.opening_firing_id),''),
	COALESCE(host,''),COALESCE(actor_kind,'')`

// scanNode reads deadline through a nullable pointer -- a node declared
// with deadline "none" (landingDeadline's zero Duration) never has one, and
// a Go time.Time cannot represent SQL NULL directly the way a pointer can.
func scanNode(row interface{ Scan(...any) error }) (NodeRecord, error) {
	var n NodeRecord
	var deadline *time.Time
	err := row.Scan(&n.ID, &n.NamespaceID, &n.OpeningFiringID, &n.Name, &n.State, &n.ClosedReason,
		&deadline, &n.ReactorDeclarationID, &n.ReactorDeclarationVersion, &n.OpeningDeclarationID, &n.Host, &n.ActorKind)
	if err != nil {
		return NodeRecord{}, err
	}
	if deadline != nil {
		n.Deadline = *deadline
	}
	return n, nil
}

// NodeByFiring returns the node opening_firing_id opened, when one exists.
func (p PostgresBackend) NodeByFiring(ctx context.Context, namespaceID, firingID string) (NodeRecord, bool, error) {
	n, err := scanNode(p.Store.Pool().QueryRow(ctx, `SELECT `+nodeColumns+` FROM declaration_nodes WHERE namespace_id=$1 AND opening_firing_id=$2`, namespaceID, firingID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return NodeRecord{}, false, nil
		}
		return NodeRecord{}, false, err
	}
	return n, true, nil
}

// NodeStatus returns one node by its own id (h40).
func (p PostgresBackend) NodeStatus(ctx context.Context, namespaceID, nodeID string) (NodeRecord, bool, error) {
	n, err := scanNode(p.Store.Pool().QueryRow(ctx, `SELECT `+nodeColumns+` FROM declaration_nodes WHERE namespace_id=$1 AND id=$2`, namespaceID, nodeID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return NodeRecord{}, false, nil
		}
		return NodeRecord{}, false, err
	}
	return n, true, nil
}

// RecordReactor snapshots, at open time, the declaration (if any) whose
// start node names this landing node (c49).
func (p PostgresBackend) RecordReactor(ctx context.Context, namespaceID, firingID, declarationID, declarationVersion string) error {
	_, err := p.Store.Pool().Exec(ctx, `UPDATE declaration_nodes SET reactor_declaration_id=$3,reactor_declaration_version=$4,updated_at=now()
		WHERE namespace_id=$1 AND opening_firing_id=$2`, namespaceID, firingID, declarationID, declarationVersion)
	return err
}

// CloseNode transitions nodeID from open to closed, guarded so it can only
// ever succeed once per node (h40, h55's "exactly once" for the expiry
// path, and every other closing path besides).
func (p PostgresBackend) CloseNode(ctx context.Context, namespaceID, nodeID, reason string) (bool, error) {
	tag, err := p.Store.Pool().Exec(ctx, `UPDATE declaration_nodes SET state='closed',closed_reason=$3,updated_at=now()
		WHERE namespace_id=$1 AND id=$2 AND state='open'`, namespaceID, nodeID, reason)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RecordNodeNote appends a declaration_evaluations row under the reserved
// node-lifecycle pseudo-declaration: a late reaction against an already-
// closed node, or the detail of an orphan-by-upgrade closing.
// declaration_evaluations.event_id is NOT NULL, so eventID is always the id
// of the reaction event that revealed the note-worthy fact.
func (p PostgresBackend) RecordNodeNote(ctx context.Context, namespaceID, eventID, outcome, reason string) error {
	_, err := p.Store.Pool().Exec(ctx, `INSERT INTO declaration_evaluations
		(id,namespace_id,event_id,declaration_id,declaration_version,outcome,reason)
		VALUES($1,$2,$3,$4,$5,$6,$7)`,
		store.NewULID(), namespaceID, eventID, nodeLifecycleDeclarationID, nodeLifecycleVersion, outcome, reason)
	return err
}

// dueNodesSQL borrows claimDueTimersSQL's FOR UPDATE SKIP LOCKED shape
// (internal/store/postgres/timers.go) over declaration_nodes' own due index
// (declaration_nodes_due_idx, migrations/0060_firings.sql) instead of the
// shared timers table: a declaration node is not a node_runs row, so it has
// no honest run_id/node_run_id to schedule a timers.go row under, and
// sharing timers' due-scan with the graph engine's own scheduler would let
// the two independent pollers race each other's rows. ScheduleTimer /
// CancelTimer remain unused here for that reason; this file's own guarded
// UPDATE is what "reuse the scheduler timer machinery" means for a subject
// that table cannot honestly describe.
const dueNodesSQL = `SELECT id,node_name FROM declaration_nodes
	WHERE namespace_id=$1 AND state='open' AND deadline IS NOT NULL AND deadline<=$2
	ORDER BY deadline,id LIMIT $3`

// ExpireDue closes every open node in namespaceID whose deadline is at or
// before now, emitting node.expired exactly once per node it closes.
func (p PostgresBackend) ExpireDue(ctx context.Context, namespaceID string, now time.Time, limit int) ([]ExpiredNode, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.Store.Pool().Query(ctx, dueNodesSQL, namespaceID, now, limit)
	if err != nil {
		return nil, err
	}
	type candidate struct{ id, name string }
	var due []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.name); err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var out []ExpiredNode
	for _, c := range due {
		tx, err := p.Store.Pool().Begin(ctx)
		if err != nil {
			return out, err
		}
		tag, err := tx.Exec(ctx, `UPDATE declaration_nodes SET state='closed',closed_reason=$3,deadline_notified_at=now(),updated_at=now()
			WHERE namespace_id=$1 AND id=$2 AND state='open'`, namespaceID, c.id, NodeReasonExpired)
		if err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if tag.RowsAffected() != 1 {
			// Already closed by a concurrent caller (or this one, on a
			// retried batch): not this call's emission to make.
			_ = tx.Rollback(ctx)
			continue
		}
		eventID := store.NewULID()
		payload, err := json.Marshal(map[string]any{"node_id": c.id, "node_name": c.name})
		if err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO signal_events(id,namespace_id,name,payload,emitter) VALUES($1,$2,$3,$4,$5)`,
			eventID, namespaceID, "node.expired", payload, DeclarationEngineActorID); err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if err := tx.Commit(ctx); err != nil {
			return out, err
		}
		out = append(out, ExpiredNode{NamespaceID: namespaceID, NodeID: c.id, NodeName: c.name, EventID: eventID})
	}
	return out, nil
}

// actionResultCandidatesSQL finds firings whose dispatched run reached
// runs.status='failed' and has not yet had an action.* signal event emitted
// for it (the NOT EXISTS guard; actionResultAdvisoryLock below closes the
// race between two callers both passing it before either commits).
const actionResultCandidatesSQL = `SELECT r.id,dn.id,dn.node_name FROM runs r
	JOIN declaration_nodes dn ON dn.namespace_id=r.namespace_id AND dn.opening_firing_id=r.id
	WHERE r.namespace_id=$1 AND r.status='failed'
	AND NOT EXISTS (SELECT 1 FROM signal_events se WHERE se.namespace_id=r.namespace_id AND se.run_id=r.id AND se.name LIKE 'action.%')
	ORDER BY r.id LIMIT $2`

// EmitActionResults emits exactly one action.* trigger per candidate firing
// (c83/h56), mapped from the last attempt of the run's single "action" node
// (internal/declengine/dispatch.go's workerEnvelope always compiles the
// firing's action into a node literally named "action").
func (p PostgresBackend) EmitActionResults(ctx context.Context, namespaceID string, limit int) ([]ActionResultEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.Store.Pool().Query(ctx, actionResultCandidatesSQL, namespaceID, limit)
	if err != nil {
		return nil, err
	}
	type candidate struct{ runID, nodeID, nodeName string }
	var due []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.runID, &c.nodeID, &c.nodeName); err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var out []ActionResultEvent
	for _, c := range due {
		tx, err := p.Store.Pool().Begin(ctx)
		if err != nil {
			return out, err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "decl-action-result:"+namespaceID+":"+c.runID); err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		var already bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM signal_events WHERE namespace_id=$1 AND run_id=$2 AND name LIKE 'action.%')`,
			namespaceID, c.runID).Scan(&already); err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if already {
			_ = tx.Rollback(ctx)
			continue
		}
		var status string
		var class string
		err = tx.QueryRow(ctx, `SELECT a.status,COALESCE(a.result->'error'->>'class','') FROM node_runs nr
			JOIN attempts a ON a.node_run_id=nr.id
			WHERE nr.namespace_id=$1 AND nr.run_id=$2 AND nr.node_key='action'
			ORDER BY a.attempt_number DESC LIMIT 1`, namespaceID, c.runID).Scan(&status, &class)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				_ = tx.Rollback(ctx)
				continue
			}
			_ = tx.Rollback(ctx)
			return out, err
		}
		trigger := actionTriggerFor(class, status)
		eventID := store.NewULID()
		payload, err := json.Marshal(map[string]any{"firing_id": c.runID, "node_id": c.nodeID, "status": status, "class": class})
		if err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO signal_events(id,namespace_id,run_id,name,payload,emitter) VALUES($1,$2,$3,$4,$5,$6)`,
			eventID, namespaceID, c.runID, trigger, payload, DeclarationEngineActorID); err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if err := tx.Commit(ctx); err != nil {
			return out, err
		}
		out = append(out, ActionResultEvent{NamespaceID: namespaceID, FiringID: c.runID, NodeID: c.nodeID, NodeName: c.nodeName, EventID: eventID, Trigger: trigger, Class: class})
	}
	return out, nil
}
