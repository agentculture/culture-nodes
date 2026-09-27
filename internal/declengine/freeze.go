// freeze.go: rollback freeze and replay (task t18, #328; spec c81, honesty
// h54; ADR 0014 "Consequences"). switch.go built the global before/shadow/
// after switch and its FlipHook seam; nodes.go built node lifecycle around
// 'open' and 'closed'. This file is what makes flipping the switch back to
// 'before' SAFE for whatever declaration nodes are still open when it
// lands, and what makes flipping it forward again resume them without
// losing or double-firing anything (h54: "a flip back and forward again
// loses no open node and dispatches no action twice").
//
// The shape:
//
//   - Freezing (flip -> 'before') is a FlipHook (FreezeHook / FreezeHookAt),
//     so it runs inside the SAME transaction as the flip that ordered it
//     (switch.go's FlipHook contract). There is never a window where the
//     switch already reads 'before' but a node is still open and unfrozen.
//     Every open node in the namespace moves to 'frozen', with the time
//     remaining on its deadline (if any) preserved in
//     declaration_nodes.deadline_remaining and its deadline cleared. Clearing
//     deadline is the pause mechanism: nodes.go's ExpireDue only ever
//     selects rows where deadline IS NOT NULL (dueNodesSQL), so a frozen
//     node is excluded from expiry by construction -- ExpireDue never needs
//     to know the word "frozen" exists.
//
//   - An event arriving for a frozen node never reaches the ordinary firing
//     loop: nodes.go's deriveNode, which already had to tell an open node
//     apart from a closed one, now also tells a frozen one apart from
//     both. Instead of matching/evaluating, the whole event is stored
//     against the node (StoreFrozenEvent), deduplicated by (node, event
//     id) -- a provider redelivery of the same event id while still frozen
//     is a silent no-op, never a second stored copy.
//
//   - Thawing (flip -> 'after') is deliberately NOT a FlipHook. FlipHook
//     runs before the flip's transaction commits, but replay dispatches
//     real actions through the dispatcher chain (switch.go's ShadowGate),
//     and ShadowGate.Dispatch reads the switch's mode through the pool, not
//     the flip's own transaction -- it would still see the OLD mode inside
//     an uncommitted hook. So thawing is ThawAndReplay, a function the
//     caller runs after SwitchStore.Flip(..., ModeAfter, ...) has already
//     returned successfully. It moves every 'frozen' node back to 'open',
//     restores each one's deadline from its paused remaining duration, and
//     replays that node's stored events, oldest arrival order first,
//     through the ordinary Engine.Handle path -- mirroring guard.go's
//     DrainSubject, nothing about a replay is privileged: whatever is
//     currently active, and whatever the switch's mode now is, decide the
//     replayed event's fate exactly as a fresh delivery would. A stored
//     event is deleted only once its own replay call returns without
//     error, so a partial failure never loses events that have not yet
//     been replayed, and a retried ThawAndReplay never replays one twice.
//
//   - A later redelivery of a reaction's event id, after it has already
//     been replayed and fired, is not this file's problem to solve twice:
//     the node it targeted is now 'closed' (evaluate() closed it the
//     moment the replayed reaction fired), so deriveNode's existing
//     already-closed path records it and stops -- the same path an
//     ordinary late reaction against any closed node takes. Nothing fires.
package declengine

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// NodeStateFrozen is declaration_nodes.state's third value (0060's CHECK
// constraint already allows it). nodes.go's own package doc explicitly
// leaves this state to "a different task's switch-freeze surface" -- this
// file is that surface, and it is the only one that ever sets or clears it.
const NodeStateFrozen = "frozen"

