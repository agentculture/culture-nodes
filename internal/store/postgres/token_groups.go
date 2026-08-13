package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// Split/join persistence (issue #43, migrations/0019_parallel_tokens.sql):
// token groups, join arrivals, the open-barrier lookup, and the two reap
// operations. Everything here that mutates state is an engineQueries method,
// so it runs inside the engine's §12.5 transaction under the run's advisory
// lock — the lock is what makes JoinArrivalCount's plain SELECT count(*) a
// race-free barrier (parallel-tokens design §4.2).

// Token reads one token row.
func (eq engineQueries) Token(ctx context.Context, tokenID string) (engine.Token, error) {
	var (
		token         engine.Token
		state         string
		parentTokenID pgtype.Text
		groupID       pgtype.Text
		originEventID pgtype.Text
		createdAt     pgtype.Timestamptz
		consumedAt    pgtype.Timestamptz
	)
	err := eq.q.QueryRow(ctx, `
		SELECT id, namespace_id, run_id, node_key, state, parent_token_id, group_id, origin_event_id, created_at, consumed_at
		FROM tokens WHERE id = $1
	`, tokenID).Scan(
		&token.ID, &token.NamespaceID, &token.RunID, &token.NodeID, &state,
		&parentTokenID, &groupID, &originEventID, &createdAt, &consumedAt,
	)
	if err != nil {
		if isNoRows(err) {
			return engine.Token{}, fmt.Errorf("postgres: engine: token %s: %w", tokenID, engine.ErrNotFound)
		}
		return engine.Token{}, fmt.Errorf("postgres: engine: Token: %w", err)
	}
	token.State = engine.TokenState(state)
	token.ParentTokenID = textOrEmpty(parentTokenID)
	token.GroupID = textOrEmpty(groupID)
	token.OriginEventID = textOrEmpty(originEventID)
	token.CreatedAt = tsValue(createdAt)
	token.ConsumedAt = tsValue(consumedAt)
	return token, nil
}

// ActiveTokenCount is how many tokens the run currently has active. The
// query is served by tokens_run_state_idx (migrations/0002), so the split
// bound check inside the transaction stays an index probe.
func (eq engineQueries) ActiveTokenCount(ctx context.Context, runID string) (int, error) {
	var count int32
	err := eq.q.QueryRow(ctx,
		`SELECT COUNT(*)::int FROM tokens WHERE run_id = $1 AND state = 'active'`, runID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("postgres: engine: ActiveTokenCount: %w", err)
	}
	return int(count), nil
}

