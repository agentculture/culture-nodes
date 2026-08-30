package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
)

// Schedules: the declared cadence that makes upkeep run itself (issue #107,
// task t33). See migrations/0033_schedules.sql for why a schedule is a table
// rather than a field on an immutable workflow, and why it EMITS AN EVENT
// rather than creating a run.
//
// The durability argument in one sentence: a fire advances the schedule's
// cursor in the same transaction that appends the signal event and creates
// whatever runs its triggers matched, so there is no state in which a
// control plane has to remember what it was in the middle of. Everything a
// restart needs to know is next_fire_at, and next_fire_at only ever moves as
// part of a committed fire.

// ScheduleCatchUp is what to do about an occurrence that came due while
// nothing was running -- the question a cron line never answers, because
// cron has no memory of the tick it missed.
//
// Both answers are defensible and which one is right depends entirely on
// what the schedule does, so it is declared per schedule rather than chosen
// globally:
//
//   - CatchUpFireOnce (the default): fire late, exactly once, then realign to
//     the next declared boundary. Right for a poll -- an upkeep sweep that
//     was missed for six hours still wants to sweep, and it wants to sweep
//     once, not six times. This deliberately does NOT backfill: six queued
//     runs would each carry the same freshly-read findings, and the
//     thundering herd would be the schedule's fault rather than the work's.
//   - CatchUpSkip: decline a MISSED occurrence and wait for the next
//     boundary. Right for anything whose value is tied to when it happens --
//     a nightly report has no business running at noon. A skip is counted
//     (schedules.skip_count) and emitted to the outbox, because "chose not to
//     run" and "never came due" must not be indistinguishable afterwards.
//
// The two differ ONLY for a missed occurrence. An occurrence that is merely
// LATE -- overdue by less than one whole interval, which is what a tick
// interval and a busy loop produce normally -- fires under both policies.
type ScheduleCatchUp string

const (
	CatchUpFireOnce ScheduleCatchUp = "fire-once"
	CatchUpSkip     ScheduleCatchUp = "skip"
)

func (c ScheduleCatchUp) valid() bool {
	return c == CatchUpFireOnce || c == CatchUpSkip
}

// topicScheduleSkipped is the durable trace a declined occurrence leaves.
// It rides the outbox, the same channel every other state change in this
// store announces itself on.
const topicScheduleSkipped = "dev.culture.nodes.schedule.skipped"

// Schedule is one declared cadence.
type Schedule struct {
	ID          string
	NamespaceID string
	Name        string
	EventName   string
	Emitter     string
	Payload     json.RawMessage
	Interval    time.Duration
	CatchUp     ScheduleCatchUp
	Enabled     bool
	NextFireAt  time.Time
	LastFiredAt time.Time
	LastEventID string
	FireCount   int64
	SkipCount   int64
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// The failure-backoff state added by migration 0050 (task t9, issue
	// #253). See schedulebackoff.go for what maintains it and why.

	// SuppressedCount is how many ticks this schedule declined to mint on
	// because its last two runs failed identically. Cumulative, like
	// SkipCount: it is history, and a repair does not un-hold those ticks.
	SuppressedCount int64
	// ConsecutiveFailures is the current streak of minted runs that failed
	// carrying LastFailureDetail. Zero whenever the last assessed run
	// completed.
	ConsecutiveFailures int64
	// LastFailureDetail is the repeated reason, verbatim: the last minted
	// run's last attempt's result.error.detail. Empty when the streak is
	// zero.
	LastFailureDetail string
	// LastAssessedRunID is the minted run whose outcome the two fields above
	// already reflect, so one run contributes to the streak exactly once.
	LastAssessedRunID string
	// FailureTaskID names the schedule_failing human task this schedule last
	// raised, pending or decided. Whether it is still PENDING is what decides
	// re-raising, so this is a handle rather than a flag.
	FailureTaskID string
}

