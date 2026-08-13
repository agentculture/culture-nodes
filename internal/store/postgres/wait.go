package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// Durable wait parks (prd-spec §9.2, §12.7): a `wait` node's dispatch does
// not hold a lease for the wall-clock time the node asks for. It parks the
// claimed work item exactly the way an asynchronous actor dispatch does
// (async.go's StartAsyncWait) — same fenced release, same waiting_external
// node run — but what will eventually wake it is a durable §12.7 wait TIMER,
// not an inbound callback. The scheduler's TimerKindWait effect
// (internal/scheduler.applyEffect) returns the work item to 'ready' when the
// timer fires; the next claim re-dispatches the node, and the wait
// dispatcher (internal/worker/wait.go) then sees a fired timer and completes
// the node run with its declared outcome through the engine's own §12.5
// transaction — which is what keeps the §9.7 loop bounds enforced across the
// park, because the resume goes through planTransition like every other
// completion.

// StartDurableWaitInput parks a leased work item on a durable wait timer.
type StartDurableWaitInput struct {
	// The fencing tuple exactly as ClaimWork handed it out. A mismatch means
	// the caller no longer holds the claim and nothing is written.
	WorkID       string
	WorkerID     string
	FencingToken int64
	Attempt      int

	NamespaceID string
	RunID       string
	NodeRunID   string
	NodeID      string
	// AttemptID is the protocol attempt id of the dispatch that armed the
	// wait, recorded on the timer payload and the audit event so an operator
	// can tie the park back to the dispatch that produced it.
	AttemptID string

	// TimerID is the durable wait timer's id. The worker derives it
	// deterministically from the node run (internal/worker's waitTimerID), so
	// a crashed-and-redispatched park re-adopts the same timer instead of
	// arming a second one.
	TimerID string
	// FireAt is when the wait elapses and the scheduler should make the work
	// item claimable again.
	FireAt time.Time
}

// insertWaitTimerSQL is deliberately ON CONFLICT DO NOTHING rather than
// scheduleTimerSQL's upsert-returning shape: if the timer row already exists
// (a re-park after an anomalous early re-dispatch), its original fire_at is
// the anchor the wait was armed against and must not move.
const insertWaitTimerSQL = `
INSERT INTO timers (id, namespace_id, run_id, node_run_id, timer_kind, fire_at, payload)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO NOTHING
`

// StartDurableWait commits the whole park in one transaction: release the
// worker's lease without completing the item, mark the node run
// waiting_external, persist the wait timer, and append the audit event.
//
// It is one transaction for the same reason StartAsyncWait is: a parked work
// item with no timer can never be woken, and a timer over a still-leased
// item would race the lease's own expiry. A caller whose fencing tuple no
// longer matches gets engine.ErrStaleClaim and writes nothing.
//
// After this commits, nothing anywhere holds anything about the wait: the
// work item is 'waiting' (invisible to ClaimWork and ReclaimExpired), the
// node run is waiting_external (visible on the run detail surface), and the
// pending timer row is the only thing that will ever wake it. Run
// cancellation retires that timer along with the work item (internal/api's
// cancelRun REAP step), so a cancelled run's wait never fires back to life.
func (s *Store) StartDurableWait(ctx context.Context, in StartDurableWaitInput) error {
	switch {
	case in.WorkID == "":
		return errors.New("postgres: StartDurableWait: workID is required")
	case in.WorkerID == "":
		return errors.New("postgres: StartDurableWait: workerID is required")
	case in.NamespaceID == "":
		return errors.New("postgres: StartDurableWait: namespaceID is required")
	case in.RunID == "" || in.NodeRunID == "":
		return errors.New("postgres: StartDurableWait: runID and nodeRunID are required")
	case in.TimerID == "":
		return errors.New("postgres: StartDurableWait: timerID is required")
	case in.FireAt.IsZero():
		return errors.New("postgres: StartDurableWait: fireAt is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: StartDurableWait: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	// The same per-run advisory lock the engine and the ledger take, so this
	// transition queues behind (or ahead of) a completion or a cancellation
	// for the same run rather than interleaving with one.
	if _, err := tx.Exec(ctx, advisoryXactLockSQL, ledger.RunLockKey(in.RunID)); err != nil {
		return fmt.Errorf("postgres: StartDurableWait: lock run %s: %w", in.RunID, err)
	}

	tag, err := tx.Exec(ctx, parkWorkSQL, in.WorkID, in.WorkerID, in.FencingToken, int32(in.Attempt))
	if err != nil {
		return fmt.Errorf("postgres: StartDurableWait: park work item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: StartDurableWait: work item %s: %w: %w", in.WorkID, engine.ErrStaleClaim, ErrStaleClaim)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE node_runs SET status = 'waiting_external', updated_at = now() WHERE id = $1`,
		in.NodeRunID,
	); err != nil {
		return fmt.Errorf("postgres: StartDurableWait: mark node run waiting: %w", err)
	}

	payload, _ := json.Marshal(map[string]string{
		"attempt_id":  in.AttemptID,
		"node_run_id": in.NodeRunID,
	})
	if _, err := tx.Exec(ctx, insertWaitTimerSQL,
		in.TimerID, in.NamespaceID, in.RunID, in.NodeRunID,
		string(TimerKindWait), in.FireAt.UTC(), payload,
	); err != nil {
		return fmt.Errorf("postgres: StartDurableWait: schedule wait timer: %w", err)
	}

	if err := appendRunEventTx(ctx, tx, in.NamespaceID, in.RunID, TypeAttemptWaitingTimer, map[string]any{
		"run_id":        in.RunID,
		"node_run_id":   in.NodeRunID,
		"node_id":       in.NodeID,
		"attempt_id":    in.AttemptID,
		"work_id":       in.WorkID,
		"worker_id":     in.WorkerID,
		"fencing_token": in.FencingToken,
		"attempt":       in.Attempt,
		"timer_id":      in.TimerID,
		"fire_at":       in.FireAt.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: StartDurableWait: commit: %w", err)
	}
	return nil
}

// TypeAttemptWaitingTimer records the transition into a durable timer wait.
// It is a distinct event type from TypeAttemptWaitingExternal for the same
// reason that one is distinct from generic attempt events: "this run is
// waiting on the clock until <fire_at>" is an operational answer, and
// finding it inside a waiting-external event would mean guessing which
// external party is meant when none exists.
const TypeAttemptWaitingTimer = "dev.culture.nodes.attempt.waiting-timer"

const selectTimerByIDSQL = `
SELECT ` + timerColumns + `
FROM timers
WHERE id = $1
`

// TimerByID loads one timer row. It reports (zero, false, nil) rather than
// an error when no timer has this id: the wait dispatcher's "has this node
// run's wait been armed yet" question treats absence as a legitimate answer,
// not a fault.
func (s *Store) TimerByID(ctx context.Context, timerID string) (Timer, bool, error) {
	if timerID == "" {
		return Timer{}, false, fmt.Errorf("postgres: TimerByID: timerID is required")
	}
	t, err := scanTimerRow(s.pool.QueryRow(ctx, selectTimerByIDSQL, timerID))
	if err != nil {
		if isNoRows(err) {
			return Timer{}, false, nil
		}
		return Timer{}, false, fmt.Errorf("postgres: TimerByID: %w", err)
	}
	return t, true, nil
}