// FreezeBackend is the freeze/replay capability a Backend can optionally
// implement, the same optional-capability pattern NodeBackend already uses
// (nodes.go's package doc comment). A Backend that does not implement it
// (the in-memory fakes the unit tests use) simply never freezes anything:
// nodes.go's deriveNode falls back to treating a would-be-frozen state as
// an ordinary already-closed node when no FreezeBackend is present, and
// FreezeHook / ThawAndReplay are no-ops for a nil FreezeBackend.
type FreezeBackend interface {
	// FreezeOpenNodes transitions every 'open' node in namespaceID to
	// 'frozen' inside tx -- the same transaction as the flip that ordered
	// it (c81). For a node with a deadline, however much time remained
	// (deadline - now, floored at zero so an already-overdue-but-not-yet-
	// expired node freezes cleanly) is preserved in deadline_remaining and
	// deadline itself is cleared; a node with no deadline keeps none.
	// Returns how many nodes it froze.
	FreezeOpenNodes(ctx context.Context, tx pgx.Tx, namespaceID string, now time.Time) (int, error)
	// ThawFrozenNodes transitions every 'frozen' node in namespaceID back
	// to 'open', restoring each one's deadline from its paused
	// deadline_remaining (now + remaining; a node frozen with no deadline
	// keeps none), and returns the thawed nodes so ThawAndReplay can walk
	// each one's stored events.
	ThawFrozenNodes(ctx context.Context, namespaceID string, now time.Time) ([]NodeRecord, error)
	// StoreFrozenEvent records one event that arrived for a frozen node,
	// in arrival order, deduplicated by (node id, event id): a redelivery
	// of the same event id while still frozen is a silent no-op. stored
	// reports whether this call actually recorded a new row.
	StoreFrozenEvent(ctx context.Context, namespaceID, nodeID string, event Event) (stored bool, err error)
	// FrozenEvents returns nodeID's stored events, oldest arrival order
	// first.
	FrozenEvents(ctx context.Context, namespaceID, nodeID string) ([]Event, error)
	// DeleteFrozenEvent removes one stored event once it has been
	// replayed, so a later freeze/thaw cycle for the same node never
	// replays it a second time.
	DeleteFrozenEvent(ctx context.Context, namespaceID, nodeID, eventID string) error
}

// FreezeHook is FreezeHookAt with the real wall clock -- what a caller
// wires into SwitchStore.Flip for an ordinary (non-test) flip to 'before'.
func FreezeHook(fb FreezeBackend) FlipHook {
	return FreezeHookAt(fb, time.Now)
}

// FreezeHookAt returns a FlipHook that freezes every open declaration node
// in the flipping namespace when (and only when) the switch is landing on
// 'before' -- whatever previous held. now is caller-supplied (nodes.go's
// ExpireDue already establishes this pattern for deterministic testing) so
// a test can freeze at an exact instant rather than racing time.Now(). A
// nil FreezeBackend or now yields a nil hook, which switch.go's Flip
// already treats as a safe no-op (it skips nil hooks).
func FreezeHookAt(fb FreezeBackend, now func() time.Time) FlipHook {
	if fb == nil || now == nil {
		return nil
	}
	return func(ctx context.Context, tx pgx.Tx, namespaceID, previous, next string) error {
		if next != ModeBefore {
			return nil
		}
		_, err := fb.FreezeOpenNodes(ctx, tx, namespaceID, now().UTC())
		return err
	}
}