// CreateScheduleInput declares a cadence. Everything in it is data an
// operator wrote down: a schedule discovers nothing, reads no external
// system, and holds no credential -- it emits the event it was told to emit,
// when it was told to emit it.
type CreateScheduleInput struct {
	NamespaceID string
	Name        string
	EventName   string
	Emitter     string
	Payload     json.RawMessage
	Interval    time.Duration
	// CatchUp defaults to CatchUpFireOnce when empty.
	CatchUp ScheduleCatchUp
	// Disabled creates the schedule already paused. The field is negative so
	// the zero value creates an ENABLED schedule: an operator who declared a
	// cadence and did not say otherwise meant it to run.
	Disabled bool
	// FirstFireAt is the first instant this schedule is due, and thereafter
	// the phase every subsequent occurrence is aligned to. Zero means "one
	// interval from now" -- a schedule created at 09:14 with a 1h interval
	// first fires at 10:14, not immediately, because creating a schedule is
	// not the same act as starting a run.
	FirstFireAt time.Time
}

const scheduleColumns = `id, namespace_id, name, event_name, emitter, payload,
	interval_seconds, catch_up, enabled, next_fire_at, last_fired_at,
	last_event_id, fire_count, skip_count, created_at, updated_at,
	suppressed_count, consecutive_failures, last_failure_detail,
	last_assessed_run_id, failure_task_id`

func scanSchedule(row pgx.Row) (Schedule, error) {
	var (
		sc            Schedule
		payload       []byte
		seconds       int64
		catchUp       string
		nextFire      pgtype.Timestamptz
		lastFired     pgtype.Timestamptz
		lastEventID   pgtype.Text
		createdAt     pgtype.Timestamptz
		updatedAt     pgtype.Timestamptz
		failureDetail pgtype.Text
		assessedRun   pgtype.Text
		failureTask   pgtype.Text
	)
	if err := row.Scan(&sc.ID, &sc.NamespaceID, &sc.Name, &sc.EventName, &sc.Emitter, &payload,
		&seconds, &catchUp, &sc.Enabled, &nextFire, &lastFired, &lastEventID,
		&sc.FireCount, &sc.SkipCount, &createdAt, &updatedAt,
		&sc.SuppressedCount, &sc.ConsecutiveFailures, &failureDetail,
		&assessedRun, &failureTask); err != nil {
		return Schedule{}, err
	}
	sc.LastFailureDetail = textOrEmpty(failureDetail)
	sc.LastAssessedRunID = textOrEmpty(assessedRun)
	sc.FailureTaskID = textOrEmpty(failureTask)
	sc.Payload = jsonOrEmptyObject(payload)
	sc.Interval = time.Duration(seconds) * time.Second
	sc.CatchUp = ScheduleCatchUp(catchUp)
	sc.NextFireAt = tsValue(nextFire)
	sc.LastFiredAt = tsValue(lastFired)
	sc.LastEventID = textOrEmpty(lastEventID)
	sc.CreatedAt = tsValue(createdAt)
	sc.UpdatedAt = tsValue(updatedAt)
	return sc, nil
}

