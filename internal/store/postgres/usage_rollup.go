package postgres

import (
	"context"
	"fmt"
)

// UsageRollup aggregates the §13.2 usage/cost telemetry (migrations/0012's
// usage_* columns on `attempts`) recorded on a set of attempts -- either
// every attempt of one node run (NodeRunUsage/NodeRunUsages) or every
// attempt of every node run belonging to one run (RunUsage). Task t2's
// acceptance criteria this type exists to satisfy:
//
//   - the rollup includes failed/retried/cancelled attempts, not only the
//     one that eventually succeeded -- a node run retried after burning
//     tokens on a failed attempt spent them regardless of the outcome, so
//     InputTokens/OutputTokens sum over every attempt in scope regardless
//     of its TechStatus ("retry burn").
//   - an attempt that reported no usage at all (usage_input_tokens IS
//     NULL) is excluded from every sum -- never folded in as a zero -- and
//     is instead counted in AttemptsNotReported, distinct from
//     AttemptsReported.
//   - cost is summed only over attempts that reported one (usage_cost IS
//     NOT NULL), and never summed across differing currencies: Cost holds
//     one entry per distinct currency actually seen among cost-reporting
//     attempts, so a caller can tell "everyone agreed on one currency"
//     (len(Cost) == 1) from "these don't share a currency, expose them
//     separately" (len(Cost) > 1) from "nobody priced their work at all"
//     (len(Cost) == 0) without ever computing a number that adds USD to
//     JPY.
type UsageRollup struct {
	// InputTokens/OutputTokens sum usage_input_tokens/usage_output_tokens
	// over attempts where usage_input_tokens IS NOT NULL -- migrations/0012's
	// documented sentinel for "this attempt reported usage at all" (the
	// engine's InsertAttempt always writes both token columns together
	// whenever an attempt carried a Usage block, so either column is an
	// equally valid presence check; see engine_store.go's InsertAttempt).
	InputTokens  int64
	OutputTokens int64

	// AttemptsReported counts the attempts InputTokens/OutputTokens summed.
	// AttemptsNotReported counts every other attempt in scope: one that
	// completed (successfully or not) with no Usage block reported at all.
	// The two are disjoint and together account for every attempt in
	// scope, so AttemptsReported == 0 && AttemptsNotReported > 0 is exactly
	// "no attempt reported usage" -- distinguishable from a reported sum
	// that happens to equal zero (AttemptsReported > 0, InputTokens == 0),
	// which never happens with AttemptsNotReported alone.
	//
	// AttemptsNotReported is a permanent category, not a transitional one
	// (issue #32's honest narrowing, stated in migrations/README.md's 0012
	// entry): an attempt reports usage only when its bridge held a parseable
	// terminal result at completion -- on the §13.2 sync result, the
	// completed/failed callback payloads (ADR 0008), or the sync 500 error
	// body. Cancelled attempts (a SIGTERM'd session emits no terminal
	// event) and result-less crashes or timeouts have nothing to report and
	// stay NULL forever, so their real burn is visible only as this count.
	// Consumers must render it as "unreported", never as zero spend.
	AttemptsReported    int
	AttemptsNotReported int

	// Cost sums usage_cost per distinct usage_currency value among
	// attempts that reported a cost (usage_cost IS NOT NULL), one
	// CurrencyCost entry per currency actually seen. An attempt that
	// reported a cost with no currency (independently nullable from cost
	// per §13.2 -- see migrations/0012's doc comment) contributes to the
	// entry whose Currency is "" rather than being dropped or guessed into
	// a named currency, so it still shows up as a distinct bucket instead
	// of silently vanishing from the total. Never sorted by magnitude --
	// by currency string, so the result is deterministic for tests and
	// callers alike.
	Cost []CurrencyCost
}

// CurrencyCost is one UsageRollup.Cost entry: the summed usage_cost of
// every attempt in scope that reported exactly this Currency (which may be
// "", meaning "reported a cost but no currency").
type CurrencyCost struct {
	Currency string
	Cost     float64
}

// nodeRunUsageWhereSQL and runUsageWhereSQL are the two scopes usageRollup
// supports: every attempt of one node run, or every attempt of every node
// run belonging to one run (the retry-burn total across a whole run,
// including node runs that were retried, cancelled, or never succeeded).
const (
	nodeRunUsageWhereSQL = `node_run_id = $1`
	runUsageWhereSQL     = `node_run_id IN (SELECT id FROM node_runs WHERE run_id = $1)`
)

// NodeRunUsage aggregates every attempt of one node run.
func (eq engineQueries) NodeRunUsage(ctx context.Context, nodeRunID string) (UsageRollup, error) {
	rollup, err := eq.usageRollup(ctx, nodeRunUsageWhereSQL, nodeRunID)
	if err != nil {
		return UsageRollup{}, fmt.Errorf("postgres: engine: NodeRunUsage: %w", err)
	}
	return rollup, nil
}

