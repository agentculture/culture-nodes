// drain.go: the declaration-side half of task t17 (#328, spec c94, ADR
// 0014 "Consequences", honesty h63) -- "flipping to 'after' drains, it
// does not strand". From the flip onward, new trigger events never start
// graph runs; they go to the declaration engine instead (that half is
// already wired -- see engine.go's Handle and dispatch.go). A graph run
// ALREADY open when the flip lands keeps running on the graph engine
// until it ends, including every reaction it is waiting for (callbacks,
// waits, approvals): nothing here freezes, cancels, or reroutes it. The
// graph engine retires for a namespace only once its open-run count
// reaches zero, and that count is what this file, plus
// internal/api/drain.go's read-only route, makes visible.
//
// DrainGate is the graph-engine side of the hook: it implements
// internal/engine.NewRunGate so internal/engine/trigger.go's two
// brand-new-run call sites (the direct trigger match, and
// DrainSubjectTriggerQueue's queue drain) can ask, in one place, "may a
// new graph run start for this namespace right now" without internal/engine
// importing this package. The answer is simply "not in 'after'" --
// before and shadow behave exactly as they did before this task existed,
// which is what keeps this hook from changing anything about those two
// modes.
package declengine

import (
	"context"
	"errors"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// DrainGate adapts a SwitchStore to internal/engine.NewRunGate. A
// namespace in 'before' or 'shadow' allows every new-run request, byte for
// byte the pre-t17 behavior; only 'after' refuses.
type DrainGate struct {
	Switch SwitchStore
}

// AllowNewRun implements engine.NewRunGate.
func (g DrainGate) AllowNewRun(ctx context.Context, namespaceID string) (bool, error) {
	if g.Switch == nil {
		return false, errors.New("declengine: drain gate needs a switch store")
	}
	mode, err := g.Switch.Mode(ctx, namespaceID)
	if err != nil {
		return false, err
	}
	return mode != ModeAfter, nil
}

// OpenRunCounter answers "how many graph runs are still open" for a
// namespace -- the fact c94 requires be visible, and the one condition the
// graph engine's own retirement for a namespace waits on (this task does
// not implement retirement itself; it makes the count that decision reads
// real and queryable).
type OpenRunCounter interface {
	// OpenGraphRunCount counts runs in namespaceID whose state is not yet
	// terminal (not completed, failed, or cancelled) -- created, running,
	// and waiting all count as open, because every one of them is a run
	// the graph engine is still acting for.
	OpenGraphRunCount(ctx context.Context, namespaceID string) (int, error)
}

// PostgresOpenRunCounter reads the runs table directly. It is read-only
// and namespace-scoped like every other query in this package.
type PostgresOpenRunCounter struct{ Store *postgres.Store }

// OpenGraphRunCount implements OpenRunCounter.
//
// "Open" is read off runs.status rather than kept as a separate counter:
// the graph engine already writes that column on every transition
// (internal/engine's RunState -- migrations/0002_runtime_execution.sql
// names the column `status`, and engine_store.go stores RunState's own
// string values there verbatim), so this can never drift from what the
// graph engine itself believes about a run the way an independently
// incremented counter could.
func (c PostgresOpenRunCounter) OpenGraphRunCount(ctx context.Context, namespaceID string) (int, error) {
	if namespaceID == "" {
		return 0, errors.New("declengine: namespace id required")
	}
	var n int
	err := c.Store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM runs WHERE namespace_id = $1 AND status NOT IN ('completed','failed','cancelled')`,
		namespaceID).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}
