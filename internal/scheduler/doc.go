// Package scheduler implements the durable-timer half of the runtime
// (docs/initial-design/culture-nodes-prd-spec.md §12.7): "Waits, retries,
// deadlines, and lease recovery are durable rows, not in-memory timers. The
// scheduler claims due timers in bounded batches and writes the resulting
// state change and outbox event transactionally."
//
// # Single active, standby instances
//
// Per §19.3 ("scheduler begins as one active lease holder with standby
// instances"), Scheduler.Run is safe to start on every scheduler process in
// a deployment: at any moment at most one of them is doing real work
// ("active"), and the rest are polling to become active ("standby"). That
// role is decided by a single, well-known PostgreSQL advisory lock held on
// a dedicated connection (see lockKey and tryAcquireLock) -- not by leader
// election in application code, a distributed lock service, or anything
// else PostgreSQL itself does not already guarantee. Whoever holds that
// session-level lock is active; everyone else backs off and retries on an
// interval. §20.4's "Scheduler restarts | Due timers are reclaimed" is a
// direct, automatic consequence of one PostgreSQL property this design
// leans on deliberately: a session-level advisory lock is released the
// instant its holding session ends, for any reason, including the holder's
// process crashing outright -- nobody has to detect the crash and call
// unlock; the next standby's pg_try_advisory_lock simply starts succeeding.
//
// # At-least-once firing, by design
//
// Scheduler.fireOne processes one timer per transaction: apply the timer's
// kind-specific effect, mark it fired (postgres.MarkFiredTx), and insert
// its outbox audit event, then commit. All three succeed together or none
// do. A crash (or an injected Hooks.AfterEffect failure -- see fireOne's
// doc comment) between applying the effect and committing leaves the timer
// still 'pending' (postgres.ClaimDueTimers never itself transitions
// status -- see its doc comment), so the very next claim -- by this same
// instance on its next tick, or by a standby that has since taken over --
// retries the whole timer from scratch. That is at-least-once delivery,
// the same guarantee internal/events.Relay documents for outbox
// publication, and for the same reason: the alternative (mark fired first,
// apply the effect second) can silently drop a timer's effect on a crash,
// which is exactly the failure this design exists to make impossible.
// Every effect this package applies is written to tolerate being re-applied
// (see applyEffect's doc comment).
//
// # Lease recovery is a standing duty, not only timer-driven
//
// §20.4's "Worker dies before dispatch | Lease expires; another worker
// claims" depends on some process actually calling
// (*postgres.Store).ReclaimExpired regularly -- an expired lease does not
// reclaim itself. Scheduler.tick calls it once per tick, unconditionally,
// independently of whether any TimerKindLeaseRecovery timer happens to be
// due right now. A lease_recovery timer (see applyEffect) is a second,
// narrower path to the same call -- useful for durably scheduling recovery
// at a specific future instant -- not a replacement for the standing sweep.
package scheduler
