package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// Event-route persistence and delivery-time matching (issue #43 task t21,
// migrations/0021_event_routes.sql; the parallel-tokens design §6.1).
//
// The write half is three engineQueries methods, so materializing a run's
// routes commits inside CreateRun's transaction and retiring them commits
// inside whichever transaction made the run terminal.
//
// The read half — matchEventRoutesUnderRunLocks — belongs to delivery, and
// the interesting decision is that it does NOT decide anything: it finds the
// active rows for (namespace, name), and the engine decides per row whether
// the guard passes, whether the run has bound headroom, and what dispatching
// the target node means (a work item, or an approval node's human task). The
// store would have had to reimplement all three to keep that logic here, and
// the third one is the design's own motivating scenario.

// InsertEventRoute materializes one `onEvent` edge as a run-scoped route.
func (eq engineQueries) InsertEventRoute(ctx context.Context, route engine.EventRoute) error {
	status := route.Status
	if status == "" {
		status = engine.EventRouteActive
	}
	_, err := eq.q.Exec(ctx, `
		INSERT INTO event_routes (id, namespace_id, run_id, event_name, target_node, guard, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, route.ID, eq.namespaceID, route.RunID, route.EventName, route.TargetNode,
		textOrNull(route.Guard), status, tsOrNow(route.CreatedAt))
	if err != nil {
		return fmt.Errorf("postgres: engine: InsertEventRoute: %w", err)
	}
	return nil
}

// RetireEventRoutes stops a run observing. It is idempotent by construction —
// the WHERE clause matches only active rows — so a run that fails, is
// cancelled, and is then read again retires its routes exactly once.
func (eq engineQueries) RetireEventRoutes(ctx context.Context, runID string) (int, error) {
	tag, err := eq.q.Exec(ctx, `
		UPDATE event_routes SET status = 'retired', retired_at = now()
		WHERE run_id = $1 AND status = 'active'
	`, runID)
	if err != nil {
		return 0, fmt.Errorf("postgres: engine: RetireEventRoutes: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ActiveEventRoutes lists a run's live routes in a stable order.
func (eq engineQueries) ActiveEventRoutes(ctx context.Context, runID string) ([]engine.EventRoute, error) {
	rows, err := eq.q.Query(ctx, `
		SELECT `+eventRouteColumns+`
		FROM event_routes WHERE run_id = $1 AND status = 'active'
		ORDER BY event_name, target_node, id
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: ActiveEventRoutes: %w", err)
	}
	routes, err := scanEventRoutes(rows)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: ActiveEventRoutes: %w", err)
	}
	return routes, nil
}

const eventRouteColumns = `id, namespace_id, run_id, event_name, target_node, guard, status, created_at`

func scanEventRoutes(rows pgx.Rows) ([]engine.EventRoute, error) {
	defer rows.Close()
	var out []engine.EventRoute
	for rows.Next() {
		var (
			route     engine.EventRoute
			guard     pgtype.Text
			createdAt pgtype.Timestamptz
		)
		if err := rows.Scan(&route.ID, &route.NamespaceID, &route.RunID, &route.EventName,
			&route.TargetNode, &guard, &route.Status, &createdAt); err != nil {
			return nil, err
		}
		route.Guard = textOrEmpty(guard)
		route.CreatedAt = tsValue(createdAt)
		out = append(out, route)
	}
	return out, rows.Err()
}

// selectCandidateEventRoutesSQL is the unlocked first read: which runs' locks
// a delivery must take before it can act on routes. Its authoritative twin is
// selectLockedEventRoutesSQL, re-run under those locks — the same two-phase
// shape selectCandidateSubscriptionsSQL uses, and for the same reason.
const selectCandidateEventRoutesSQL = `
SELECT ` + eventRouteColumns + `
FROM event_routes
WHERE namespace_id = $1 AND event_name = $2 AND status = 'active'
  AND ($3::text IS NULL OR run_id = $3)
ORDER BY run_id, id
`

