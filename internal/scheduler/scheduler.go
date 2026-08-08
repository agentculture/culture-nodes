package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	idstore "github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// lockKey names the single, well-known PostgreSQL advisory lock this
// package's active/standby role contends for. It is deliberately one key
// for the whole scheduler role (prd-spec §19.3 speaks of "one active lease
// holder" in the singular, not one per namespace or per timer kind): this
// package coordinates *which process* runs the timer loop, not which
// timers that process is allowed to touch (that scoping is ClaimDueTimers'
// job, per namespace-agnostic timers just like work_items/ClaimWork).
const lockKey = "culture-nodes:scheduler:active-lease"

// tryAdvisoryLockSQL and the unlock/holder queries below all key off the
// same hashtextextended($1, 0) encoding store.go's InsertEvent already uses
// for its own (transaction-scoped) per-aggregate lock, so a lock key here
// reads the same way there does: a stable name, not a magic number.
const tryAdvisoryLockSQL = `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`
const advisoryUnlockSQL = `SELECT pg_advisory_unlock(hashtextextended($1, 0))`

const (
	defaultTickInterval = time.Second
	defaultBatchSize    = 100
)

// Status is whether a Scheduler instance currently holds the single-active
// advisory lock (StatusActive) or is polling to acquire it (StatusStandby).
type Status string

const (
	StatusStandby Status = "standby"
	StatusActive  Status = "active"
)

// HealthReport is a point-in-time snapshot returned by Scheduler.Health.
type HealthReport struct {
	// Status is this instance's current role.
	Status Status
	// LastTick is when this instance last completed a tick while active.
	// It is the zero time.Time if this instance has never completed a
	// tick (e.g. it is still standby, or just became active).
	LastTick time.Time
	// LastTickErr is the error from the most recent tick, or "" if that
	// tick succeeded (or none has run yet). A tick error does not stop the
	// scheduler -- the next tick retries -- this is purely observability.
	LastTickErr string
}

// Hooks lets tests inject failure points into fireOne's per-timer
// processing that would otherwise require killing a real OS process to
// exercise. The zero value performs no injection -- production callers
// never set this field.
type Hooks struct {
	// AfterEffect, if set, is called once a due timer's kind-specific
	// effect has been applied inside its still-open, uncommitted per-timer
	// transaction, before that timer is marked fired. Returning an error
	// aborts and rolls back the whole transaction -- undoing the effect
	// along with everything else -- exactly as a real crash at that exact
	// point would leave things: see fireOne's doc comment for why that is
	// safe to retry on the next tick.
	AfterEffect func(t postgres.Timer) error
}

// Options configures a Scheduler. The zero value is valid: every field
// defaults as documented below.
type Options struct {
	// OwnerID identifies this instance in timers.claimed_by and in the
	// outbox/work_items provenance its ticks produce. Defaults to a fresh
	// ULID prefixed "scheduler-".
	OwnerID string
	// TickInterval is how often an active instance claims and fires due
	// timers. Defaults to 1s.
	TickInterval time.Duration
	// StandbyRetryInterval is how often a standby instance retries the
	// advisory lock. Defaults to TickInterval.
	StandbyRetryInterval time.Duration
	// BatchSize bounds how many due timers one tick's ClaimDueTimers call
	// claims. Defaults to 100.
	BatchSize int
	// Hooks injects test-only failure points (see the Hooks doc comment).
	// The zero value never injects anything.
	Hooks Hooks
}

func (o Options) tickInterval() time.Duration {
	if o.TickInterval > 0 {
		return o.TickInterval
	}
	return defaultTickInterval
}

func (o Options) standbyRetryInterval() time.Duration {
	if o.StandbyRetryInterval > 0 {
		return o.StandbyRetryInterval
	}
	return o.tickInterval()
}

func (o Options) batchSize() int {
	if o.BatchSize > 0 {
		return o.BatchSize
	}
	return defaultBatchSize
}

// Scheduler is the single-active/standby durable-timer loop (see the
// package doc comment). The zero value is not usable; construct one with
// New.
type Scheduler struct {
	db   *postgres.Store
	pool *pgxpool.Pool
	opts Options

	mu       sync.RWMutex
	status   Status
	lastTick time.Time
	lastErr  string
}

// New returns a Scheduler backed by db. It does nothing until Run is
// called -- constructing one never touches PostgreSQL.
func New(db *postgres.Store, opts Options) *Scheduler {
	if opts.OwnerID == "" {
		opts.OwnerID = "scheduler-" + idstore.NewULID()
	}
	return &Scheduler{
		db:     db,
		pool:   db.Pool(),
		opts:   opts,
		status: StatusStandby,
	}
}

