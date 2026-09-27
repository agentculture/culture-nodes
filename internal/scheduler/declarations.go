package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The declaration engine's periodic half (task t38, #328).
//
// It runs here, not in a loop of its own, for the reason schedules.go gives
// for schedules: this package already owns "the control plane does something
// because a clock said so", already elects exactly one active instance by
// advisory lock, and already reads one injectable clock (Options.Now). A
// second loop would re-derive both and race this one. The cadence is the
// tick's, and every call is bounded (internal/declengine.Driver's batch).

// DeclarationEngine is what the scheduler needs from the declaration engine
// (internal/declengine.Driver satisfies it; this package does not import
// declengine so the dependency stays one-way): a bounded periodic pass at a
// caller-supplied instant -- node deadlines, action.* results, and the
// re-runnable thaw-and-replay -- and the post-commit handler the schedule
// fires offer their events to.
type DeclarationEngine interface {
	postgres.DeliveredEventHandler
	Drive(ctx context.Context, now time.Time) error
}

// driveDeclarations is one declaration pass. Nil Options.Declarations -- every
// deployment that has not enabled the engine -- is a no-op.
func (sch *Scheduler) driveDeclarations(ctx context.Context) error {
	if sch.opts.Declarations == nil {
		return nil
	}
	if err := sch.opts.Declarations.Drive(ctx, sch.now()); err != nil {
		return fmt.Errorf("scheduler: tick: declarations: %w", err)
	}
	return nil
}

// declarationEvents is the handler FireSchedule offers a fired event to
// after its commit, or nil when the engine is not enabled.
func (sch *Scheduler) declarationEvents() postgres.DeliveredEventHandler {
	if sch.opts.Declarations == nil {
		return nil
	}
	return sch.opts.Declarations
}
