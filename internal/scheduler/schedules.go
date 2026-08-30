package scheduler

import (
	"context"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The schedule half of the tick (issue #107, task t33).
//
// This lives in the scheduler package rather than in a new one because the
// package already owns exactly this responsibility -- "the control plane does
// something because a clock said so" -- and already solves the two hard parts
// of it: a single active instance elected by a PostgreSQL advisory lock, and
// a restart story that falls out of PostgreSQL's own guarantees rather than
// out of application bookkeeping (see the package doc comment). A second
// timer loop beside this one would have to re-derive both, and would have to
// contend with this one for the right to be the singleton.
//
// What a schedule fire does NOT share with a timer fire is the firing
// mechanism, and that is deliberate. A timer's effect is applied by this
// package (applyEffect, in scheduler.go). A schedule's effect is one
// Store.FireSchedule call, because the effect -- append a signal event, let
// its triggers create runs, advance the cursor -- has to be ONE transaction
// with the store's own delivery code, and that transaction is the store's to
// own. This file is the loop; internal/store/postgres/schedules.go is the
// durability.

// fireDueSchedules is the schedule half of one tick: list the enabled
// schedules whose declared instant has arrived, and try to fire each.
//
// Per-schedule failures do not abort the batch and do not fail the tick, for
// the same reason fireOne's do not (see its doc comment): a schedule that
// could not fire did not advance its cursor, so it is still due and the next
// tick retries it. Returning an error here is reserved for the read itself
// failing, which is an infrastructure fault the caller surfaces through
// Health.
func (sch *Scheduler) fireDueSchedules(ctx context.Context) error {
	due, err := sch.db.DueSchedules(ctx, sch.now(), sch.opts.batchSize())
	if err != nil {
		return fmt.Errorf("scheduler: tick: DueSchedules: %w", err)
	}
	for _, d := range due {
		// A schedule fires an event, and an event's whole point is that a
		// published workflow's trigger may turn it into a run (task t17b).
		// That needs the namespace's engine, so a namespace whose engine
		// cannot be built is skipped rather than allowed to fire an event
		// that would silently start nothing.
		eng, err := sch.engineFor(d.NamespaceID)
		if err != nil {
			continue
		}
		_, _ = sch.db.FireSchedule(ctx, postgres.FireScheduleInput{
			ScheduleID: d.ID,
			Now:        sch.now(),
			Pickup:     eng,
			Trigger:    eng,
			// Task t9 (issue #253): the fire itself decides whether this
			// occurrence is worth minting, because that decision needs the
			// schedule row under the same lock the cadence advance takes. The
			// loop's only job is to carry the deployment's policy in.
			ProbeInterval: sch.opts.ScheduleProbeInterval,
			AlertAfter:    sch.opts.ScheduleFailureAlertAfter,
			BeforeCommit:  sch.opts.Hooks.BeforeScheduleCommit,
		})
	}
	return nil
}
