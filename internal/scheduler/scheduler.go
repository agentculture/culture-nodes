package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/telemetry"
	"github.com/agentculture/culture-nodes/internal/worker"
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
	// Telemetry instruments the engine's §12.5 completion transaction (task
	// t19) for the deadline-timeout path (commitTimeout below), the one
	// place this package itself drives an engine to completion. The zero
	// value, a nil *telemetry.Provider, is a safe no-op -- see
	// internal/telemetry.Provider's doc comment -- so a Scheduler built
	// without this option (every existing caller, every existing test)
	// behaves exactly as it did before this option existed.
	Telemetry *telemetry.Provider
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

	// engineMu and engines cache a §12.5 engine per namespace, so firing
	// deadline timers across many namespaces does not recompile the
	// contracts.Validator's embedded schemas on every single timer -- see
	// engineFor.
	engineMu sync.Mutex
	engines  map[string]*engine.Engine

	registryMu sync.Mutex
	registries map[string]worker.Registry
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

	topic, cancelInv, err := sch.applyEffect(ctx, tx, t)
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
	if cancelInv != nil {
		// Cancellation is external and best-effort (§13.6). Run it only after
		// the durable timer transaction is closed, and off the singleton tick
		// loop so an unreachable bridge cannot stall unrelated timers.
		go sch.cancelDeadlineInvocation(context.WithoutCancel(ctx), *cancelInv)
	}
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
func (sch *Scheduler) applyEffect(ctx context.Context, tx pgx.Tx, t postgres.Timer) (topic string, cancelInv *actors.PendingInvocation, err error) {
	switch t.Kind {
	case postgres.TimerKindWait, postgres.TimerKindRetry:
		if t.NodeRunID == "" {
			return "", nil, fmt.Errorf("timer %s (kind %s) has no node_run_id subject", t.ID, t.Kind)
		}
		if _, err := tx.Exec(ctx, makeWorkItemAvailableSQL, t.NodeRunID); err != nil {
			return "", nil, fmt.Errorf("make work item available for node run %s: %w", t.NodeRunID, err)
		}
		// Zero rows affected is not an error: the subject work item may
		// already be completed (a stale wait/retry timer firing after the
		// node run finished by some other path), which is a legitimate,
		// harmless no-op -- domain outcome, not engine failure (repo
		// CLAUDE.md ground rule).
		return topicTimerFired, nil, nil

	case postgres.TimerKindDeadline:
		// The effect is prd-spec §12.6's own timeout: fail the
		// waiting_external attempt this deadline belongs to, through the
		// engine's own §12.5 completion transaction -- see
		// failWaitingExternal. That happens through the store's ambient
		// pool, deliberately NOT through tx, for the identical reason
		// TimerKindLeaseRecovery's effect is not (see that case, below):
		// CompleteAttempt opens its own transaction, and this package does
		// not get to nest one inside tx. The tradeoff is the same one that
		// case accepts too -- if tx later rolls back (a stale MarkFiredTx
		// guard, an injected test crash), failWaitingExternal's own commit
		// stays landed and simply is not retried, because it is guarded to
		// be a safe no-op on a retry: see its doc comment.
		inv, paused, err := sch.failWaitingExternal(ctx, t)
		if err != nil {
			return "", nil, fmt.Errorf("deadline timer %s: fail waiting_external attempt: %w", t.ID, err)
		}
		// A paused continuation cancels nothing -- the whole point is that the
		// session stays warm -- and a zero PendingInvocation means
		// failWaitingExternal found nothing to fail at all. Everything else
		// goes to the cancel step, INCLUDING an invocation carrying no
		// actor-assigned id: that one sends no request, but it is still a
		// deadline that expired against a live session, and the run's event
		// stream is where an operator has to be able to see that (task t12).
		// Gating on the invocation id here instead would make the one case
		// where the cancel is impossible the one case that leaves no trace.
		if paused || inv.RunID == "" {
			return topicDeadlineExpired, nil, nil
		}
		return topicDeadlineExpired, &inv, nil

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
			return "", nil, fmt.Errorf("lease-recovery timer %s: ReclaimExpired: %w", t.ID, err)
		}
		return topicLeaseRecoverySwept, nil, nil

	default:
		return "", nil, fmt.Errorf("timer %s: unknown timer kind %q", t.ID, t.Kind)
	}
}

