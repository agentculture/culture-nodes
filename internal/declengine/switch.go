// switch.go: the global, per-namespace before/shadow/after engine switch
// (spec c80, ADR 0014 "Consequences", honesty h53) and the shadow dispatch
// gate that keeps it true. It does not wire the declaration engine into
// event delivery or gate the graph engine (internal/engine) directly --
// that is t17's drain hook and t18's freeze/replay -- it only builds the
// switch primitive and the dispatch gate both attach to. Task t38 wires the
// engine into delivery and the switch into an API route (deliver.go,
// internal/api/declswitch.go).
package declengine

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Engine switch modes (c80). The switch is global per namespace: every
// namespace has exactly one current mode.
const (
	ModeBefore = "before"
	ModeShadow = "shadow"
	ModeAfter  = "after"
)

// ValidMode reports whether m is one of the three switch values.
func ValidMode(m string) bool {
	switch m {
	case ModeBefore, ModeShadow, ModeAfter:
		return true
	}
	return false
}

// FlipHook runs inside the same transaction that records a flip, before it
// commits (h53: "switching to 'after' stops the graph engine acting for
// that scope in the same transaction"). No hook is registered by this
// task; t17 (drain) and t18 (freeze/replay) are the intended callers --
// t17 stopping the graph engine from starting new runs for the scope, t18
// freezing open declaration nodes on a flip back to 'before'.
type FlipHook func(ctx context.Context, tx pgx.Tx, namespaceID, previous, next string) error

// SwitchStore persists and reads the global per-namespace engine switch.
// Every Flip is recorded; nothing is ever overwritten (see
// migrations/0061_engine_switch.sql's immutable history table). "Every
// flip is recorded" is therefore true by construction: there is no
// separate mutable "current state" row that a flip could update without
// also appending to history.
type SwitchStore interface {
	// Mode returns the namespace's current switch value, defaulting to
	// ModeBefore when it has never flipped.
	Mode(ctx context.Context, namespaceID string) (string, error)
	// Flip records a transition to mode, attributed to actor (never
	// empty: a flip is a human or deterministic-migration decision, never
	// an agent's own completion claim -- PRD §10.4), and returns the mode
	// it replaced. Every hook runs inside the same transaction as the
	// recorded flip, before it commits; a hook error aborts the flip.
	Flip(ctx context.Context, namespaceID, mode, actor, reason string, hooks ...FlipHook) (previous string, err error)
}

// PostgresSwitchStore adapts the 0061 engine_switch_history table.
type PostgresSwitchStore struct{ Store *postgres.Store }

// Mode reads the newest history row for namespaceID, or ModeBefore when
// the namespace has never flipped.
func (s PostgresSwitchStore) Mode(ctx context.Context, namespaceID string) (string, error) {
	if namespaceID == "" {
		return "", errors.New("declengine: namespace id required")
	}
	var mode string
	err := s.Store.Pool().QueryRow(ctx,
		`SELECT mode FROM engine_switch_history WHERE namespace_id=$1 ORDER BY seq DESC LIMIT 1`, namespaceID).Scan(&mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return ModeBefore, nil
	}
	if err != nil {
		return "", err
	}
	return mode, nil
}

// Flip appends one engine_switch_history row inside a transaction that
// also runs every hook, so a future gate (t17/t18) can make its own
// namespace-scoped change atomic with the flip that authorizes it. It holds
// the namespace's exclusive switch lock (switchlock.go), so it waits for
// every evaluation already acting on the old mode; a caller that must also
// replay under that lock uses FlipSwitch.
func (s PostgresSwitchStore) Flip(ctx context.Context, namespaceID, mode, actor, reason string, hooks ...FlipHook) (string, error) {
	if namespaceID == "" || actor == "" {
		return "", errors.New("declengine: namespace id and actor required")
	}
	if !ValidMode(mode) {
		return "", fmt.Errorf("declengine: invalid engine switch mode %q", mode)
	}
	var previous string
	err := s.HoldExclusive(ctx, namespaceID, func(ctx context.Context) error {
		var err error
		previous, err = s.flip(ctx, namespaceID, mode, actor, reason, hooks)
		return err
	})
	return previous, err
}

