package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/store"
)

// Work claiming (docs/initial-design/culture-nodes-prd-spec.md §12.4).
//
// A work_items row is a claimable unit of ready work: work ID, namespace,
// node-run ID, state, state version, lease owner, lease expiry, fencing
// token, attempt, and available-at time -- exactly the fields §12.4 lists
// for "a work record". This file implements the four operations that move
// a row through its lifecycle, plus the invariants callers may rely on:
//
//  1. Claiming is atomic. ClaimWork is a single `UPDATE work_items ...
//     FROM (SELECT ... FOR UPDATE SKIP LOCKED LIMIT $n) ...` statement: two
//     concurrent callers racing over the same ready row never both win it --
//     the loser's SKIP LOCKED subquery simply does not see a row the winner
//     already has locked, so it returns fewer (possibly zero) rows rather
//     than blocking or erroring. There is no read-then-write window for a
//     second caller to land in.
//
//  2. A claim always assigns a fencing token strictly greater than any
//     token this work item has ever held, and bumps attempt alongside it --
//     each successful claim is a new attempt at the same logical work. This
//     is enforced by `fencing_token = fencing_token + 1` inside the same
//     UPDATE that flips state to 'leased', so the increment and the win are
//     the same atomic step.
//
//  3. Reclaiming an expired lease (ReclaimExpired) flips state back to
//     'ready' and clears the lease, but deliberately does NOT itself
//     increment fencing_token -- the increment happens exactly once, at the
//     next successful ClaimWork, per invariant 2 above. This still
//     satisfies §12.4's "reclaiming an expired lease increments the fencing
//     token": the token a reclaimed-then-reclaimed(-again) row carries
//     before anyone re-claims it is irrelevant, because nothing holds a
//     lease on a 'ready' row and therefore nothing can call CompleteWork or
//     ExtendLease against it (both require state = 'leased'). What matters
//     operationally -- a late holder of the OLD lease can never again
//     satisfy the completion guard -- holds regardless of which statement
//     performs the arithmetic. Centralizing the increment in ClaimWork means
//     a work item that gets reclaimed N times before anyone re-claims it
//     still only pays for one increment, not N.
//
//  4. Every completion (CompleteWork) and lease renewal (ExtendLease) is a
//     single UPDATE guarded by work ID + current state ('leased') + lease
//     owner + fencing token + attempt, all in one WHERE clause. Zero rows
//     affected means the guard failed -- the caller no longer holds the
//     lease it thinks it holds -- and both methods report that as the typed
//     ErrStaleClaim sentinel rather than silently no-op'ing. A late worker
//     therefore cannot commit a completion (or extend a lease) over a newer
//     attempt: by the time its stale write lands, either a newer claim has
//     already bumped fencing_token/attempt out from under it, or the row is
//     no longer even in 'leased' state.
//
// Test-to-recovery-matrix mapping (§20.4). Each test below (claiming_test.go
// unless noted) is written to prove one row of the recovery matrix, not just
// to exercise a method:
//
//   - TestClaimWorkIsExclusiveUnderConcurrency proves "SQS signal is
//     duplicated | PostgreSQL claim permits one current owner": two
//     concurrent ClaimWork calls over one ready row, exactly one wins.
//   - TestReclaimExpiredThenClaimGetsHigherFencingToken proves "Worker dies
//     before dispatch | Lease expires; another worker claims" (and, in the
//     same shape, "Scheduler restarts | Due timers are reclaimed"): after
//     ReclaimExpired, a second claim succeeds and carries a strictly higher
//     fencing token than the first.
//   - TestCompleteWorkStaleTokenRejected and
//     TestExtendLeaseWrongOwnerRejected prove "Actor callback arrives after
//     a newer attempt | Record as late; fencing rejects state change": a
//     write against an old fencing token, or from the wrong lease owner,
//     is rejected with ErrStaleClaim rather than silently applied.
//   - tests/fault/claiming_fault_test.go's two-OS-process test proves the
//     same "PostgreSQL claim permits one current owner" row under actual
//     process-level concurrency (not goroutines): 50 ready items, two
//     worker binaries, every item completed exactly once.
//   - tests/fault/claiming_fault_test.go's kill -9 test proves "Worker dies
//     before dispatch | Lease expires; another worker claims" end-to-end,
//     including real process death and a real elapsed-time deadline
//     (lease expiry + 5s).
//   - tests/fault/claiming_fault_test.go's duplicate-signal test proves "SQS
//     signal is duplicated | PostgreSQL claim permits one current owner"
//     one level up: two independent ready rows for the same logical
//     node-run (as two enqueue calls for one duplicated signal would
//     produce) both complete at the work_items layer (technical status),
//     but a UNIQUE(node_run_id, attempt) guard on the results table
//     admits only one effective completion -- domain outcome is not the
//     same thing as technical status (repo CLAUDE.md ground rule).

// WorkItem is the input to Store.EnqueueWork: a unit of ready work bound to
// a node run. AvailableAt defaults to now() when zero.
type WorkItem struct {
	NamespaceID string
	NodeRunID   string
	AvailableAt time.Time
}

