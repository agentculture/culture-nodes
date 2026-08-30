-- 0050_schedule_failure_backoff.sql
--
-- Backoff and a singleton alert for a schedule whose work keeps failing the
-- same way (task t9, spec c40/c4/c44, issue #253).
--
-- WHAT WENT WRONG. Migration 0033 gave the control plane a declared cadence,
-- and a cadence has exactly one opinion: it is time again. On 2026-08-29/30
-- the `pr-upkeep-sweep-5m` schedule minted 184 consecutive
-- `contract_rejected` runs whose attempts all reported the SAME
-- `result.error.detail`. Every one of them cost a dispatch, a ledger trail
-- and a share of an operator's attention; not one carried information the
-- first had not already carried. The cadence was doing exactly what it was
-- declared to do, which is why the fix is state on the schedule rather than a
-- change to the declaration.
--
-- WHY SUPPRESSION IS A RATE AND NOT A STOP. Disabling a schedule that starts
-- failing would be a defensible reflex and it is the wrong one: it converts a
-- broken environment into a broken environment PLUS a schedule nobody
-- remembers to re-enable, and it makes the repair invisible -- there is no
-- longer anything running that could notice the environment came back. So a
-- suppressed schedule still mints a PROBE, at most every
-- NODES_SCHEDULE_PROBE_INTERVAL (default 30m). The probe is the half that
-- makes suppression safe to apply without a human in the loop: `enabled`
-- keeps its 0033 meaning ("an operator said stop"), and suppression is a
-- separate, self-clearing fact ("the environment said stop").
--
-- WHY IDENTITY OF THE REASON, NOT MERELY FAILURE. Two different failures in a
-- row are a system in motion; the same failure twice is a system that has
-- already told you everything it is going to tell you. Suppressing on any two
-- failures would throttle a schedule that is genuinely making progress
-- through distinct problems. So the trigger is `last_failure_detail`
-- repeating, and the counter below is a streak of failures carrying ONE
-- detail, reset by any other detail and by any completed run.
--
-- Expand-only (docs/adr/0002-migration-policy.md): five added columns, all
-- nullable or defaulted, nothing dropped, renamed or tightened. An N-1 binary
-- neither reads nor writes any of them, so it fires this table's schedules
-- exactly as it did before -- on the cadence, with no suppression. That is a
-- degraded behaviour, not a broken one, which is the shape the ADR's N-1
-- promise requires.

-- How many ticks this schedule declined to mint on because its last two runs
-- failed identically. Counted rather than inferred, for the same reason 0033
-- counts skip_count: "the schedule held back" and "the schedule never came
-- due" must not look the same to whoever is asking why nothing has run.
ALTER TABLE schedules ADD COLUMN suppressed_count BIGINT NOT NULL DEFAULT 0;

-- The current streak of minted runs that failed carrying last_failure_detail.
-- Reset to 0 by a completed run; reset to 1 by a failure reporting a
-- different detail. It is the input to both decisions: >= 2 suppresses the
-- cadence, and >= NODES_SWEEP_FAILURE_ALERT_AFTER raises the human task.
ALTER TABLE schedules ADD COLUMN consecutive_failures BIGINT NOT NULL DEFAULT 0;

-- The `result.error.detail` of the last minted run's last attempt -- the
-- repeated reason, verbatim, so an operator reading GET /v1alpha1/schedules
-- learns WHY the schedule is holding back without joining to a run. NULL
-- whenever the streak is zero: a schedule whose last run completed is not
-- carrying a reason.
ALTER TABLE schedules ADD COLUMN last_failure_detail TEXT;

-- The run whose outcome the counters above already reflect. Without it a
-- suppressed tick would re-read the same terminal run on every pass and count
-- one failure many times, which would turn a 5-minute cadence into a
-- consecutive_failures counter that climbs six times an hour on its own. The
-- rule this column encodes: one minted run contributes to the streak exactly
-- once.
ALTER TABLE schedules ADD COLUMN last_assessed_run_id TEXT REFERENCES runs (id);

-- The schedule_failing human task this schedule last raised. A real foreign
-- key rather than a boolean, because "is one outstanding" has to be answered
-- by that task's CURRENT status: while it is pending the question has been
-- asked and must not be asked again, and once a human decides it a schedule
-- that is still failing is asking a new question, not repeating an old one.
ALTER TABLE schedules ADD COLUMN failure_task_id TEXT REFERENCES human_tasks (id);