// OwnerID reports the identifier this instance stamps into
// timers.claimed_by and outbox/work_items provenance -- the effective
// value after Options.OwnerID's default was applied.
func (sch *Scheduler) OwnerID() string { return sch.opts.OwnerID }

// Health returns a snapshot of this instance's current role and last tick.
func (sch *Scheduler) Health() HealthReport {
	sch.mu.RLock()
	defer sch.mu.RUnlock()
	return HealthReport{Status: sch.status, LastTick: sch.lastTick, LastTickErr: sch.lastErr}
}

func (sch *Scheduler) setStatus(s Status) {
	sch.mu.Lock()
	sch.status = s
	sch.mu.Unlock()
}

func (sch *Scheduler) recordTick(err error) {
	sch.mu.Lock()
	sch.lastTick = time.Now().UTC()
	if err != nil {
		sch.lastErr = err.Error()
	} else {
		sch.lastErr = ""
	}
	sch.mu.Unlock()
}

// Run is the scheduler's whole life cycle: alternate between standby
// (polling for the advisory lock) and active (ticking until the lock is
// lost or ctx is done), forever, until ctx is done. It returns ctx.Err()
// when ctx is done, and a non-nil error only for a failure that is not
// "someone else currently holds the lock" -- e.g. the pool itself refusing
// new connections.
//
// Run is meant to be started once per process, in its own goroutine; every
// scheduler process in a deployment runs the exact same Run loop (see the
// package doc comment's "single active, standby instances" section) -- the
// active/standby decision happens inside Run, not by a caller choosing
// which mode to start in.
func (sch *Scheduler) Run(ctx context.Context) error {
	sch.setStatus(StatusStandby)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		conn, acquired, err := sch.tryAcquireLock(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("scheduler: acquire advisory lock: %w", err)
		}
		if !acquired {
			if !sleepCtx(ctx, sch.opts.standbyRetryInterval()) {
				return ctx.Err()
			}
			continue
		}

		sch.runActive(ctx, conn)
		sch.setStatus(StatusStandby)
		// Loop back: either ctx is now done (the top-of-loop check above
		// returns on the next pass) or the lock was merely lost (a failed
		// Ping in runActive), in which case we retry acquiring it exactly
		// like a standby would.
	}
}

// tryAcquireLock acquires a connection dedicated to holding the
// single-active advisory lock for as long as this instance remains active.
// It Hijacks the connection out of pool's own management (pgxpool.Conn.
// Hijack) precisely so that connection is never handed back into general
// circulation while it might be holding the lock -- see the package doc
// comment. On any failure, or if the lock is already held elsewhere, the
// connection is closed and (false, nil) or an error is returned; it is
// never leaked back into pool in a state pool doesn't know about.
func (sch *Scheduler) tryAcquireLock(ctx context.Context) (*pgx.Conn, bool, error) {
	pooled, err := sch.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire pooled connection: %w", err)
	}
	raw := pooled.Hijack()

	var locked bool
	if err := raw.QueryRow(ctx, tryAdvisoryLockSQL, lockKey).Scan(&locked); err != nil {
		closeHijacked(raw)
		return nil, false, fmt.Errorf("pg_try_advisory_lock: %w", err)
	}
	if !locked {
		closeHijacked(raw)
		return nil, false, nil
	}
	return raw, true, nil
}

func closeHijacked(conn *pgx.Conn) {
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Close(closeCtx)
}

// runActive ticks on conn's lock -- which it owns exclusively until this
// call returns -- until ctx is done or the lock is discovered lost (a
// failed Ping: the connection died, e.g. because another process forcibly
// terminated it, simulating the crash that lets a standby take over). It
// always closes conn before returning, releasing the advisory lock either
// explicitly (best-effort pg_advisory_unlock, for a clean shutdown) or
// simply by ending the session (if the connection is already unusable) --
// PostgreSQL guarantees the latter releases the lock regardless, which is
// what makes standby takeover automatic (see the package doc comment).
func (sch *Scheduler) runActive(ctx context.Context, conn *pgx.Conn) {
	sch.setStatus(StatusActive)
	defer sch.releaseLock(conn)

	ticker := time.NewTicker(sch.opts.tickInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if err := conn.Ping(ctx); err != nil {
			// The dedicated lock connection is gone: our session (and
			// therefore the advisory lock it held) has already ended.
			// Someone else can now acquire it. Stop being active and let
			// Run's outer loop retry acquiring, exactly like a standby.
			return
		}

		sch.recordTick(sch.tick(ctx))
	}
}