// InsertTokenGroup records one split's fan-out set (design D4: the
// cardinality is discovered at split time and fixed at creation).
func (eq engineQueries) InsertTokenGroup(ctx context.Context, group engine.TokenGroup) error {
	_, err := eq.q.Exec(ctx, `
		INSERT INTO token_groups (id, namespace_id, run_id, split_node_run_id, parent_group_id, cardinality, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, group.ID, eq.namespaceID, group.RunID, group.SplitNodeRunID,
		textOrNull(group.ParentGroupID), int32(group.Cardinality), tsOrNow(group.CreatedAt))
	if err != nil {
		return fmt.Errorf("postgres: engine: InsertTokenGroup: %w", err)
	}
	return nil
}

// TokenGroup reads one token group.
func (eq engineQueries) TokenGroup(ctx context.Context, groupID string) (engine.TokenGroup, error) {
	var (
		group         engine.TokenGroup
		parentGroupID pgtype.Text
		cardinality   int32
		createdAt     pgtype.Timestamptz
	)
	err := eq.q.QueryRow(ctx, `
		SELECT id, namespace_id, run_id, split_node_run_id, parent_group_id, cardinality, created_at
		FROM token_groups WHERE id = $1
	`, groupID).Scan(
		&group.ID, &group.NamespaceID, &group.RunID, &group.SplitNodeRunID,
		&parentGroupID, &cardinality, &createdAt,
	)
	if err != nil {
		if isNoRows(err) {
			return engine.TokenGroup{}, fmt.Errorf("postgres: engine: token group %s: %w", groupID, engine.ErrNotFound)
		}
		return engine.TokenGroup{}, fmt.Errorf("postgres: engine: TokenGroup: %w", err)
	}
	group.ParentGroupID = textOrEmpty(parentGroupID)
	group.Cardinality = int(cardinality)
	group.CreatedAt = tsValue(createdAt)
	return group, nil
}

// InsertJoinArrival appends one branch's arrival at a barrier. The
// join_arrivals (join_node_run_id, token_id) unique constraint is the
// idempotency backstop: a branch token can arrive at one barrier exactly
// once, so a replayed arrival is a constraint violation rather than a
// silently inflated count. (The fenced completion guard already refuses the
// duplicate completion before this insert could run; the constraint makes
// the invariant hold even against a future caller that bypasses it.)
func (eq engineQueries) InsertJoinArrival(ctx context.Context, arrival engine.JoinArrival) error {
	var output any
	if len(arrival.Output) > 0 {
		output = []byte(arrival.Output)
	}
	_, err := eq.q.Exec(ctx, `
		INSERT INTO join_arrivals (id, namespace_id, run_id, join_node_run_id, group_id, token_id, from_node, outcome, output, arrived_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, arrival.ID, eq.namespaceID, arrival.RunID, arrival.JoinNodeRunID, arrival.GroupID,
		arrival.TokenID, arrival.FromNode, arrival.Outcome, output, tsOrNow(arrival.ArrivedAt))
	if err != nil {
		return fmt.Errorf("postgres: engine: InsertJoinArrival: %w", err)
	}
	return nil
}

// JoinArrivalCount counts a barrier's recorded arrivals. Under the run's
// advisory lock this is exact — no counter column, nothing to drift.
func (eq engineQueries) JoinArrivalCount(ctx context.Context, joinNodeRunID string) (int, error) {
	var count int32
	err := eq.q.QueryRow(ctx,
		`SELECT COUNT(*)::int FROM join_arrivals WHERE join_node_run_id = $1`, joinNodeRunID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("postgres: engine: JoinArrivalCount: %w", err)
	}
	return int(count), nil
}

// OpenJoinBarrier locates the parked barrier for (run, node, group): the
// waiting_join node run whose own token carries the arriving token's group.
// The lookup joins node_runs -> tokens on token_id; node_runs_waiting_join_idx
// (migrations/0019, partial on status = 'waiting_join') serves the outer
// filter, so the probe stays exact however many node runs the run has.
func (eq engineQueries) OpenJoinBarrier(ctx context.Context, runID, nodeKey, groupID string) (engine.NodeRun, error) {
	var id string
	err := eq.q.QueryRow(ctx, `
		SELECT nr.id
		FROM node_runs AS nr
		JOIN tokens AS t ON t.id = nr.token_id
		WHERE nr.run_id = $1 AND nr.node_key = $2 AND nr.status = 'waiting_join' AND t.group_id = $3
	`, runID, nodeKey, groupID).Scan(&id)
	if err != nil {
		if isNoRows(err) {
			return engine.NodeRun{}, fmt.Errorf("postgres: engine: no open join barrier for run %s node %s group %s: %w",
				runID, nodeKey, groupID, engine.ErrNotFound)
		}
		return engine.NodeRun{}, fmt.Errorf("postgres: engine: OpenJoinBarrier: %w", err)
	}
	return eq.NodeRun(ctx, id)
}

// groupSubtreeCTE resolves a group and every nested descendant group by
// recursive parentage — the group-scoped reap must reach an inner split's
// branches, and an inner join's parked barrier, when an outer barrier fires
// early (design §4.4; review point S2).
const groupSubtreeCTE = `
WITH RECURSIVE grp AS (
	SELECT id FROM token_groups WHERE id = $2
	UNION ALL
	SELECT tg.id FROM token_groups AS tg JOIN grp ON tg.parent_group_id = grp.id
)`

// ReapGroupBranches retires a token group's still-live branches, keeping
// keepTokenID (the firing barrier's own token): cancels their non-terminal
// node runs — nested waiting_join barriers included — cancels their leasable
// work items, retires their pending timers and signal subscriptions, and
// consumes their active tokens. It returns the cancelled node runs' ids so
// the completion can emit branch.cancelled per branch and the caller can
// best-effort propagate cancellation to async actors after commit.
func (eq engineQueries) ReapGroupBranches(ctx context.Context, runID, groupID, keepTokenID string) ([]string, error) {
	// Node runs first (RETURNING drives the rest): any node run whose token
	// belongs to the group subtree, except the barrier's own.
	rows, err := eq.q.Query(ctx, groupSubtreeCTE+`
		UPDATE node_runs SET status = 'cancelled', updated_at = now(), completed_at = now()
		WHERE run_id = $1
		  AND status NOT IN ('completed', 'failed', 'cancelled')
		  AND token_id IN (
			SELECT id FROM tokens WHERE group_id IN (SELECT id FROM grp) AND id <> $3
		  )
		RETURNING id
	`, runID, groupID, keepTokenID)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: ReapGroupBranches: cancel node runs: %w", err)
	}
	reaped, err := scanIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: ReapGroupBranches: %w", err)
	}
	if err := eq.reapNodeRunAppendages(ctx, reaped); err != nil {
		return nil, fmt.Errorf("postgres: engine: ReapGroupBranches: %w", err)
	}
	// Consume the subtree's remaining active tokens — the losing branches'
	// current positions AND any nested barrier's own first-arrival token
	// (review point R1: left active, it would sit there forever).
	if _, err := eq.q.Exec(ctx, groupSubtreeCTE+`
		UPDATE tokens SET state = 'consumed', consumed_at = now()
		WHERE run_id = $1 AND state = 'active'
		  AND group_id IN (SELECT id FROM grp) AND id <> $3
	`, runID, groupID, keepTokenID); err != nil {
		return nil, fmt.Errorf("postgres: engine: ReapGroupBranches: consume tokens: %w", err)
	}
	return reaped, nil
}

