package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
)

// Schedule failure backoff and the singleton failing-schedule alert (task t9,
// spec c40/c4/c44, issue #253). Migration 0050 carries the columns; this file
// is the rule that maintains them, and FireSchedule is its only caller.
//
// # What this is answering
//
// On 2026-08-29/30 the `pr-upkeep-sweep-5m` schedule minted 184 consecutive
// `contract_rejected` runs whose attempts all reported the same
// `result.error.detail`. A cadence has one opinion -- it is time again -- and
// nothing in migration 0033 gave it a way to notice that the last 183 answers
// were identical.
//
// # Why suppression is a rate, not a stop
//
// The obvious fix, disabling a schedule that starts failing, is the wrong
// one. It converts a broken environment into a broken environment PLUS a
// schedule nobody remembers to re-enable, and it destroys the only thing that
// could have noticed the repair: something running. So a suppressed schedule
// still mints a PROBE, at most once per ProbeInterval. `enabled` keeps its
// 0033 meaning ("an operator said stop") and suppression stays a separate,
// self-clearing fact ("the environment said stop"). One completed run puts
// the schedule straight back on its declared cadence with nobody typing
// anything.
//
// # Why identity of the reason and not merely failure
//
// Two different failures in a row are a system in motion; the same failure
// twice is a system that has already said everything it is going to say.
// Suppressing on any two failures would throttle a schedule that is genuinely
// working through distinct problems, so the trigger is the DETAIL repeating.
//
// # Why the alert is one task and not one per failure
//
// 184 identical runs becoming 182 identical human tasks is the same defect
// wearing a different hat. One question is asked; while it is unanswered it is
// not asked again; once a human decides it, a schedule that is STILL failing
// is asking a new question, not repeating an old one -- so re-raising is
// gated on a new terminal run having been assessed, never on a bare tick.

const (
	// DefaultScheduleProbeInterval is NODES_SCHEDULE_PROBE_INTERVAL's default:
	// the floor between mints while a schedule is suppressed.
	//
	// Thirty minutes rather than a backoff curve. An exponential curve
	// optimises for the cost of the probes, which is the smaller number here
	// -- a five-minute sweep suppressed to half-hourly already costs 1/6th of
	// what #253 cost. What it would trade away is the thing suppression exists
	// to protect: a bounded, PREDICTABLE answer to "how long after the fix
	// does this start working again", which an operator can hold in their head
	// and does not have to reconstruct from how long the outage happened to
	// last.
	DefaultScheduleProbeInterval = 30 * time.Minute

	// DefaultSweepFailureAlertAfter is NODES_SWEEP_FAILURE_ALERT_AFTER's
	// default: the consecutive-identical-failure count at which a human is
	// asked. Three, because two is the count that engages suppression and a
	// human asked at two would be asked about every transient double-failure
	// the system is already handling on its own.
	DefaultSweepFailureAlertAfter = 3

	// scheduleSuppressAfter is the streak length at which the declared cadence
	// stops minting. Two is the smallest number that can express "the same
	// reason twice", which is the fact suppression is a response to; it is not
	// configurable because one is not expressible (nothing has repeated yet)
	// and anything larger is what ProbeInterval is for.
	scheduleSuppressAfter = 2

	// ScheduleFailingTaskKind is the human_tasks.kind this file raises. It is
	// NOT an approval node's task: no node run is parked on it and no run
	// resumes when it is decided (PRD §9.9 governs those). It is a question
	// about a schedule, and the run it names is the evidence, not the subject.
	ScheduleFailingTaskKind = "schedule_failing"
)