// ThawAndReplay thaws every frozen node in namespaceID and replays each
// one's stored events, oldest arrival order first, through the ordinary
// Engine.Handle path. Call it once SwitchStore.Flip has already committed a
// flip to 'after' -- see this file's package doc for why thawing cannot be
// a FlipHook the way freezing is. A nil FreezeBackend is a safe no-op.
func ThawAndReplay(ctx context.Context, e *Engine, fb FreezeBackend, namespaceID string, now time.Time) error {
	if fb == nil {
		return nil
	}
	nodes, err := fb.ThawFrozenNodes(ctx, namespaceID, now)
	if err != nil {
		return err
	}
	var failures []error
	for _, node := range nodes {
		events, err := fb.FrozenEvents(ctx, namespaceID, node.ID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, ev := range events {
			if err := e.Handle(ctx, ev); err != nil {
				failures = append(failures, err)
				continue
			}
			// Only delete once replay of THIS event succeeded: a failure
			// partway through a node's queue leaves the remaining (and this
			// one, on a caller retry) events in place rather than losing them.
			if err := fb.DeleteFrozenEvent(ctx, namespaceID, node.ID, ev.ID); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

// PostgresBackend freeze/replay methods (declaration_nodes and
// declaration_node_frozen_events, migrations 0060 and 0066).

// FreezeOpenNodes implements FreezeBackend.
func (p PostgresBackend) FreezeOpenNodes(ctx context.Context, tx pgx.Tx, namespaceID string, now time.Time) (int, error) {
	tag, err := tx.Exec(ctx, `UPDATE declaration_nodes SET
		state='frozen',
		deadline_remaining = CASE WHEN deadline IS NOT NULL THEN GREATEST(deadline - $2, INTERVAL '0') ELSE NULL END,
		deadline = NULL,
		updated_at = now()
	 WHERE namespace_id=$1 AND state=$3`, namespaceID, now, NodeStateOpen)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ThawFrozenNodes implements FreezeBackend.
func (p PostgresBackend) ThawFrozenNodes(ctx context.Context, namespaceID string, now time.Time) ([]NodeRecord, error) {
	rows, err := p.Store.Pool().Query(ctx, `UPDATE declaration_nodes SET
		state='open',
		deadline = CASE WHEN deadline_remaining IS NOT NULL THEN $2::timestamptz + deadline_remaining ELSE NULL END,
		deadline_remaining = NULL,
		updated_at = now()
	 WHERE namespace_id=$1 AND state=$3
	 RETURNING `+nodeColumns, namespaceID, now, NodeStateFrozen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeRecord
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// freezeAdvisoryKey serializes concurrent StoreFrozenEvent calls for the
// same node so arrival_order assignment (MAX+1) never races two callers
// into the same value -- the same pg_advisory_xact_lock shape nodes.go's
// EmitActionResults already uses for its own per-run serialization.
func freezeAdvisoryKey(namespaceID, nodeID string) string {
	return "decl-freeze:" + namespaceID + ":" + nodeID
}

// StoreFrozenEvent implements FreezeBackend.
func (p PostgresBackend) StoreFrozenEvent(ctx context.Context, namespaceID, nodeID string, event Event) (bool, error) {
	tx, err := p.Store.Pool().Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, freezeAdvisoryKey(namespaceID, nodeID)); err != nil {
		return false, err
	}
	var already bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM declaration_node_frozen_events WHERE namespace_id=$1 AND node_id=$2 AND event_id=$3)`,
		namespaceID, nodeID, event.ID).Scan(&already); err != nil {
		return false, err
	}
	if already {
		return false, tx.Commit(ctx)
	}
	var next int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(arrival_order),0)+1 FROM declaration_node_frozen_events WHERE namespace_id=$1 AND node_id=$2`,
		namespaceID, nodeID).Scan(&next); err != nil {
		return false, err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO declaration_node_frozen_events(namespace_id,node_id,event_id,arrival_order,event) VALUES($1,$2,$3,$4,$5)`,
		namespaceID, nodeID, event.ID, next, payload); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// FrozenEvents implements FreezeBackend.
func (p PostgresBackend) FrozenEvents(ctx context.Context, namespaceID, nodeID string) ([]Event, error) {
	rows, err := p.Store.Pool().Query(ctx, `SELECT event FROM declaration_node_frozen_events
	 WHERE namespace_id=$1 AND node_id=$2 ORDER BY arrival_order`, namespaceID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// DeleteFrozenEvent implements FreezeBackend.
func (p PostgresBackend) DeleteFrozenEvent(ctx context.Context, namespaceID, nodeID, eventID string) error {
	_, err := p.Store.Pool().Exec(ctx, `DELETE FROM declaration_node_frozen_events WHERE namespace_id=$1 AND node_id=$2 AND event_id=$3`,
		namespaceID, nodeID, eventID)
	return err
}

// Compile-time assertion that PostgresBackend still satisfies both
// optional capabilities now that this file adds more methods to it.
var (
	_ NodeBackend   = PostgresBackend{}
	_ FreezeBackend = PostgresBackend{}
)