// failWaitingExternal is TimerKindDeadline's effect (prd-spec §12.6): if an
// asynchronous invocation is still waiting_external when its deadline timer
// fires, fail its attempt through the engine's own §12.5 completion
// transaction, the same as a late `failed` callback would.
//
// The guard is the whole safety argument, both for this task's "a completed
// attempt never receives a late deadline failure" requirement and for this
// method's own safe-to-retry contract: InvocationByDeadlineTimer's state is
// read fresh on every call, so a timer that gets here again after a partial
// failure on an earlier attempt (see below), or after some other path
// already closed the invocation, or after there never was an invocation to
// begin with (a hand-built timer in a test), all resolve the same way --
// nothing left to do, return nil, no error.
//
// This mirrors internal/actors.commitTerminal's own two-step shape (resume
// the parked work item, then commit through the engine) and inherits the
// same accepted edge case that comment lists only implicitly: if
// ResumeWaitingWork commits but the subsequent CompleteAttempt fails for a
// genuine infrastructure reason (not ErrStaleClaim), the work item is left
// leased under the fencing tuple ResumeWaitingWork set. A retry of this
// method then finds ResumeWaitingWork itself refusing (the item is no
// longer 'waiting') and returns cleanly rather than looping forever -- the
// resumed lease's own expiry and this package's standing ReclaimExpired
// sweep (see tick) are what eventually recover it, exactly as they would
// for any other worker that went dark mid-dispatch.
func (sch *Scheduler) failWaitingExternal(ctx context.Context, t postgres.Timer) (actors.PendingInvocation, bool, error) {
	inv, closeWait, ok, err := sch.waitForDeadlineTimer(ctx, t)
	if err != nil {
		return actors.PendingInvocation{}, false, err
	}
	if !ok || inv.State != actors.InvocationWaiting {
		return actors.PendingInvocation{}, false, nil
	}
	paused, nextCheck, err := sch.deadlineContinuationHolds(ctx, inv)
	if err != nil {
		return actors.PendingInvocation{}, false, err
	}
	if paused {
		// Pausing without replacing the timer would remove the only thing that
		// ever re-evaluates the bound: this timer has fired, and nothing else
		// schedules another. The session would then run with NO deadline at
		// all, which is precisely the "loop that relies solely on an agent
		// deciding when to stop" PRD §9.7 forbids -- and it would look like
		// the bound was honoured, because the declaration is right there in
		// the graph. Re-arm before pausing, or do not pause.
		if err := sch.rearmDeadline(ctx, t, nextCheck); err != nil {
			return actors.PendingInvocation{}, false,
				fmt.Errorf("re-arm deadline for attempt %s: %w", inv.AttemptID, err)
		}
		return inv, true, nil
	}

	cs, err := postgres.NewCallbackStore(sch.db, t.NamespaceID)
	if err != nil {
		return actors.PendingInvocation{}, false, fmt.Errorf("build callback store for namespace %s: %w", t.NamespaceID, err)
	}

	if err := cs.ResumeWaitingWork(ctx, inv, actors.DefaultResumeLease); err != nil {
		if errors.Is(err, engine.ErrStaleClaim) {
			// Something else -- a racing callback, an operator
			// cancellation, a worker's own lease reclaim -- already moved
			// this attempt on. Nothing to fail.
			return actors.PendingInvocation{}, false, nil
		}
		return actors.PendingInvocation{}, false, fmt.Errorf("resume parked work for attempt %s: %w", inv.AttemptID, err)
	}

	eng, err := sch.engineFor(t.NamespaceID)
	if err != nil {
		return actors.PendingInvocation{}, false, fmt.Errorf("build engine for namespace %s: %w", t.NamespaceID, err)
	}

	if _, err := eng.CompleteAttempt(ctx, engine.CompletionRequest{
		WorkID:       inv.WorkID,
		WorkerID:     inv.WorkerID,
		FencingToken: inv.FencingToken,
		Attempt:      inv.Attempt,
		// The actor the dispatch resolved (migration 0015), carried from the
		// durable invocation rather than inferred. Without it every
		// deadline-expired attempt persisted actor_id NULL and vanished from
		// the per-actor statistics -- so a session that ran until its bound
		// expired was billable work no comparison between actors could see,
		// and task t11's superseding record would then appear to create
		// retry burn out of nothing rather than replace a row that was
		// already there (ADR 0012 §4).
		ActorID:    inv.ActorID,
		TechStatus: engine.StatusTimedOut,
		// Naming the origin is what stops this completion being re-dispatched
		// (task t10, spec claim c42). By the time this call returns, the
		// session behind the attempt has not been asked to stop -- the cancel
		// is issued after fireOne commits, deliberately (decision q3/c48) --
		// so the engine must treat the retry budget as unspendable here. It
		// is the scheduler that knows this, and this is the only place it can
		// say so.
		TimeoutOrigin: engine.TimeoutOriginDeadline,
		Output:        deadlineTimeoutOutput(t),
	}); err != nil {
		if errors.Is(err, engine.ErrStaleClaim) || errors.Is(err, engine.ErrTerminalNodeRun) || errors.Is(err, engine.ErrTerminalRun) {
			// The engine's own fenced guard refused: the resume above won
			// the race, but something newer committed before this call
			// reached the engine. Nothing was written -- the whole §12.5
			// transaction rolled back -- so there is nothing to undo.
			return actors.PendingInvocation{}, false, nil
		}
		return actors.PendingInvocation{}, false, fmt.Errorf("fail attempt %s on deadline: %w", inv.AttemptID, err)
	}

	if err := closeWait(ctx, cs); err != nil {
		return actors.PendingInvocation{}, false, fmt.Errorf("close wait record for attempt %s: %w", inv.AttemptID, err)
	}
	return inv, false, nil
}