// ReapRunState retires everything still live in a run except keepNodeRunID —
// the engine-transaction mirror of the API cancel REAP (design D6): active
// tokens, non-terminal node runs (waiting_join barriers included), leasable
// work items, pending timers, pending signal subscriptions. It returns the
// cancelled node runs' ids.
func (eq engineQueries) ReapRunState(ctx context.Context, runID, keepNodeRunID string) ([]string, error) {
	rows, err := eq.q.Query(ctx, `
		UPDATE node_runs SET status = 'cancelled', updated_at = now(), completed_at = now()
		WHERE run_id = $1 AND id <> $2
		  AND status NOT IN ('completed', 'failed', 'cancelled')
		RETURNING id
	`, runID, keepNodeRunID)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: ReapRunState: cancel node runs: %w", err)
	}
	reaped, err := scanIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: ReapRunState: %w", err)
	}
	if err := eq.reapNodeRunAppendages(ctx, reaped); err != nil {
		return nil, fmt.Errorf("postgres: engine: ReapRunState: %w", err)
	}
	if _, err := eq.q.Exec(ctx,
		`UPDATE tokens SET state = 'consumed', consumed_at = now() WHERE run_id = $1 AND state = 'active'`, runID,
	); err != nil {
		return nil, fmt.Errorf("postgres: engine: ReapRunState: consume tokens: %w", err)
	}
	// A dead run stops observing: its standing event routes are retired
	// alongside the timers and subscriptions above (issue #43, design §6.1).
	// A route left active would let a later delivery create a token — and
	// therefore claimable work — inside a run that is already terminal, which
	// is exactly the re-dispatch zombie issue #19 closed for cancellation.
	if _, err := eq.RetireEventRoutes(ctx, runID); err != nil {
		return nil, fmt.Errorf("postgres: engine: ReapRunState: %w", err)
	}
	return reaped, nil
}