// selectLockedEventRoutesSQL is the authoritative match, restricted to the
// runs whose advisory locks this transaction already holds. A route whose run
// joined the candidate set after the locks were taken is deliberately not
// chased: that race has no defined order, and treating it as
// event-before-route (no pickup) is the same answer the subscription path
// gives to its own version of the race.
const selectLockedEventRoutesSQL = `
SELECT ` + eventRouteColumns + `
FROM event_routes
WHERE namespace_id = $1 AND event_name = $2 AND status = 'active'
  AND ($3::text IS NULL OR run_id = $3)
  AND run_id = ANY($4)
ORDER BY run_id, id
FOR UPDATE
`

// candidateEventRouteRuns reads which runs have an active route for this
// event — the run ids whose advisory locks the delivery must take. It runs
// BEFORE any lock is held, so its answer is only a lock plan.
func candidateEventRouteRuns(ctx context.Context, tx pgx.Tx, in DeliverSignalEventInput) ([]string, error) {
	rows, err := tx.Query(ctx, selectCandidateEventRoutesSQL, in.NamespaceID, in.Name, textOrNull(in.RunID))
	if err != nil {
		return nil, fmt.Errorf("postgres: DeliverSignalEvent: match event routes: %w", err)
	}
	routes, err := scanEventRoutes(rows)
	if err != nil {
		return nil, fmt.Errorf("postgres: DeliverSignalEvent: scan event route: %w", err)
	}
	seen := make(map[string]struct{}, len(routes))
	var ids []string
	for _, route := range routes {
		if _, ok := seen[route.RunID]; ok {
			continue
		}
		seen[route.RunID] = struct{}{}
		ids = append(ids, route.RunID)
	}
	return ids, nil
}

// lockedEventRoutes re-reads the routes authoritatively under the locks.
func lockedEventRoutes(ctx context.Context, tx pgx.Tx, in DeliverSignalEventInput, lockedRuns []string) ([]engine.EventRoute, error) {
	if len(lockedRuns) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, selectLockedEventRoutesSQL, in.NamespaceID, in.Name, textOrNull(in.RunID), lockedRuns)
	if err != nil {
		return nil, fmt.Errorf("postgres: DeliverSignalEvent: match event routes: %w", err)
	}
	routes, err := scanEventRoutes(rows)
	if err != nil {
		return nil, fmt.Errorf("postgres: DeliverSignalEvent: scan event route: %w", err)
	}
	return routes, nil
}

// runEventRoutePickup asks the engine to pick the fact up for each matched
// route, inside this delivery's transaction.
//
// The engine transaction view built here deliberately carries a nil ledger
// runtime: pickup creates control-flow state (a token, a node run, its work
// or its human task) and appends audit events, and appends no ledger record —
// evidence is produced by attempts, not by an event arriving. A pickup path
// that ever needs the ledger must be given a real runtime here rather than
// discovering nil at run time, which is why this is stated and not implied.
func runEventRoutePickup(
	ctx context.Context, tx pgx.Tx, namespaceID string,
	pickup engine.EventPickupRunner, routes []engine.EventRoute, ev SignalEvent,
) ([]engine.EventPickupResult, error) {
	if pickup == nil || len(routes) == 0 {
		return nil, nil
	}
	inner := &engineTx{engineQueries: engineQueries{q: tx, namespaceID: namespaceID}}
	fact := engine.PickupEvent{ID: ev.ID, Name: ev.Name, Emitter: ev.Emitter, Payload: ev.Payload}

	results := make([]engine.EventPickupResult, 0, len(routes))
	for _, route := range routes {
		result, err := pickup.PickUpEvent(ctx, inner, route, fact)
		if err != nil {
			return nil, fmt.Errorf("postgres: DeliverSignalEvent: pick up route %s: %w", route.ID, err)
		}
		results = append(results, result)
	}
	return results, nil
}