// deadlineContinuationHolds reports whether the node's declared continuation
// says to keep going, and WHEN the answer could next change.
//
// The second return value is what makes pausing safe. Of the three bounds,
// only maxWallClock changes with the passage of time; maxSessions and
// maxContinuations change only when a new attempt is recorded, which the
// completion path already re-evaluates. So the next moment a paused node's
// verdict can flip on its own is exactly `node_run.created_at + maxWallClock`.
//
// A zero nextCheck means no time-based bound was declared. The caller must
// then NOT pause: there would be no future moment at which anything reconsiders,
// and "keep the session warm" would silently become "run forever".
func (sch *Scheduler) deadlineContinuationHolds(ctx context.Context, inv actors.PendingInvocation) (bool, time.Time, error) {
	var digest string
	var ir []byte
	var created time.Time
	if err := sch.pool.QueryRow(ctx, `
		SELECT wv.content_digest, wv.normalized_ir, nr.created_at
		FROM runs r JOIN workflow_versions wv ON wv.id = r.workflow_version_id
		JOIN node_runs nr ON nr.id = $2
		WHERE r.id = $1`, inv.RunID, inv.NodeRunID).Scan(&digest, &ir, &created); err != nil {
		return false, time.Time{}, fmt.Errorf("load continuation declaration for node run %s: %w", inv.NodeRunID, err)
	}
	wf, err := engine.LoadWorkflow(digest, ir)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("load workflow for deadline continuation: %w", err)
	}
	node := wf.Nodes[inv.NodeID]
	if node == nil || node.Continue == nil {
		return false, time.Time{}, nil
	}
	var sessions int
	if err := sch.pool.QueryRow(ctx, `SELECT count(*) FROM attempts WHERE node_run_id = $1`, inv.NodeRunID).Scan(&sessions); err != nil {
		return false, time.Time{}, fmt.Errorf("count continuation attempts for node run %s: %w", inv.NodeRunID, err)
	}
	remaining := 0
	if node.Continue.Bounds.MaxSessions > sessions {
		remaining = node.Continue.Bounds.MaxSessions - sessions
	}
	decision := node.DecideContinuation(engine.ContinuationState{
		NodeState: "incomplete", RemainingSessions: remaining,
		Continuations: max(0, sessions-1), Sessions: sessions,
		WallClock: time.Since(created),
	})
	if !decision.Continue {
		return false, time.Time{}, nil
	}
	if node.Continue.Bounds.MaxWallClock <= 0 {
		// Declared bounds, but none of them time-based. Refuse to pause rather
		// than pause unboundedly; the caller cancels, which is the fail-closed
		// reading of "absent or false -> CANCEL".
		return false, time.Time{}, nil
	}
	return true, created.Add(node.Continue.Bounds.MaxWallClock), nil
}