// CreateSchedule declares a cadence. It refuses a declaration it could not
// honour rather than storing one that would misbehave later: a zero or
// negative interval has no next occurrence, and an unknown catch-up policy
// has no defined answer for a missed one.
func (s *Store) CreateSchedule(ctx context.Context, in CreateScheduleInput) (Schedule, error) {
	if in.CatchUp == "" {
		in.CatchUp = CatchUpFireOnce
	}
	switch {
	case in.NamespaceID == "":
		return Schedule{}, errors.New("postgres: CreateSchedule: namespaceID is required")
	case in.Name == "":
		return Schedule{}, errors.New("postgres: CreateSchedule: name is required")
	case in.EventName == "":
		return Schedule{}, errors.New("postgres: CreateSchedule: eventName is required")
	case in.Interval <= 0:
		return Schedule{}, errors.New("postgres: CreateSchedule: interval must be positive")
	case in.Interval%time.Second != 0:
		// Stored in whole seconds (migration 0033). Truncating silently would
		// make a 1500ms cadence quietly become 1s, which is a schedule that
		// does not do what its declaration says.
		return Schedule{}, errors.New("postgres: CreateSchedule: interval must be a whole number of seconds")
	case !in.CatchUp.valid():
		return Schedule{}, fmt.Errorf("postgres: CreateSchedule: unknown catch-up policy %q (want %q or %q)",
			in.CatchUp, CatchUpFireOnce, CatchUpSkip)
	}
	if in.Emitter == "" {
		// Attribution for operators, never an authority claim (PRD §10.4):
		// a schedule-emitted fact says a schedule emitted it.
		in.Emitter = "schedule:" + in.Name
	}
	first := in.FirstFireAt
	if first.IsZero() {
		first = time.Now().UTC().Add(in.Interval)
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO schedules (id, namespace_id, name, event_name, emitter, payload,
			interval_seconds, catch_up, enabled, next_fire_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+scheduleColumns,
		store.NewULID(), in.NamespaceID, in.Name, in.EventName, in.Emitter,
		jsonOrEmptyObject(in.Payload), int64(in.Interval/time.Second), string(in.CatchUp),
		!in.Disabled, first.UTC())
	sc, err := scanSchedule(row)
	if err != nil {
		return Schedule{}, fmt.Errorf("postgres: CreateSchedule: %w", err)
	}
	return sc, nil
}

// Schedule returns one schedule by id, scoped to a namespace.
func (s *Store) Schedule(ctx context.Context, namespaceID, id string) (Schedule, error) {
	sc, err := scanSchedule(s.pool.QueryRow(ctx,
		`SELECT `+scheduleColumns+` FROM schedules WHERE id = $1 AND namespace_id = $2`, id, namespaceID))
	if err != nil {
		if isNoRows(err) {
			return Schedule{}, fmt.Errorf("postgres: schedule %s: %w", id, ErrNotFound)
		}
		return Schedule{}, fmt.Errorf("postgres: Schedule: %w", err)
	}
	return sc, nil
}

// ListSchedules returns every schedule in a namespace, enabled or not: a
// disabled schedule is precisely the thing an operator is looking for when
// they ask why nothing has run.
func (s *Store) ListSchedules(ctx context.Context, namespaceID string) ([]Schedule, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+scheduleColumns+` FROM schedules WHERE namespace_id = $1 ORDER BY name`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("postgres: ListSchedules: %w", err)
	}
	defer rows.Close()
	out := make([]Schedule, 0, 8)
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: ListSchedules: %w", err)
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: ListSchedules: %w", err)
	}
	return out, nil
}

// SetScheduleEnabled pauses or resumes a schedule.
//
// Disabling deliberately does NOT move next_fire_at. A schedule disabled at
// 09:00 and re-enabled at 17:00 is overdue on re-enable, and its declared
// catch-up policy is what decides whether that overdue occurrence fires --
// the same question, with the same answer, as a control plane that was down
// for those eight hours. Resetting the cursor on disable would quietly make
// "pause" mean "pause and forget", which is a different thing and not the one
// an operator asked for.
func (s *Store) SetScheduleEnabled(ctx context.Context, namespaceID, id string, enabled bool) (Schedule, error) {
	sc, err := scanSchedule(s.pool.QueryRow(ctx,
		`UPDATE schedules SET enabled = $3, updated_at = now()
		 WHERE id = $1 AND namespace_id = $2
		 RETURNING `+scheduleColumns, id, namespaceID, enabled))
	if err != nil {
		if isNoRows(err) {
			return Schedule{}, fmt.Errorf("postgres: schedule %s: %w", id, ErrNotFound)
		}
		return Schedule{}, fmt.Errorf("postgres: SetScheduleEnabled: %w", err)
	}
	return sc, nil
}