// RunUsage aggregates every attempt of every node run belonging to one run.
func (eq engineQueries) RunUsage(ctx context.Context, runID string) (UsageRollup, error) {
	rollup, err := eq.usageRollup(ctx, runUsageWhereSQL, runID)
	if err != nil {
		return UsageRollup{}, fmt.Errorf("postgres: engine: RunUsage: %w", err)
	}
	return rollup, nil
}

// usageRollup runs the shared two-query aggregation (totals/counts, then
// cost-by-currency) over attempts matching whereSQL, a literal fragment
// naming exactly one placeholder ($1), parameterized by scopeID.
func (eq engineQueries) usageRollup(ctx context.Context, whereSQL, scopeID string) (UsageRollup, error) {
	var rollup UsageRollup
	err := eq.q.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(usage_input_tokens), 0),
			COALESCE(SUM(usage_output_tokens), 0),
			COUNT(*) FILTER (WHERE usage_input_tokens IS NOT NULL)::int,
			COUNT(*) FILTER (WHERE usage_input_tokens IS NULL)::int
		FROM attempts
		WHERE `+whereSQL,
		scopeID,
	).Scan(&rollup.InputTokens, &rollup.OutputTokens, &rollup.AttemptsReported, &rollup.AttemptsNotReported)
	if err != nil {
		return UsageRollup{}, fmt.Errorf("totals: %w", err)
	}

	rows, err := eq.q.Query(ctx, `
		SELECT COALESCE(usage_currency, ''), SUM(usage_cost)
		FROM attempts
		WHERE `+whereSQL+`
		  AND usage_cost IS NOT NULL
		GROUP BY COALESCE(usage_currency, '')
		ORDER BY 1`,
		scopeID,
	)
	if err != nil {
		return UsageRollup{}, fmt.Errorf("cost by currency: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cc CurrencyCost
		if err := rows.Scan(&cc.Currency, &cc.Cost); err != nil {
			return UsageRollup{}, fmt.Errorf("cost by currency: scan: %w", err)
		}
		rollup.Cost = append(rollup.Cost, cc)
	}
	if err := rows.Err(); err != nil {
		return UsageRollup{}, fmt.Errorf("cost by currency: %w", err)
	}
	return rollup, nil
}

// NodeRunUsages batches NodeRunUsage over many node runs in two queries
// (totals/counts grouped by node_run_id, then cost-by-currency grouped by
// (node_run_id, currency)) instead of one round trip per id -- the same
// batching shape internal/api/queries.go's latestAttemptActorIDs uses for
// the cross-run node-runs listing this method exists to serve. A node run
// with no attempts at all (never dispatched) is simply absent from the
// returned map; callers read that as the zero UsageRollup (0 reported, 0
// not-reported, no cost) rather than a lookup failure.
func (eq engineQueries) NodeRunUsages(ctx context.Context, nodeRunIDs []string) (map[string]UsageRollup, error) {
	out := make(map[string]UsageRollup, len(nodeRunIDs))
	if len(nodeRunIDs) == 0 {
		return out, nil
	}

	rows, err := eq.q.Query(ctx, `
		SELECT node_run_id,
			COALESCE(SUM(usage_input_tokens), 0),
			COALESCE(SUM(usage_output_tokens), 0),
			COUNT(*) FILTER (WHERE usage_input_tokens IS NOT NULL)::int,
			COUNT(*) FILTER (WHERE usage_input_tokens IS NULL)::int
		FROM attempts
		WHERE node_run_id = ANY($1)
		GROUP BY node_run_id`,
		nodeRunIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: NodeRunUsages: totals: %w", err)
	}
	for rows.Next() {
		var (
			nodeRunID string
			rollup    UsageRollup
		)
		if err := rows.Scan(&nodeRunID, &rollup.InputTokens, &rollup.OutputTokens, &rollup.AttemptsReported, &rollup.AttemptsNotReported); err != nil {
			rows.Close()
			return nil, fmt.Errorf("postgres: engine: NodeRunUsages: totals: scan: %w", err)
		}
		out[nodeRunID] = rollup
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: engine: NodeRunUsages: totals: %w", err)
	}

	costRows, err := eq.q.Query(ctx, `
		SELECT node_run_id, COALESCE(usage_currency, ''), SUM(usage_cost)
		FROM attempts
		WHERE node_run_id = ANY($1)
		  AND usage_cost IS NOT NULL
		GROUP BY node_run_id, COALESCE(usage_currency, '')
		ORDER BY 1, 2`,
		nodeRunIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: NodeRunUsages: cost by currency: %w", err)
	}
	defer costRows.Close()
	for costRows.Next() {
		var (
			nodeRunID string
			cc        CurrencyCost
		)
		if err := costRows.Scan(&nodeRunID, &cc.Currency, &cc.Cost); err != nil {
			return nil, fmt.Errorf("postgres: engine: NodeRunUsages: cost by currency: scan: %w", err)
		}
		rollup := out[nodeRunID]
		rollup.Cost = append(rollup.Cost, cc)
		out[nodeRunID] = rollup
	}
	if err := costRows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: engine: NodeRunUsages: cost by currency: %w", err)
	}
	return out, nil
}