func (sch *Scheduler) releaseLock(conn *pgx.Conn) {
	if conn.IsClosed() {
		return
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Best-effort: if this Exec fails (e.g. the connection died between
	// runActive's last successful Ping and here), the subsequent Close
	// still ends the session, which releases the lock regardless -- see
	// this method's doc comment.
	_, _ = conn.Exec(closeCtx, advisoryUnlockSQL, lockKey)
	_ = conn.Close(closeCtx)
}

// sleepCtx sleeps for d or until ctx is done, whichever comes first. It
// reports whether it returned because d elapsed (true) as opposed to ctx
// ending the wait early (false).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// tick is one bounded unit of active-scheduler work: sweep expired leases
// (the standing duty -- see the package doc comment), then claim and fire
// up to opts.batchSize() due timers. A per-timer failure inside fireOne
// does not abort the batch or fail tick as a whole -- see fireOne's doc
// comment for why that timer simply stays 'pending' for the next tick to
// retry. tick returns an error only for an infrastructure-level failure
// (ReclaimExpired or ClaimDueTimers itself erroring), which
// Scheduler.recordTick surfaces through Health without stopping the loop.
func (sch *Scheduler) tick(ctx context.Context) error {
	if _, err := sch.db.ReclaimExpired(ctx); err != nil {
		return fmt.Errorf("scheduler: tick: ReclaimExpired: %w", err)
	}

	due, err := sch.db.ClaimDueTimers(ctx, sch.opts.OwnerID, time.Now().UTC(), sch.opts.batchSize())
	if err != nil {
		return fmt.Errorf("scheduler: tick: ClaimDueTimers: %w", err)
	}

	for _, t := range due {
		// fireOne's own errors (a stale claim, an injected test failure, a
		// bad subject) are expected/recoverable outcomes, not tick
		// failures: the timer involved simply did not commit and stays
		// 'pending' for the next claim to retry. See fireOne's doc
		// comment.
		_ = sch.fireOne(ctx, t)
	}
	return nil
}

// fireOne processes exactly one claimed timer inside exactly one
// transaction: apply its kind-specific effect (applyEffect), mark it fired
// (postgres.MarkFiredTx), and insert its outbox audit event, then commit.
// All three happen together or none do -- see the package doc comment's
// "at-least-once firing" section for why that all-or-nothing shape is what
// makes retrying a crashed attempt safe.
//
// Two distinct things can make fireOne stop short of committing, and both
// are handled the same way (roll back, return an error the caller
// deliberately ignores -- see tick):
//
//   - applyEffect fails, or the Hooks.AfterEffect test hook returns an
//     error (simulating a crash between the effect and MarkFiredTx without
//     needing to kill a real process) -- the transaction rolls back via
//     the deferred Rollback, undoing the effect too.
//   - MarkFiredTx reports fired == false: this timer's claim has since
//     been taken by someone else (see claimDueTimersSQL's doc comment for
//     when that can legitimately happen -- e.g. a standby that took over
//     mid-batch). Rolling back here is what stops this caller from
//     committing an effect and an outbox event for a timer another
//     instance now owns.
func (sch *Scheduler) fireOne(ctx context.Context, t postgres.Timer) error {
	tx, err := sch.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: fireOne: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	topic, err := sch.applyEffect(ctx, tx, t)
	if err != nil {
		return fmt.Errorf("scheduler: fireOne: apply effect for timer %s: %w", t.ID, err)
	}

	if sch.opts.Hooks.AfterEffect != nil {
		if err := sch.opts.Hooks.AfterEffect(t); err != nil {
			return fmt.Errorf("scheduler: fireOne: AfterEffect hook for timer %s: %w", t.ID, err)
		}
	}

	fired, err := postgres.MarkFiredTx(ctx, tx, t.ID, sch.opts.OwnerID)
	if err != nil {
		return fmt.Errorf("scheduler: fireOne: MarkFiredTx for timer %s: %w", t.ID, err)
	}
	if !fired {
		return nil
	}

	if _, err := postgres.InsertOutboxTx(ctx, sch.db, tx, postgres.InsertOutboxInput{
		NamespaceID: t.NamespaceID,
		Topic:       topic,
		Payload:     timerOutboxPayload(t),
	}); err != nil {
		return fmt.Errorf("scheduler: fireOne: InsertOutboxTx for timer %s: %w", t.ID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("scheduler: fireOne: commit for timer %s: %w", t.ID, err)
	}
	committed = true
	return nil
}

// Outbox topics this package emits. Not part of internal/events'
// Type* constants (that package documents its list as illustrative, not
// closed) -- these are added here, where they are used, rather than by
// editing that existing file for a new set of types it does not otherwise
// need to know about.
const (
	topicTimerFired         = "dev.culture.nodes.timer.fired"
	topicDeadlineExpired    = "dev.culture.nodes.timer.deadline-expired"
	topicLeaseRecoverySwept = "dev.culture.nodes.timer.lease-recovery-swept"
)

// makeWorkItemAvailableSQL is the wait/retry effect (prd-spec §12.7): flip
// the subject work item back to 'ready', available now. It intentionally
// does not require the row to currently be in any particular state other
// than "not completed" -- re-running this UPDATE (e.g. because fireOne's
// transaction rolled back and the timer is being retried) is a pure
// overwrite of available_at/state to the same target values, which is
// naturally idempotent: applying it twice, or twice with a crash in
// between, leaves the row in exactly the state one successful application
// would have. state_version still advances on every application (matching
// claiming.go's own convention of bumping it on every state-affecting
// write), which is safe precisely because nothing downstream treats a
// state_version bump alone as a distinct, external event -- only the
// timer's own single outbox insert is that.
const makeWorkItemAvailableSQL = `
UPDATE work_items
SET available_at = now(),
    state = 'ready',
    state_version = state_version + 1,
    updated_at = now()
WHERE node_run_id = $1 AND state <> 'completed'
`

// applyEffect performs t's kind-specific effect (prd-spec §12.7) inside tx
// and returns the outbox topic fireOne should record for it. It is always
// called before Hooks.AfterEffect and before MarkFiredTx -- see fireOne's
// doc comment.
func (sch *Scheduler) applyEffect(ctx context.Context, tx pgx.Tx, t postgres.Timer) (topic string, err error) {
	switch t.Kind {
	case postgres.TimerKindWait, postgres.TimerKindRetry:
		if t.NodeRunID == "" {
			return "", fmt.Errorf("timer %s (kind %s) has no node_run_id subject", t.ID, t.Kind)
		}
		if _, err := tx.Exec(ctx, makeWorkItemAvailableSQL, t.NodeRunID); err != nil {
			return "", fmt.Errorf("make work item available for node run %s: %w", t.NodeRunID, err)
		}
		// Zero rows affected is not an error: the subject work item may
		// already be completed (a stale wait/retry timer firing after the
		// node run finished by some other path), which is a legitimate,
		// harmless no-op -- domain outcome, not engine failure (repo
		// CLAUDE.md ground rule).
		return topicTimerFired, nil

	case postgres.TimerKindDeadline:
		// The effect *is* the outbox event: prd-spec §12.7 names this
		// explicitly ("kind deadline -> insert a deadline-expired outbox
		// event"). There is no state to mutate here -- reacting to a
		// deadline (e.g. failing the waiting_external attempt, prd-spec
		// §12.6) is the engine's job downstream of this event, not the
		// scheduler's.
		return topicDeadlineExpired, nil

	case postgres.TimerKindLeaseRecovery:
		// Deliberately NOT run through tx: ReclaimExpired is already a
		// single, atomic, idempotent UPDATE (claiming.go) with no
		// dependency on this timer's own commit -- calling it against the
		// store's ambient pool means its effect lands (and stays landed)
		// even if this timer's own transaction later rolls back (e.g. a
		// stale MarkFiredTx guard, or an injected test crash), which is
		// fine: reclaiming an expired lease that is then "reclaimed again"
		// on a retry is a harmless no-op, not a correctness problem.
		if _, err := sch.db.ReclaimExpired(ctx); err != nil {
			return "", fmt.Errorf("lease-recovery timer %s: ReclaimExpired: %w", t.ID, err)
		}
		return topicLeaseRecoverySwept, nil

	default:
		return "", fmt.Errorf("timer %s: unknown timer kind %q", t.ID, t.Kind)
	}
}

// timerOutboxPayload builds the outbox payload for a fired timer: IDs and
// safe metadata only, per internal/events' package doc rule that event
// data must never carry workflow input, node output, or anything else
// large or sensitive. node_run_id is included under that exact key so
// internal/events.Relay's nodeRunIDFromPayload (relay.go) can derive a
// queue.WorkRef.NodeRunID for this event the same way every other outbox
// row's payload does.
func timerOutboxPayload(t postgres.Timer) json.RawMessage {
	payload := struct {
		TimerID   string `json:"timer_id"`
		Kind      string `json:"kind"`
		RunID     string `json:"run_id,omitempty"`
		NodeRunID string `json:"node_run_id,omitempty"`
	}{
		TimerID:   t.ID,
		Kind:      string(t.Kind),
		RunID:     t.RunID,
		NodeRunID: t.NodeRunID,
	}
	// A struct of plain strings never fails to marshal.
	b, _ := json.Marshal(payload)
	return b
}