// reapNodeRunAppendages cancels the leasable work items and retires the
// pending timers and signal subscriptions of the given node runs. Work items
// in 'ready', 'waiting', AND 'leased' are all cancelled — the same
// three-state reap run cancellation performs, for the same issue-#19 reason:
// a 'waiting' row left alone is exactly what a fired deadline timer would
// flip back to 'ready' for a branch that is supposed to be dead. The
// 'canceled' (one l) spellings are the timers/signal_subscriptions tables'
// own status vocabulary.
func (eq engineQueries) reapNodeRunAppendages(ctx context.Context, nodeRunIDs []string) error {
	if len(nodeRunIDs) == 0 {
		return nil
	}
	if _, err := eq.q.Exec(ctx, `
		UPDATE work_items SET state = 'cancelled', updated_at = now()
		WHERE state IN ('ready', 'waiting', 'leased') AND node_run_id = ANY($1)
	`, nodeRunIDs); err != nil {
		return fmt.Errorf("cancel work items: %w", err)
	}
	if _, err := eq.q.Exec(ctx, `
		UPDATE timers SET status = 'canceled'
		WHERE status = 'pending' AND node_run_id = ANY($1)
	`, nodeRunIDs); err != nil {
		return fmt.Errorf("cancel timers: %w", err)
	}
	if _, err := eq.q.Exec(ctx, `
		UPDATE signal_subscriptions SET status = 'canceled'
		WHERE status = 'pending' AND node_run_id = ANY($1)
	`, nodeRunIDs); err != nil {
		return fmt.Errorf("cancel signal subscriptions: %w", err)
	}
	return nil
}

func scanIDs(rows interface {
	Next() bool
	Scan(...any) error
	Close()
	Err() error
}) ([]string, error) {
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// JoinAggregateRow is one arrival as the worker's join dispatch reads it
// back to build the join's ordered aggregated output (design D5).
type JoinAggregateRow struct {
	FromNode  string
	TokenID   string
	Outcome   string
	Output    json.RawMessage
	ArrivedAt time.Time
}

// JoinAggregate is what a worker needs to complete a satisfied join node
// run: the arrivals in arrival order and the group's discovered cardinality.
type JoinAggregate struct {
	Cardinality int
	Arrivals    []JoinAggregateRow
}

// JoinAggregate reads a join node run's arrivals and group cardinality —
// the worker-side read behind the join dispatch seam (design D2). Arrival
// order is (arrived_at, id): rows written in one transaction share a
// timestamp, and the ULID id breaks the tie in insertion order.
func (s *Store) JoinAggregate(ctx context.Context, joinNodeRunID string) (JoinAggregate, error) {
	var agg JoinAggregate
	err := s.pool.QueryRow(ctx, `
		SELECT tg.cardinality
		FROM join_arrivals AS ja
		JOIN token_groups AS tg ON tg.id = ja.group_id
		WHERE ja.join_node_run_id = $1
		LIMIT 1
	`, joinNodeRunID).Scan(&agg.Cardinality)
	if err != nil {
		if isNoRows(err) {
			return JoinAggregate{}, fmt.Errorf("postgres: join node run %s has no recorded arrivals: %w", joinNodeRunID, ErrNotFound)
		}
		return JoinAggregate{}, fmt.Errorf("postgres: JoinAggregate: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT from_node, token_id, outcome, output, arrived_at
		FROM join_arrivals
		WHERE join_node_run_id = $1
		ORDER BY arrived_at, id
	`, joinNodeRunID)
	if err != nil {
		return JoinAggregate{}, fmt.Errorf("postgres: JoinAggregate: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			row       JoinAggregateRow
			output    []byte
			arrivedAt pgtype.Timestamptz
		)
		if err := rows.Scan(&row.FromNode, &row.TokenID, &row.Outcome, &output, &arrivedAt); err != nil {
			return JoinAggregate{}, fmt.Errorf("postgres: JoinAggregate: scan: %w", err)
		}
		row.Output = json.RawMessage(output)
		row.ArrivedAt = tsValue(arrivedAt)
		agg.Arrivals = append(agg.Arrivals, row)
	}
	if err := rows.Err(); err != nil {
		return JoinAggregate{}, fmt.Errorf("postgres: JoinAggregate: %w", err)
	}
	return agg, nil
}
