package postgres

import (
	"context"
	"fmt"
)

// The two durable measures a run's declared economic budget is spent against
// (migration 0023, task t11 of the economy-discord-graphs plan; issue #48
// item 5, spec claim c6, honesty h5).
//
// Both are *Store methods rather than engineQueries ones, for continuation.go's
// reason: the caller is a worker holding a claim and about to dispatch
// (internal/worker/budget.go), outside any completion transaction. Both read
// committed rows only, which is also what makes the budget hold across
// horizontally scaled workers rather than per process.

// SessionStart is one new provider session a run opened: the row
// RecordSessionStart writes.
type SessionStart struct {
	// AttemptID is the §13.1 protocol attempt id of the dispatch that opened
	// the session -- the idempotency key (migration 0023).
	AttemptID   string
	NamespaceID string
	RunID       string
	NodeRunID   string
	NodeKey     string
	// ActorRef is the reference the node named; ActorID is the resolved
	// actors-table row id when the registry could answer one, "" otherwise
	// (stored NULL).
	ActorRef string
	ActorID  string
}

const recordSessionStartSQL = `
INSERT INTO run_sessions (attempt_id, namespace_id, run_id, node_run_id, node_key, actor_ref, actor_id)
VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''))
ON CONFLICT (attempt_id) DO NOTHING
`

// RecordSessionStart charges one cold start to a run.
//
// Idempotent on the protocol attempt id: a worker that recorded a start and
// then re-entered the same dispatch charges the session once. Two distinct
// dispatches are two distinct sessions and charge twice, which is the honest
// reading -- each opened its own conversation.
//
// It is called immediately BEFORE the invocation, never after. See migration
// 0023 for why that deliberately over-counts a dispatch that dies in
// transport: a budget that under-counts spends money the author forbade.
func (s *Store) RecordSessionStart(ctx context.Context, start SessionStart) error {
	if start.AttemptID == "" || start.RunID == "" {
		return fmt.Errorf("postgres: RecordSessionStart: attempt id and run id are required")
	}
	_, err := s.pool.Exec(ctx, recordSessionStartSQL,
		start.AttemptID, start.NamespaceID, start.RunID, start.NodeRunID,
		start.NodeKey, start.ActorRef, start.ActorID)
	if err != nil {
		return fmt.Errorf("postgres: RecordSessionStart: %w", err)
	}
	return nil
}

const runSessionStartsSQL = `SELECT count(*)::int FROM run_sessions WHERE run_id = $1`

// RunSessionStarts is how many NEW provider sessions this run has started --
// the number `budget.maxSessions` bounds. Warm turns are not in this table
// and are therefore not in this count, which is the entire point (spec claim
// c46).
func (s *Store) RunSessionStarts(ctx context.Context, runID string) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx, runSessionStartsSQL, runID).Scan(&count); err != nil {
		return 0, fmt.Errorf("postgres: RunSessionStarts: %w", err)
	}
	return count, nil
}

// UncachedInput is a run's measured uncached input spend, with the coverage
// counts that say how much of the run the measure could actually see.
//
// The counts are not decoration. A budget can only ever bound MEASURED
// spend: an attempt that reported no usage at all (a cancelled session, a
// crash with no terminal result -- see UsageRollup.AttemptsNotReported for
// why that category is permanent rather than transitional) burned real
// tokens that no ceiling here can charge for. Reporting the total without
// saying how many attempts it could not see would make the number read as
// complete when it is a floor.
type UncachedInput struct {
	// Tokens is SUM(usage_input_tokens - COALESCE(usage_cached_input_tokens, 0))
	// over the attempts of this run that reported usage.
	//
	// The COALESCE is the honesty decision, and it is deliberately the
	// EXPENSIVE reading: an attempt that reported input tokens but no cached
	// figure charges its input IN FULL. ADR 0009 keeps the column NULL for a
	// backend that reports no cache telemetry precisely so nobody
	// zero-fills it into a fabricated measurement; the same rule applied to
	// a DECISION means the budget must not hand out a discount it cannot
	// see. "No cache telemetry" is not "0% cached" as a fact, and it is
	// charged as if it were 0% cached as a policy -- the two are different
	// statements and only the second one is being made here.
	Tokens int64
	// AttemptsWithoutCacheTelemetry counts the reported attempts whose input
	// was charged in full for want of a cached figure. It makes the
	// conservative charge auditable: an operator can see how much of the
	// spend is measured and how much is assumed.
	AttemptsWithoutCacheTelemetry int
	// AttemptsReported / AttemptsNotReported partition every attempt of the
	// run exactly as UsageRollup's fields of the same name do -- including
	// the superseded-row exclusion (ADR 0012 §3), without which they would
	// not partition anything: one dispatch would count once as reported and
	// once as not.
	AttemptsReported    int
	AttemptsNotReported int
}

// The superseded-row exclusion is attemptCurrentUnaliasedSQL, the same
// fragment usage_rollup.go applies, for the same reason and with the same
// force: this is an aggregate over `attempts`, so ADR 0012 §3's reader rule
// applies to it. It matters MORE here than in a rollup, in fact -- a rollup
// that double-counts misreports, while a budget that double-counts SPENDS
// the ceiling the author declared, tripping `budget.maxUncachedInput` early
// against tokens no session ever sent. Nobody files that as a bug; the run
// just looks expensive.
const runUncachedInputSQL = `
SELECT
    COALESCE(SUM(usage_input_tokens - COALESCE(usage_cached_input_tokens, 0)), 0),
    COUNT(*) FILTER (WHERE usage_input_tokens IS NOT NULL AND usage_cached_input_tokens IS NULL)::int,
    COUNT(*) FILTER (WHERE usage_input_tokens IS NOT NULL)::int,
    COUNT(*) FILTER (WHERE usage_input_tokens IS NULL)::int
FROM attempts
WHERE node_run_id IN (SELECT id FROM node_runs WHERE run_id = $1)` + attemptCurrentUnaliasedSQL

// RunUncachedInput measures what `budget.maxUncachedInput` bounds: the input
// tokens this run has sent that the provider did not demonstrably serve from
// cache, summed over every attempt of every node run -- including failed,
// retried and cancelled ones, for UsageRollup's retry-burn reason (a failed
// attempt spent its tokens regardless of how it ended), and excluding
// superseded ones, for ADR 0012 §3's reason (a corrected record and its
// correction describe one dispatch, not two).
func (s *Store) RunUncachedInput(ctx context.Context, runID string) (UncachedInput, error) {
	var spend UncachedInput
	err := s.pool.QueryRow(ctx, runUncachedInputSQL, runID).Scan(
		&spend.Tokens,
		&spend.AttemptsWithoutCacheTelemetry,
		&spend.AttemptsReported,
		&spend.AttemptsNotReported,
	)
	if err != nil {
		return UncachedInput{}, fmt.Errorf("postgres: RunUncachedInput: %w", err)
	}
	return spend, nil
}