// rearmDeadline replaces a fired deadline timer with the next one, so a paused
// continuation is re-evaluated instead of forgotten. The payload is copied
// verbatim: waitForDeadlineTimer resolves the invocation from those fields, so
// the replacement resolves the same attempt the original did.
func (sch *Scheduler) rearmDeadline(ctx context.Context, t postgres.Timer, fireAt time.Time) error {
	if fireAt.IsZero() {
		return fmt.Errorf("refusing to pause with no future re-check time")
	}
	_, err := sch.pool.Exec(ctx,
		`INSERT INTO timers (id, namespace_id, run_id, node_run_id, timer_kind, fire_at, payload)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		idstore.NewULID(), t.NamespaceID, t.RunID, t.NodeRunID,
		string(postgres.TimerKindDeadline), fireAt, t.Payload)
	return err
}

// waitForDeadlineTimer resolves which durable async record a fired deadline
// timer belongs to, and returns the matching way to retire it.
//
// A deadline timer names a timer, not an attempt, and there are two kinds of
// waiting_external record it may have been scheduled by: an asynchronous ACTOR
// invocation (0009's actor_invocations, resumed by an inbound §13.4 callback)
// and a parked RUNNER operation (0011's runner_invocations, resumed by an
// outbound status sample). Everything after this lookup is identical for both
// — resume under the recorded fencing tuple, complete through the engine's own
// §12.5 transaction, retire the record — which is exactly why the difference
// is confined to this one function instead of a second copy of
// failWaitingExternal. There is one waiting_external timeout rule in this
// system, and this keeps it that way.
//
// The actor table is consulted first only because it is the older and more
// common case; the two are disjoint (a timer id appears in at most one), so
// the order is a cost decision, not a correctness one.
func (sch *Scheduler) waitForDeadlineTimer(
	ctx context.Context, t postgres.Timer,
) (actors.PendingInvocation, func(context.Context, *postgres.CallbackStore) error, bool, error) {
	inv, ok, err := sch.db.InvocationByDeadlineTimer(ctx, t.ID)
	if err != nil {
		return actors.PendingInvocation{}, nil, false, fmt.Errorf("load invocation for deadline timer %s: %w", t.ID, err)
	}
	if ok {
		return inv, func(ctx context.Context, cs *postgres.CallbackStore) error {
			return cs.CloseInvocation(ctx, inv.AttemptID, actors.InvocationCompleted)
		}, true, nil
	}

	inv, ok, err = sch.db.RunnerOperationByDeadlineTimer(ctx, t.ID)
	if err != nil {
		return actors.PendingInvocation{}, nil, false, fmt.Errorf("load runner operation for deadline timer %s: %w", t.ID, err)
	}
	if ok {
		namespaceID := t.NamespaceID
		if namespaceID == "" {
			namespaceID = inv.NamespaceID
		}
		return inv, func(ctx context.Context, _ *postgres.CallbackStore) error {
			return sch.db.CloseRunnerOperation(ctx, namespaceID, inv.AttemptID, postgres.RunnerOperationCompleted)
		}, true, nil
	}

	// A deadline timer that belongs to neither is "nothing to fail" rather
	// than a fault: a hand-built timer in a test, or one whose record some
	// other path already removed.
	return actors.PendingInvocation{}, nil, false, nil
}

// engineFor returns the cached §12.5 engine for namespaceID, building and
// caching one on first use. Engines are namespace-scoped (postgres.NewEngine
// binds one namespace's contracts validator and store queries), and a
// deployment's timers table is not (ClaimDueTimers claims due timers across
// every namespace in one batch -- see this package's doc comment) -- so a
// scheduler that has ever fired a deadline timer for more than one
// namespace ends up holding one engine per namespace it has seen, for as
// long as this Scheduler runs.
func (sch *Scheduler) engineFor(namespaceID string) (*engine.Engine, error) {
	sch.engineMu.Lock()
	defer sch.engineMu.Unlock()

	if eng, ok := sch.engines[namespaceID]; ok {
		return eng, nil
	}
	eng, err := postgres.NewEngine(sch.db, namespaceID, engine.WithTelemetry(sch.opts.Telemetry))
	if err != nil {
		return nil, err
	}
	if sch.engines == nil {
		sch.engines = make(map[string]*engine.Engine)
	}
	sch.engines[namespaceID] = eng
	return eng, nil
}

// deadlineTimeoutOutput is the diagnostic body recorded on the attempt a
// deadline timer fails, mirroring internal/actors' own failureOutput shape
// (a `failed` callback's error body) so an operator sees the same shape of
// diagnostic whichever path produced the timeout.
func deadlineTimeoutOutput(t postgres.Timer) json.RawMessage {
	payload := struct {
		Error struct {
			Class  string `json:"class"`
			Detail string `json:"detail"`
		} `json:"error"`
	}{}
	payload.Error.Class = string(actors.ClassTimeout)
	payload.Error.Detail = fmt.Sprintf("waiting_external deadline timer %s expired before a terminal callback arrived", t.ID)
	encoded, err := json.Marshal(payload)
	if err != nil {
		// A struct of plain strings never fails to marshal; see
		// timerOutboxPayload's identical comment below.
		return json.RawMessage(`{"error":{"class":"timeout"}}`)
	}
	return encoded
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