func (s PostgresSwitchStore) flip(ctx context.Context, namespaceID, mode, actor, reason string, hooks []FlipHook) (string, error) {
	tx, err := s.Store.Pool().Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var previous string
	err = tx.QueryRow(ctx,
		`SELECT mode FROM engine_switch_history WHERE namespace_id=$1 ORDER BY seq DESC LIMIT 1 FOR UPDATE`, namespaceID).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		previous = ModeBefore
	} else if err != nil {
		return "", err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO engine_switch_history(id,namespace_id,mode,actor,reason) VALUES($1,$2,$3,$4,NULLIF($5,''))`,
		store.NewULID(), namespaceID, mode, actor, reason); err != nil {
		return "", err
	}

	for _, hook := range hooks {
		if hook == nil {
			continue
		}
		if err := hook(ctx, tx, namespaceID, previous, mode); err != nil {
			return "", err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return previous, nil
}

// GraphRunLookup finds the graph run that handled a signal event. Shadow
// lineage derives from it instead of an origin marker (c80): shadow
// dispatches nothing, so ShadowGate never mints a marker for
// MarkerService.Resolve to verify later.
type GraphRunLookup interface {
	// RunForEvent returns the graph run's id, or "" when no graph run has
	// (yet) consumed eventID -- not an error: the mapping is opportunistic.
	RunForEvent(ctx context.Context, namespaceID, eventID string) (string, error)
}

// PostgresGraphRunLookup reads runs.trigger_event_id (migration 0043),
// the same column the graph engine's own CreateRun sets from a trigger
// event (internal/engine/trigger.go) and WorkerDispatcher's real firings
// piggyback on. It is read-only: this task does not change how, or
// whether, the graph engine picks or names a run.
type PostgresGraphRunLookup struct{ Store *postgres.Store }

func (g PostgresGraphRunLookup) RunForEvent(ctx context.Context, namespaceID, eventID string) (string, error) {
	if namespaceID == "" || eventID == "" {
		return "", nil
	}
	var runID string
	err := g.Store.Pool().QueryRow(ctx,
		`SELECT id FROM runs WHERE namespace_id=$1 AND trigger_event_id=$2 ORDER BY created_at,id LIMIT 1`,
		namespaceID, eventID).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return runID, err
}

// ShadowLineageStore links a would-fire firing to the graph run that
// handled the same event ("graph run id -> shadow lineage", c80), and
// looks that link up by graph run so a later event continuing the same
// causal chain can resolve shadow lineage through the graph engine's own
// run identity. This is the hook t17 (drain) and t18 (freeze/replay)
// attach to; neither is implemented here.
type ShadowLineageStore interface {
	RecordShadowLineage(ctx context.Context, namespaceID, firingID, graphRunID string) error
	// GraphRunFiring returns the would-fire firing already recorded
	// against graphRunID, or "" when none has fired yet.
	GraphRunFiring(ctx context.Context, namespaceID, graphRunID string) (string, error)
}

// PostgresShadowLineageStore adapts the 0061 declaration_shadow_lineage
// table.
type PostgresShadowLineageStore struct{ Store *postgres.Store }

func (s PostgresShadowLineageStore) RecordShadowLineage(ctx context.Context, namespaceID, firingID, graphRunID string) error {
	if namespaceID == "" || firingID == "" || graphRunID == "" {
		return errors.New("declengine: namespace id, firing id and graph run id required")
	}
	_, err := s.Store.Pool().Exec(ctx,
		`INSERT INTO declaration_shadow_lineage(id,namespace_id,firing_id,graph_run_id) VALUES($1,$2,$3,$4)
 ON CONFLICT (namespace_id,firing_id) DO NOTHING`,
		store.NewULID(), namespaceID, firingID, graphRunID)
	return err
}

func (s PostgresShadowLineageStore) GraphRunFiring(ctx context.Context, namespaceID, graphRunID string) (string, error) {
	if namespaceID == "" || graphRunID == "" {
		return "", nil
	}
	var firingID string
	err := s.Store.Pool().QueryRow(ctx,
		`SELECT firing_id FROM declaration_shadow_lineage WHERE namespace_id=$1 AND graph_run_id=$2 ORDER BY created_at LIMIT 1`,
		namespaceID, graphRunID).Scan(&firingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return firingID, err
}

// ShadowGate wraps a Dispatcher with the global engine switch (h53). In
// 'after' it delegates to Underlying unchanged. In 'before' or 'shadow' it
// never calls Underlying at all, so the declaration engine dispatches
// zero actions through it -- no worker run, no actor invocation, no
// ledger record for the action itself.
//
// Because Claim (engine.go's evaluate) has already created the firing's
// declaration_firings row, and already recorded the matched / lineage
// checked / condition true / dispatching evaluation steps, before
// Dispatch is ever called, that row and its evaluation trail already ARE
// spec c80's 'would fire' record. ShadowGate's own job is the half
// evaluate() cannot do on its own: recording which graph run handled the
// same event, so a later firing that continues this chain can derive its
// lineage from the graph engine's real run instead of a marker shadow
// never stamps.
type ShadowGate struct {
	Switch     SwitchStore
	Underlying Dispatcher
	GraphRuns  GraphRunLookup
	ShadowLine ShadowLineageStore
}

// Dispatch never fails solely because no graph run has (yet) handled this
// event: the shadow-lineage mapping is opportunistic, and its absence is
// not this firing's failure.
func (g ShadowGate) Dispatch(ctx context.Context, r DispatchRequest) (DispatchResult, error) {
	if g.Switch == nil {
		return DispatchResult{}, errors.New("declengine: shadow gate needs a switch store")
	}
	mode, err := g.Switch.Mode(ctx, r.Firing.NamespaceID)
	if err != nil {
		return DispatchResult{}, err
	}
	if mode == ModeAfter {
		if g.Underlying == nil {
			return DispatchResult{}, errors.New("declengine: shadow gate needs an underlying dispatcher for 'after'")
		}
		return g.Underlying.Dispatch(ctx, r)
	}
	// before or shadow: dispatch zero actions (h53). Shadowed:true tells
	// evaluate() (engine.go) to record this firing's terminal outcome as
	// OutcomeShadow rather than OutcomeFired (t13, spec c88) -- the
	// declaration_evaluations trail must say a firing never really acted,
	// not just that it claimed and opened a landing node.
	if g.GraphRuns != nil && g.ShadowLine != nil {
		runID, err := g.GraphRuns.RunForEvent(ctx, r.Firing.NamespaceID, r.Firing.EventID)
		if err != nil {
			return DispatchResult{}, err
		}
		if runID != "" {
			if err := g.ShadowLine.RecordShadowLineage(ctx, r.Firing.NamespaceID, r.Firing.ID, runID); err != nil {
				return DispatchResult{}, err
			}
		}
	}
	return DispatchResult{Shadowed: true}, nil
}

// PrepareOrigin makes ShadowGate satisfy the optional interface engine.go's
// Handle checks for (dispatch.go's WorkerDispatcher.PrepareOrigin), so
// wrapping a WorkerDispatcher in a ShadowGate does not silently drop its
// reaction-binding step once the switch reaches 'after'.
func (g ShadowGate) PrepareOrigin(ctx context.Context, origin OriginEvent, markers *MarkerService) error {
	preparer, ok := g.Underlying.(interface {
		PrepareOrigin(context.Context, OriginEvent, *MarkerService) error
	})
	if !ok {
		return nil
	}
	mode, err := g.Switch.Mode(ctx, origin.NamespaceID)
	if err != nil {
		return err
	}
	if mode != ModeAfter {
		// No marker was ever minted for a shadow firing (Dispatch never
		// calls Underlying), so there is nothing for the underlying
		// dispatcher's own PrepareOrigin to reconcile yet.
		return nil
	}
	return preparer.PrepareOrigin(ctx, origin, markers)
}