// ClaimedWork is a work_items row as returned by Store.ClaimWork: the full
// lease state a caller needs to later call Store.CompleteWork or
// Store.ExtendLease against the same claim.
type ClaimedWork struct {
	ID             string
	NamespaceID    string
	NodeRunID      string
	State          string
	StateVersion   int64
	LeaseOwner     string
	LeaseExpiresAt time.Time
	FencingToken   int64
	Attempt        int32
	AvailableAt    time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ErrStaleClaim is returned by Store.CompleteWork and Store.ExtendLease
// when the guarded UPDATE affects zero rows: the caller's (work ID, lease
// owner, fencing token, attempt) tuple no longer matches the row's current
// lease, because a newer claim has since taken it, the lease was reclaimed,
// or the work item is not in a leased state at all. This is the typed
// surface of §12.4's "late workers cannot commit over a newer attempt".
var ErrStaleClaim = errors.New("postgres: stale claim: work item is not currently leased under this owner/fencing-token/attempt")

const enqueueWorkSQL = `
INSERT INTO work_items (id, namespace_id, node_run_id, state, available_at)
VALUES ($1, $2, $3, 'ready', $4)
`

// EnqueueWork inserts a new ready work_items row. AvailableAt defaults to
// now() when zero, matching Store.InsertOutbox's convention.
func (s *Store) EnqueueWork(ctx context.Context, in WorkItem) error {
	switch {
	case in.NamespaceID == "":
		return fmt.Errorf("postgres: EnqueueWork: namespaceID is required")
	case in.NodeRunID == "":
		return fmt.Errorf("postgres: EnqueueWork: nodeRunID is required")
	}

	if _, err := s.pool.Exec(ctx, enqueueWorkSQL,
		store.NewULID(), in.NamespaceID, in.NodeRunID, tsOrNow(in.AvailableAt),
	); err != nil {
		return fmt.Errorf("postgres: EnqueueWork: %w", err)
	}
	return nil
}

// claimWorkSQL is the atomic claim: the subquery picks up to $3 ready,
// due rows under FOR UPDATE SKIP LOCKED (so a concurrent claimant never
// blocks on, or double-wins, a row another claimant already has locked),
// and the outer UPDATE flips exactly those rows to 'leased' under this
// caller's owner, a fresh lease expiry, and an incremented fencing token
// and attempt -- all in the one statement, so "picked" and "won" cannot
// diverge.
const claimWorkSQL = `
UPDATE work_items AS w
SET state            = 'leased',
    lease_owner      = $1,
    lease_expires_at = now() + ($2 * interval '1 second'),
    fencing_token    = w.fencing_token + 1,
    attempt          = w.attempt + 1,
    state_version    = w.state_version + 1,
    updated_at       = now()
FROM (
    SELECT id
    FROM work_items
    WHERE namespace_id = $4 AND state = 'ready' AND available_at <= now()
    ORDER BY available_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT $3
) AS claimable
WHERE w.id = claimable.id
RETURNING w.id, w.namespace_id, w.node_run_id, w.state, w.state_version,
          w.lease_owner, w.lease_expires_at, w.fencing_token, w.attempt,
          w.available_at, w.created_at, w.updated_at
`

// ClaimWork atomically claims up to limit ready, due work_items rows for
// workerID, leasing each for leaseDuration. It returns the rows it won --
// possibly fewer than limit, possibly zero when nothing is claimable right
// now -- never an error merely because there was nothing to claim.
func (s *Store) ClaimWork(ctx context.Context, namespaceID, workerID string, leaseDuration time.Duration, limit int) ([]ClaimedWork, error) {
	switch {
	case namespaceID == "":
		return nil, fmt.Errorf("postgres: ClaimWork: namespaceID is required (claiming is namespace-scoped: a worker serves one namespace and must never lease another's work)")
	case workerID == "":
		return nil, fmt.Errorf("postgres: ClaimWork: workerID is required")
	case leaseDuration <= 0:
		return nil, fmt.Errorf("postgres: ClaimWork: leaseDuration must be positive")
	case limit <= 0:
		return nil, fmt.Errorf("postgres: ClaimWork: limit must be positive")
	}

	rows, err := s.pool.Query(ctx, claimWorkSQL, workerID, leaseDuration.Seconds(), int64(limit), namespaceID)
	if err != nil {
		return nil, fmt.Errorf("postgres: ClaimWork: %w", err)
	}
	defer rows.Close()

	var claimed []ClaimedWork
	for rows.Next() {
		var (
			id, namespaceID, nodeRunID, state string
			stateVersion, fencingToken        int64
			attempt                           int32
			leaseOwner                        pgtype.Text
			leaseExpiresAt                    pgtype.Timestamptz
			availableAt, createdAt, updatedAt pgtype.Timestamptz
		)
		if err := rows.Scan(
			&id, &namespaceID, &nodeRunID, &state, &stateVersion,
			&leaseOwner, &leaseExpiresAt, &fencingToken, &attempt,
			&availableAt, &createdAt, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("postgres: ClaimWork: scan: %w", err)
		}
		claimed = append(claimed, ClaimedWork{
			ID:             id,
			NamespaceID:    namespaceID,
			NodeRunID:      nodeRunID,
			State:          state,
			StateVersion:   stateVersion,
			LeaseOwner:     textOrEmpty(leaseOwner),
			LeaseExpiresAt: tsValue(leaseExpiresAt),
			FencingToken:   fencingToken,
			Attempt:        attempt,
			AvailableAt:    tsValue(availableAt),
			CreatedAt:      tsValue(createdAt),
			UpdatedAt:      tsValue(updatedAt),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: ClaimWork: %w", err)
	}

	return claimed, nil
}

// reclaimExpiredSQL flips every 'leased' row whose lease has expired back
// to 'ready', available immediately, with the lease cleared. It
// deliberately leaves fencing_token untouched -- see the invariant-3 doc
// comment above the package-level block at the top of this file.
const reclaimExpiredSQL = `
UPDATE work_items
SET state            = 'ready',
    lease_owner      = NULL,
    lease_expires_at = NULL,
    available_at     = now(),
    state_version    = state_version + 1,
    updated_at       = now()
WHERE state = 'leased'
  AND lease_expires_at IS NOT NULL
  AND lease_expires_at <= now()
`

// ReclaimExpired flips every work item whose lease has expired back to
// 'ready' (available immediately) and returns how many rows it reclaimed.
// It is safe to call concurrently and repeatedly (e.g. from every worker's
// poll loop, or a dedicated scheduler): PostgreSQL's normal row-level
// locking on UPDATE means two concurrent callers racing over the same
// expired row never both reclaim it -- the second caller's WHERE clause is
// re-evaluated against the first caller's already-committed change once its
// row lock is released, so it simply no longer matches.
func (s *Store) ReclaimExpired(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, reclaimExpiredSQL)
	if err != nil {
		return 0, fmt.Errorf("postgres: ReclaimExpired: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// completeWorkSQL guards the completion write with the full lease tuple:
// work ID, expected state ('leased' -- a work item can only be completed
// while actively leased), lease owner, fencing token, and attempt. Zero
// rows affected means at least one of those no longer matches.
const completeWorkSQL = `
UPDATE work_items
SET state            = 'completed',
    lease_owner      = NULL,
    lease_expires_at = NULL,
    state_version    = state_version + 1,
    updated_at       = now()
WHERE id = $1
  AND state = 'leased'
  AND lease_owner = $2
  AND fencing_token = $3
  AND attempt = $4
`

// CompleteWork marks a claimed work item completed, guarded by workID +
// the caller's workerID + fencingToken + expectedAttempt matching the
// row's current lease exactly. It returns ErrStaleClaim -- never a silent
// no-op -- when that guard fails, e.g. because a newer claim already took
// the item, the lease was reclaimed out from under the caller, or the item
// was already completed by someone else.
func (s *Store) CompleteWork(ctx context.Context, workID, workerID string, fencingToken int64, expectedAttempt int) error {
	switch {
	case workID == "":
		return fmt.Errorf("postgres: CompleteWork: workID is required")
	case workerID == "":
		return fmt.Errorf("postgres: CompleteWork: workerID is required")
	}

	tag, err := s.pool.Exec(ctx, completeWorkSQL, workID, workerID, fencingToken, int32(expectedAttempt))
	if err != nil {
		return fmt.Errorf("postgres: CompleteWork: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleClaim
	}
	return nil
}

// extendLeaseSQL guards a lease renewal the same way completeWorkSQL guards
// completion, minus the attempt check (a heartbeat does not need to state
// which attempt it is extending, only which lease): work ID, expected
// state, lease owner, and fencing token must all still match.
const extendLeaseSQL = `
UPDATE work_items
SET lease_expires_at = now() + ($4 * interval '1 second'),
    updated_at        = now()
WHERE id = $1
  AND state = 'leased'
  AND lease_owner = $2
  AND fencing_token = $3
`

// ExtendLease renews a claimed work item's lease by extension, guarded by
// workID + workerID + fencingToken matching the row's current lease. It
// returns ErrStaleClaim when that guard fails, for the same reasons
// CompleteWork does.
func (s *Store) ExtendLease(ctx context.Context, workID, workerID string, fencingToken int64, extension time.Duration) error {
	switch {
	case workID == "":
		return fmt.Errorf("postgres: ExtendLease: workID is required")
	case workerID == "":
		return fmt.Errorf("postgres: ExtendLease: workerID is required")
	case extension <= 0:
		return fmt.Errorf("postgres: ExtendLease: extension must be positive")
	}

	tag, err := s.pool.Exec(ctx, extendLeaseSQL, workID, workerID, fencingToken, extension.Seconds())
	if err != nil {
		return fmt.Errorf("postgres: ExtendLease: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleClaim
	}
	return nil
}