// DeleteSchedule removes a schedule. The signal_events facts it appended and
// the runs those started are untouched: they are history, and history does
// not stop being true because the thing that produced it was retired.
func (s *Store) DeleteSchedule(ctx context.Context, namespaceID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM schedules WHERE id = $1 AND namespace_id = $2`, id, namespaceID)
	if err != nil {
		return fmt.Errorf("postgres: DeleteSchedule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: schedule %s: %w", id, ErrNotFound)
	}
	return nil
}

// DueSchedule names one schedule a tick should try to fire. It carries the
// namespace because the tick is namespace-agnostic (exactly like
// ClaimDueTimers) while the engine it needs to run triggers through is not.
type DueSchedule struct {
	ID          string
	NamespaceID string
}

// DueSchedules lists enabled schedules whose next occurrence is at or before
// now, oldest first. It claims nothing and locks nothing: the authoritative
// due check is re-run inside FireSchedule's own row lock, so this read is
// only a candidate list. Making it a claim would create the very window this
// design avoids -- a claimed-but-unfired schedule that a crash leaves
// unreachable until something expires it.
func (s *Store) DueSchedules(ctx context.Context, now time.Time, limit int) ([]DueSchedule, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, namespace_id FROM schedules
		WHERE enabled AND next_fire_at <= $1
		ORDER BY next_fire_at
		LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: DueSchedules: %w", err)
	}
	defer rows.Close()
	var out []DueSchedule
	for rows.Next() {
		var d DueSchedule
		if err := rows.Scan(&d.ID, &d.NamespaceID); err != nil {
			return nil, fmt.Errorf("postgres: DueSchedules: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: DueSchedules: %w", err)
	}
	return out, nil
}

// FireScheduleInput fires one occurrence of one schedule.
type FireScheduleInput struct {
	ScheduleID string
	// Now is the instant this fire is happening at, passed in rather than
	// read from the clock. Production passes time.Now; tests pass whatever
	// instant they are asserting about, which is why not one schedule test in
	// this repo sleeps.
	Now time.Time
	// Pickup and Trigger are the engine slices delivery runs the fact
	// through -- the same two DeliverSignalEventInput takes. Trigger is what
	// turns this fire into a run, and passing nil means the fact is appended
	// and starts nothing, which is a legitimate configuration (a schedule
	// whose only consumers are parked signal waits).
	Pickup  engine.EventPickupRunner
	Trigger engine.EventTriggerRunner
	// ProbeInterval is the floor between mints while this schedule is
	// suppressed (NODES_SCHEDULE_PROBE_INTERVAL). Zero selects
	// DefaultScheduleProbeInterval. See schedulebackoff.go.
	ProbeInterval time.Duration
	// AlertAfter is the consecutive-identical-failure count at which one
	// pending schedule_failing human task is raised
	// (NODES_SWEEP_FAILURE_ALERT_AFTER). Zero selects
	// DefaultSweepFailureAlertAfter; negative disables the alert.
	AlertAfter int
	// BeforeCommit is a test seam: it runs inside the still-open transaction,
	// after the event was appended and the cursor advanced, before commit.
	// Returning an error rolls the whole thing back -- which is exactly the
	// state a process killed at that instant would leave behind, without
	// needing to kill a real process to get there. Production callers never
	// set it.
	BeforeCommit func(Schedule) error
}

// ScheduleFireResult is what one attempted fire did.
type ScheduleFireResult struct {
	// Schedule is the row as it stands after this attempt.
	Schedule Schedule
	// Fired is true only when an event was appended and committed.
	Fired bool
	// Skipped is true when a MISSED occurrence was declined under
	// CatchUpSkip. Fired and Skipped are never both true, and both being
	// false means the schedule was not due, not enabled, or was being fired
	// by someone else at that moment.
	Skipped bool
	// Suppressed is true when a DUE occurrence was declined because this
	// schedule's last two minted runs failed with the same reason and the
	// probe interval has not elapsed (task t9, issue #253). It is mutually
	// exclusive with Fired and Skipped, and it advances the cursor exactly as
	// a fire does -- a suppressed occurrence is spent, not deferred.
	Suppressed bool
	// Missed is how many whole intervals had elapsed past the due instant --
	// 0 for an on-time or merely late fire.
	Missed int64
	// Delivery is the appended fact and everything it started. Zero when
	// nothing fired.
	Delivery SignalDelivery
	// AlertTaskID names the schedule_failing human task this attempt raised,
	// empty when it raised none (the usual case: one is already pending, the
	// streak is short, or nothing failed).
	AlertTaskID string
}

// FireSchedule fires one occurrence, if one is due, in ONE transaction.
//
// This is where acceptance criterion 2 is actually met, so the ordering is
// the point rather than an implementation detail:
//
//  1. Lock the row with FOR UPDATE SKIP LOCKED. A second control plane
//     ticking at the same instant gets no row and returns immediately --
//     it does not block, and it does not wait to discover it lost.
//  2. Re-read enabled and next_fire_at UNDER that lock. This is what catches
//     the other ordering: a second ticker that arrives after the winner has
//     committed acquires the lock cleanly and finds the schedule no longer
//     due. Neither guard is redundant; they cover opposite interleavings.
//  3. Append the event, run its triggers (creating runs), and advance the
//     cursor -- all still inside the same transaction.
//  4. Commit.
//
// A crash anywhere before step 4 leaves next_fire_at where it was, so the
// occurrence is still due and the next tick retries it in full. A crash after
// step 4 leaves the cursor advanced, so nothing re-fires. The process never
// has to know which side of the commit it died on, because it never asks --
// it reads next_fire_at, which is the answer either way.
func (s *Store) FireSchedule(ctx context.Context, in FireScheduleInput) (ScheduleFireResult, error) {
	if in.ScheduleID == "" {
		return ScheduleFireResult{}, errors.New("postgres: FireSchedule: scheduleID is required")
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ScheduleFireResult{}, fmt.Errorf("postgres: FireSchedule: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	sc, err := scanSchedule(tx.QueryRow(ctx,
		`SELECT `+scheduleColumns+` FROM schedules WHERE id = $1 FOR UPDATE SKIP LOCKED`, in.ScheduleID))
	if err != nil {
		if isNoRows(err) {
			// Either the row is locked by another fire of the same occurrence
			// (step 1 above), or the schedule was deleted. Both are "nothing
			// for this caller to do", and neither is a fault.
			return ScheduleFireResult{}, nil
		}
		return ScheduleFireResult{}, fmt.Errorf("postgres: FireSchedule: lock schedule %s: %w", in.ScheduleID, err)
	}
	if !sc.Enabled || sc.NextFireAt.After(now) {
		return ScheduleFireResult{Schedule: sc}, nil
	}

	missed := int64(now.Sub(sc.NextFireAt) / sc.Interval)
	next := advanceCadence(sc.NextFireAt, sc.Interval, now)

	// Task t9 (issue #253). Read what the LAST occurrence produced before
	// deciding whether to produce another one, and record the answer, so the
	// same terminal run is never counted twice. Both of the decisions below
	// -- hold this tick back, ask a human -- read only the state this call
	// writes, still inside the same transaction and the same row lock the
	// cadence advance uses.
	alertTaskID, err := s.assessScheduleOutcomeTx(ctx, tx, &sc, in.AlertAfter, now)
	if err != nil {
		return ScheduleFireResult{}, err
	}

	if scheduleIsSuppressed(sc, in.ProbeInterval, now) {
		suppressed, err := scanSchedule(tx.QueryRow(ctx, `
			UPDATE schedules
			SET next_fire_at = $2, suppressed_count = suppressed_count + 1, updated_at = now()
			WHERE id = $1
			RETURNING `+scheduleColumns, sc.ID, next))
		if err != nil {
			return ScheduleFireResult{}, fmt.Errorf("postgres: FireSchedule: suppress schedule %s: %w", sc.ID, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return ScheduleFireResult{}, fmt.Errorf("postgres: FireSchedule: commit suppression: %w", err)
		}
		return ScheduleFireResult{Schedule: suppressed, Suppressed: true, Missed: missed, AlertTaskID: alertTaskID}, nil
	}

	if sc.CatchUp == CatchUpSkip && missed > 0 {
		if err := s.skipOccurrenceTx(ctx, tx, sc, next, missed); err != nil {
			return ScheduleFireResult{}, err
		}
		sc.NextFireAt, sc.SkipCount, sc.UpdatedAt = next, sc.SkipCount+1, now
		if err := tx.Commit(ctx); err != nil {
			return ScheduleFireResult{}, fmt.Errorf("postgres: FireSchedule: commit skip: %w", err)
		}
		return ScheduleFireResult{Schedule: sc, Skipped: true, Missed: missed, AlertTaskID: alertTaskID}, nil
	}

	delivery, err := s.deliverSignalEventTx(ctx, tx, DeliverSignalEventInput{
		NamespaceID: sc.NamespaceID,
		Name:        sc.EventName,
		Payload:     sc.Payload,
		Emitter:     sc.Emitter,
		Pickup:      in.Pickup,
		Trigger:     in.Trigger,
	})
	if err != nil {
		return ScheduleFireResult{}, fmt.Errorf("postgres: FireSchedule: deliver %s: %w", sc.EventName, err)
	}

	fired, err := scanSchedule(tx.QueryRow(ctx, `
		UPDATE schedules
		SET next_fire_at = $2, last_fired_at = $3, last_event_id = $4,
		    fire_count = fire_count + 1, updated_at = now()
		WHERE id = $1
		RETURNING `+scheduleColumns, sc.ID, next, now, delivery.Event.ID))
	if err != nil {
		return ScheduleFireResult{}, fmt.Errorf("postgres: FireSchedule: advance schedule %s: %w", sc.ID, err)
	}

	if in.BeforeCommit != nil {
		if err := in.BeforeCommit(fired); err != nil {
			return ScheduleFireResult{}, fmt.Errorf("postgres: FireSchedule: BeforeCommit for schedule %s: %w", sc.ID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return ScheduleFireResult{}, fmt.Errorf("postgres: FireSchedule: commit: %w", err)
	}
	return ScheduleFireResult{Schedule: fired, Fired: true, Missed: missed, Delivery: delivery, AlertTaskID: alertTaskID}, nil
}

// skipOccurrenceTx realigns a CatchUpSkip schedule past a missed occurrence
// and records that it did. The outbox row is the whole reason this is not
// just an UPDATE: "the schedule declined to run" has to be readable
// afterwards, or a skip is indistinguishable from a schedule that silently
// stopped -- the exact failure acceptance criterion 2 names.
func (s *Store) skipOccurrenceTx(ctx context.Context, tx pgx.Tx, sc Schedule, next time.Time, missed int64) error {
	if _, err := tx.Exec(ctx, `
		UPDATE schedules SET next_fire_at = $2, skip_count = skip_count + 1, updated_at = now()
		WHERE id = $1`, sc.ID, next); err != nil {
		return fmt.Errorf("postgres: FireSchedule: realign skipped schedule %s: %w", sc.ID, err)
	}
	payload, err := json.Marshal(map[string]any{
		"schedule_id":   sc.ID,
		"schedule_name": sc.Name,
		"event_name":    sc.EventName,
		"missed":        missed,
		"catch_up":      string(sc.CatchUp),
		"next_fire_at":  next,
	})
	if err != nil {
		return fmt.Errorf("postgres: FireSchedule: marshal skip payload for schedule %s: %w", sc.ID, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox (id, namespace_id, topic, payload, status, available_at)
		 VALUES ($1, $2, $3, $4, 'pending', now())`,
		store.NewULID(), sc.NamespaceID, topicScheduleSkipped, payload); err != nil {
		return fmt.Errorf("postgres: FireSchedule: record skip for schedule %s: %w", sc.ID, err)
	}
	return nil
}

// advanceCadence returns the first occurrence strictly after now, keeping the
// phase the operator declared.
//
// It steps from the DUE instant rather than from now, so a schedule declared
// to fire on the hour still fires on the hour after an outage that ended at
// 05:30 -- the next occurrence is 06:00, not 06:30. A cadence that drifted by
// however late each tick happened to be would, over a long enough run, stop
// meaning what its declaration said.
func advanceCadence(due time.Time, interval time.Duration, now time.Time) time.Time {
	if interval <= 0 {
		// Unreachable through CreateSchedule (it refuses a non-positive
		// interval) and through the CHECK constraint on the column. Guarded
		// anyway because the alternative to this branch is a division by zero
		// in a loop that fires schedules.
		return now.Add(time.Second)
	}
	steps := now.Sub(due)/interval + 1
	return due.Add(time.Duration(steps) * interval)
}