// scheduleOutcomeSQL reads the outcome of the most recent terminal run the
// schedule's LAST appended event minted.
//
// The join is through runs.trigger_event_id (migration 0043), which is the
// durable link a schedule fire already leaves behind: FireSchedule stores the
// appended signal event in schedules.last_event_id, and engine.TriggerEvent
// stamps that event's id on every run it mints from it. Nothing new has to be
// recorded at mint time for this read to work.
//
// The reason is the last attempt's `result.error.detail` -- the shape
// internal/worker's diagnosticOutput writes for every refusal and every failed
// dispatch. When an attempt reported no such envelope (a contract rejection
// records the actor's own output, which need not carry one) the technical
// status stands in, and the run's status behind that. The fallbacks matter as
// much as the primary: #253's 184 runs were `contract_rejected`, so a rule
// that could only compare error envelopes would not have suppressed the very
// incident it exists for.
const scheduleOutcomeSQL = `
SELECT r.id, r.status,
       COALESCE(NULLIF(a.result->'error'->>'detail', ''), NULLIF(a.status, ''), r.status)
FROM runs r
LEFT JOIN LATERAL (
    SELECT at.result, at.status
    FROM attempts at
    JOIN node_runs nr ON nr.id = at.node_run_id
    WHERE nr.run_id = r.id
    ORDER BY at.completed_at DESC NULLS LAST, at.id DESC
    LIMIT 1
) a ON TRUE
WHERE r.namespace_id = $1 AND r.trigger_event_id = $2 AND r.completed_at IS NOT NULL
ORDER BY r.completed_at DESC, r.id DESC
LIMIT 1`

