package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The continuation lookup: which conversation, if any, a new dispatch to an
// actor should continue (task t4, spec claim c3,
// docs/adr/0010-continuation-ref-on-request.md).
//
// It is a *Store method rather than an engineQueries one because its caller
// is a worker about to dispatch (internal/worker/dispatch.go's
// dispatchActor), which holds a claim and is outside any completion
// transaction. It reads committed attempt rows only: a handle becomes
// re-dispatchable exactly when the attempt that reported it committed, which
// is also what makes the lookup work across worker processes rather than
// only within the one that saw the result.

const latestContinuationRefSQL = `
SELECT a.continuation_ref
FROM attempts AS a
JOIN node_runs AS nr ON nr.id = a.node_run_id
WHERE nr.run_id = $1
  AND a.actor_id = $2
  AND a.continuation_ref IS NOT NULL
ORDER BY a.completed_at DESC, a.id DESC
LIMIT 1
`

// LatestContinuationRef returns the most recent continuation handle reported
// within one run by one actor, and whether there was one at all.
//
// SCOPE, STATED HONESTLY. Same run, same actor row — and deliberately
// nothing wider (ADR 0010 §4):
//
//   - Not the workstream session key spec claim c3 ultimately wants (actor +
//     repo + workstream, which outlives a single run). That needs a declared
//     transport key all three bridges exclude from their Bound-inputs block
//     (task t5) and a per-key serialization so two dispatches cannot
//     interleave turns on one provider thread (task t6). Until both exist, a
//     cross-run lookup would resume a conversation nothing declared it
//     wanted resumed.
//   - Not across actors. A handle is meaningful only to the actor that
//     issued it, and `actor_id = $2` never matches SQL NULL, so an
//     unattributed dispatch (empty actorID) resolves nothing rather than
//     inheriting some other dispatch's session.
//
// Attempts that reported no handle are skipped rather than shadowing an
// earlier one that did: `continuation_ref IS NOT NULL` is in the WHERE
// clause, not applied to the newest row. A turn that failed technically
// after a session existed does not erase the session.
//
// Absence is not an error. No prior handle means a cold dispatch, which
// costs more and is never wrong.
func (s *Store) LatestContinuationRef(ctx context.Context, runID, actorID string) (string, bool, error) {
	if runID == "" || actorID == "" {
		return "", false, nil
	}
	var ref string
	err := s.pool.QueryRow(ctx, latestContinuationRefSQL, runID, actorID).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("postgres: LatestContinuationRef: %w", err)
	}
	return ref, true, nil
}