// assessScheduleOutcomeTx folds the last minted run's outcome into the
// schedule's failure state and, when the streak has reached alertAfter, raises
// the one human task. It mutates sc in place so the caller's suppression
// decision reads post-assessment state, and returns the id of a task it
// raised (empty when it raised none).
//
// It is a no-op unless a NEW terminal run is there to assess. That is the
// whole reason last_assessed_run_id exists: a suppressed five-minute schedule
// re-reads the same failed run twelve times an hour, and counting each read
// would turn the streak into a clock.
func (s *Store) assessScheduleOutcomeTx(ctx context.Context, tx pgx.Tx, sc *Schedule, alertAfter int, now time.Time) (string, error) {
	if sc.LastEventID == "" {
		return "", nil // never fired: there is nothing to have failed
	}

	var runID, runStatus, detail string
	err := tx.QueryRow(ctx, scheduleOutcomeSQL, sc.NamespaceID, sc.LastEventID).Scan(&runID, &runStatus, &detail)
	if err != nil {
		if isNoRows(err) {
			// The last fire either matched no trigger or its run is still
			// going. Neither is a failure, and neither is evidence of one.
			return "", nil
		}
		return "", fmt.Errorf("postgres: FireSchedule: assess schedule %s: %w", sc.ID, err)
	}
	if runID == sc.LastAssessedRunID {
		return "", nil // already folded in
	}

	switch {
	case engine.RunState(runStatus) == engine.RunCompleted:
		// A completed run is the repair, observed. The streak and the reason
		// both go, because a stale reason on a schedule that is working again
		// is worse than no reason: it reads as a current fault.
		sc.ConsecutiveFailures, sc.LastFailureDetail = 0, ""
	case detail == sc.LastFailureDetail:
		sc.ConsecutiveFailures++
	default:
		sc.ConsecutiveFailures, sc.LastFailureDetail = 1, detail
	}
	sc.LastAssessedRunID = runID

	alertTaskID, err := s.raiseScheduleFailingTaskTx(ctx, tx, sc, runID, alertAfter, now)
	if err != nil {
		return "", err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE schedules
		SET consecutive_failures = $2, last_failure_detail = NULLIF($3, ''),
		    last_assessed_run_id = $4, failure_task_id = NULLIF($5, ''), updated_at = now()
		WHERE id = $1`,
		sc.ID, sc.ConsecutiveFailures, sc.LastFailureDetail, sc.LastAssessedRunID, sc.FailureTaskID); err != nil {
		return "", fmt.Errorf("postgres: FireSchedule: record assessment for schedule %s: %w", sc.ID, err)
	}
	return alertTaskID, nil
}

// raiseScheduleFailingTaskTx asks a human about a schedule that keeps failing
// the same way -- once. It returns the id of the task it created, or "" when
// the streak is short, the alert is disabled, or the last question raised is
// still unanswered.
//
// runID is the failing run the task points at. human_tasks.run_id is NOT NULL
// and that is right here rather than merely tolerated: a claim that a schedule
// is failing has to name a run somebody can open, or it is an assertion with
// no evidence behind it (PRD §10.4).
func (s *Store) raiseScheduleFailingTaskTx(ctx context.Context, tx pgx.Tx, sc *Schedule, runID string, alertAfter int, now time.Time) (string, error) {
	if alertAfter < 0 {
		return "", nil
	}
	if alertAfter == 0 {
		alertAfter = DefaultSweepFailureAlertAfter
	}
	if sc.ConsecutiveFailures < int64(alertAfter) {
		return "", nil
	}

	if sc.FailureTaskID != "" {
		var status pgtype.Text
		if err := tx.QueryRow(ctx,
			`SELECT status FROM human_tasks WHERE id = $1`, sc.FailureTaskID).Scan(&status); err != nil && !isNoRows(err) {
			return "", fmt.Errorf("postgres: FireSchedule: read schedule_failing task %s: %w", sc.FailureTaskID, err)
		}
		if textOrEmpty(status) == engine.HumanTaskStatusPending {
			return "", nil // asked, not yet answered
		}
	}

	request, err := json.Marshal(map[string]any{
		"schedule_id":   sc.ID,
		"schedule_name": sc.Name,
		"event_name":    sc.EventName,
		// The repeated reason, verbatim. It is the whole content of the
		// question: "this schedule is failing" is not actionable, "this
		// schedule has failed N times with THIS message" is.
		"reason":               sc.LastFailureDetail,
		"consecutive_failures": sc.ConsecutiveFailures,
		"last_failed_run_id":   runID,
		// A completion claim about a schedule, made by the control plane from
		// facts it read -- never a verdict on whether the work is done
		// (PRD §10.4). A human decides what to do about it.
		"proposed_action": "investigate the failing environment, then decide this task; " +
			"the schedule keeps probing every probe interval and resumes its cadence on its own once a run completes",
	})
	if err != nil {
		return "", fmt.Errorf("postgres: FireSchedule: marshal schedule_failing request for schedule %s: %w", sc.ID, err)
	}

	taskID := store.NewULID()
	if _, err := tx.Exec(ctx, `
		INSERT INTO human_tasks (id, namespace_id, run_id, kind, status, request, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		taskID, sc.NamespaceID, runID, ScheduleFailingTaskKind, engine.HumanTaskStatusPending, request, now); err != nil {
		return "", fmt.Errorf("postgres: FireSchedule: raise schedule_failing task for schedule %s: %w", sc.ID, err)
	}
	sc.FailureTaskID = taskID
	return taskID, nil
}

// scheduleIsSuppressed reports whether this due occurrence should be held
// back: the last two minted runs failed with the same reason, and less than
// one probe interval has passed since the last mint.
//
// It is measured from LastFiredAt rather than from when suppression engaged,
// so the guarantee is the one an operator can check against the runs table --
// "no two runs closer together than the probe interval" -- rather than one
// about internal state they cannot see.
func scheduleIsSuppressed(sc Schedule, probeInterval time.Duration, now time.Time) bool {
	if sc.ConsecutiveFailures < scheduleSuppressAfter {
		return false
	}
	if probeInterval <= 0 {
		probeInterval = DefaultScheduleProbeInterval
	}
	if sc.LastFiredAt.IsZero() {
		// A streak with nothing ever fired is not reachable through
		// FireSchedule (the streak is built from runs its own fires minted),
		// but a probe is the safe answer to an impossible state: it produces
		// evidence rather than an indefinite hold.
		return false
	}
	return now.Sub(sc.LastFiredAt) < probeInterval
}
