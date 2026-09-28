// Task t11 (#328, trigger-condition-action plan; spec "Budgets can be set
// at four levels: node, machine, declaration and alias. Any of them may
// carry a budget."): before a claimed firing is dispatched, EVERY budget
// that applies to it -- its landing node, the target machine (the rendered
// action's with.uses), the declaration itself, and every alias that
// directly contains it -- must have headroom, or the dispatch is blocked
// (docs/plans/2026-09-27-trigger-condition-action.md's open vagueness on
// precedence records the working assumption this file implements
// literally: "every applicable budget must have headroom"; an override
// order is explicitly left for the owner to decide later, not designed
// here).
//
// This reuses ADR 0011's declared-economic-budget semantics and units
// (docs/adr/0011-declared-economic-budget.md) unchanged rather than
// inventing a second economic vocabulary: max_sessions bounds cold-start
// dispatches, max_uncached_input bounds measured uncached input tokens, and
// an attempt that reported no cache telemetry is charged in full for the
// same honesty reason ADR 0011 §5 gives. What ADR 0011 built for ONE run's
// spec.budget, this widens to FOUR shared scopes a firing can belong to at
// once -- see migrations/0063_decl_budgets.sql for why that needs its own
// spend ledger rather than reusing migration 0023's run_sessions.
//
// WHERE THE CHECK SITS. Immediately after the firing is claimed and its
// marker minted, immediately before the dispatcher is invoked -- the exact
// position ADR 0011 §2 established for the graph engine's own budget check
// ("on CLAIMED work ... one step before the actor is invoked") and for the
// same reason: completing (or in this engine, blocking) an attempt needs
// the claim already held, and nothing external has been touched yet, so a
// blocked dispatch spends nothing.
//
// WHAT A BLOCK LOOKS LIKE. The firing's own evaluation stops at
// OutcomeBudgetBlocked -- a visible declaration_evaluations record, the same
// "nothing is silently dropped" rule every other guard in this package
// (guard.go, nodes.go) already keeps. Then, exactly like nodes.go's
// action.* triggers, the declaration engine emits action.budget_exhausted
// linked to the node the blocked dispatch would have landed on, and runs it
// back through Handle so any declaration reacting to that trigger can fire.
// Unlike action.failed and friends, no run and no actor invocation ever
// happened here -- the block is entirely a control-plane decision -- but the
// emitted trigger has the same shape so a declaration can route it the same
// way.
package declengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/store"
)

// BudgetScope is one of the four levels a budget can attach to.
type BudgetScope string

const (
	BudgetScopeNode        BudgetScope = "node"
	BudgetScopeMachine     BudgetScope = "machine"
	BudgetScopeDeclaration BudgetScope = "declaration"
	BudgetScopeAlias       BudgetScope = "alias"
)

// ActionTriggerBudgetExhausted is the trigger kind a blocked dispatch emits
// (internal/decl/kinds.Triggers), the fifth member of nodes.go's action.*
// family. It lives here rather than in nodes.go because, unlike the other
// four, it never comes from a run's terminal result -- it is emitted
// directly by the budget check below, not by EmitActionResults' scan.
const ActionTriggerBudgetExhausted = "action.budget_exhausted"

// OutcomeBudgetBlocked (engine.go, spec c88's "budget-blocked") is this
// firing's own evaluation outcome when a budget refused it. It is distinct
// from ActionTriggerBudgetExhausted: that name is what OTHER declarations
// react to; the outcome is the record on the blocked firing itself.

// Budget is one declared budget row, in ADR 0011's exact two units. Zero on
// a field means that axis is not bounded -- never "zero allowed", mirroring
// engine.Budget and enforced the same way at the schema layer (migration
// 0063's CHECK constraints refuse a stored zero or an empty declaration).
type Budget struct {
	Scope            BudgetScope
	Key              string
	MaxSessions      int
	MaxUncachedInput int64
}

// Declared reports whether this budget bounds anything. A row read from
// storage always does (the schema requires at least one axis); this exists
// for the same reason engine.Budget.Declared() does -- so a hand-built zero
// value in a test is inert rather than a surprise refusal.
func (b Budget) Declared() bool {
	return b.MaxSessions > 0 || b.MaxUncachedInput > 0
}

// BudgetSpend is what has already been spent against one budget, in the same
// two units ADR 0011's postgres.UncachedInput carries, including its
// coverage counts: an attempt that reported no cache telemetry is charged in
// full (the shortfall named, never hidden -- ADR 0011 §5), and an attempt
// that reported no usage at all burned tokens no ceiling here can see, so
// the measured total is always a floor, never a complete account.
type BudgetSpend struct {
	Sessions                      int
	UncachedInputTokens           int64
	AttemptsWithoutCacheTelemetry int
	AttemptsNotReported           int
}

// BudgetBackend is the budget-check surface a Backend can optionally
// implement -- the same capability-detection pattern nodes.go's NodeBackend
// already uses (see this file's and that file's package doc comments): a
// Backend that does not implement it (the in-memory fakes the unit tests in
// this package use unless they opt in) dispatches every firing with no
// budget enforcement at all, exactly as every declaration did before task
// t11.
type BudgetBackend interface {
	// ApplicableBudgets returns every declared budget that governs a firing
	// about to dispatch: its landing node, the target machine (empty for an
	// action with no with.uses, e.g. human.ask -- never matches a machine
	// budget), the declaration itself, and every alias that directly
	// contains it.
	ApplicableBudgets(ctx context.Context, namespaceID, node, machine, declarationID string) ([]Budget, error)
	// BudgetSpend reads what one (scope, key) budget has already spent.
	BudgetSpend(ctx context.Context, namespaceID string, scope BudgetScope, key string) (BudgetSpend, error)
	// ChargeBudgetSpend records one dispatched firing's session against
	// every scope it belongs to (node, machine, declaration, its aliases),
	// so a later firing under any of them sees this one in its spend. Called
	// only once a firing has cleared every applicable budget check --
	// exactly the ADR 0011 §4 "over-count on the side of caution" position,
	// immediately BEFORE the dispatcher is invoked.
	ChargeBudgetSpend(ctx context.Context, namespaceID, firingID, node, machine, declarationID string) error
	// EmitBudgetExhausted appends the action.budget_exhausted signal event a
	// blocked dispatch is linked to (by node name) and returns its id, so
	// the caller can run it back through Handle.
	EmitBudgetExhausted(ctx context.Context, namespaceID, node, reason string) (eventID string, err error)
}

// actionMachine reads the "target machine" a rendered action would dispatch
// to: the same with.uses field dispatch.go's workerEnvelope decodes. An
// action with none (human.ask, or a malformed With in a hand-built test)
// reads as "", which never matches a machine-scoped budget -- there is no
// machine this dispatch would spend against.
func actionMachine(action decl.Action) string {
	var with struct {
		Uses string `json:"uses"`
	}
	if err := json.Unmarshal(action.With, &with); err != nil {
		return ""
	}
	return with.Uses
}

// checkBudgets is task t11's guard. ok is false only when at least one
// applicable budget has no headroom left; err is non-nil only when a budget
// could not be READ, in which case ok's value is meaningless -- exactly
// ADR 0011 §2's "a budget that cannot be read resolves neither way" rule:
// the caller must treat a read error as transient, not as a refusal.
func (e *Engine) checkBudgets(ctx context.Context, namespaceID, node, machine string, a ActiveDeclaration) (ok bool, reason string, err error) {
	bb, isBudgetBackend := e.backend.(BudgetBackend)
	if !isBudgetBackend {
		return true, "", nil
	}
	budgets, err := bb.ApplicableBudgets(ctx, namespaceID, node, machine, a.ID)
	if err != nil {
		return false, "", fmt.Errorf("declengine: read applicable budgets: %w", err)
	}
	for _, b := range budgets {
		spend, err := bb.BudgetSpend(ctx, namespaceID, b.Scope, b.Key)
		if err != nil {
			return false, "", fmt.Errorf("declengine: read spend for %s budget %q: %w", b.Scope, b.Key, err)
		}
		if b.MaxSessions > 0 && spend.Sessions+1 > b.MaxSessions {
			return false, fmt.Sprintf(
				"%s budget %q is exhausted: dispatching would open session %d of a declared ceiling of %d (ADR 0011 max_sessions)",
				b.Scope, b.Key, spend.Sessions+1, b.MaxSessions), nil
		}
		if b.MaxUncachedInput > 0 && spend.UncachedInputTokens >= b.MaxUncachedInput {
			return false, fmt.Sprintf(
				"%s budget %q is exhausted: %d uncached input tokens already spent against a declared ceiling of %d "+
					"(ADR 0011 max_uncached_input; %d attempts reported no cache telemetry and were charged in full, "+
					"%d attempts reported no usage at all and could not be charged)",
				b.Scope, b.Key, spend.UncachedInputTokens, b.MaxUncachedInput,
				spend.AttemptsWithoutCacheTelemetry, spend.AttemptsNotReported), nil
		}
	}
	return true, "", nil
}

// dispatchActs reports whether this engine's dispatch will really invoke the
// action: false when the dispatcher is the switch's ShadowGate and the
// namespace is not in 'after'. The read agrees with ShadowGate.Dispatch's own
// because a production evaluation holds the namespace's shared switch lock
// from its first mode read to its last write (switchlock.go). Any other
// dispatcher always acts.
func (e *Engine) dispatchActs(ctx context.Context, namespaceID string) (bool, error) {
	var gate ShadowGate
	switch g := e.dispatcher.(type) {
	case ShadowGate:
		gate = g
	case *ShadowGate:
		gate = *g
	default:
		return true, nil
	}
	if gate.Switch == nil {
		return false, errors.New("declengine: shadow gate needs a switch store")
	}
	mode, err := gate.Switch.Mode(ctx, namespaceID)
	return mode == ModeAfter, err
}

// chargeBudgetSpend records one dispatched firing against every scope it
// belongs to. A no-op when the backend does not implement BudgetBackend.
// evaluate() calls it only for a dispatch that acts (dispatchActs): shadow
// records its would-fire firing without consuming a real budget.
func (e *Engine) chargeBudgetSpend(ctx context.Context, namespaceID, firingID, node, machine, declarationID string) error {
	bb, isBudgetBackend := e.backend.(BudgetBackend)
	if !isBudgetBackend {
		return nil
	}
	return bb.ChargeBudgetSpend(ctx, namespaceID, firingID, node, machine, declarationID)
}

// emitBudgetExhausted appends action.budget_exhausted, linked to the node
// the blocked dispatch would have opened, and runs it back through Handle so
// any declaration reacting to it can fire -- nodes.go's ExpireDue and
// EmitActionResults' exact pattern for their own emitted trigger events. A
// no-op when the backend does not implement BudgetBackend: nothing to link
// the visible record to.
func (e *Engine) emitBudgetExhausted(ctx context.Context, namespaceID, node, reason string) error {
	bb, isBudgetBackend := e.backend.(BudgetBackend)
	if !isBudgetBackend {
		return nil
	}
	eventID, err := bb.EmitBudgetExhausted(ctx, namespaceID, node, reason)
	if err != nil {
		return fmt.Errorf("declengine: emit %s: %w", ActionTriggerBudgetExhausted, err)
	}
	if eventID == "" {
		return nil
	}
	return e.Handle(ctx, Event{NamespaceID: namespaceID, ID: eventID, Kind: ActionTriggerBudgetExhausted, Node: node})
}

// PostgresBackend budget methods (migration 0063).

// ApplicableBudgets reads declaration_budgets for the four scopes a firing
// belongs to in one query: its node, its machine (skipped when "", since
// scope_key <> ” is enforced and an empty machine can never match a stored
// row), its declaration, and every alias directly containing it (a lookup
// through declaration_alias_members -- nested parent aliases are
// deliberately not walked; direct membership only, see migration 0063's
// header).
const applicableBudgetsSQL = `SELECT scope, scope_key, max_sessions, max_uncached_input FROM declaration_budgets
 WHERE namespace_id = $1 AND (
   (scope = 'node' AND scope_key = $2) OR
   (scope = 'machine' AND $3 <> '' AND scope_key = $3) OR
   (scope = 'declaration' AND scope_key = $4) OR
   (scope = 'alias' AND scope_key IN (
     SELECT a.name FROM declaration_alias_members m JOIN declaration_aliases a ON a.id = m.alias_id
     WHERE m.namespace_id = $1 AND m.declaration_id = $4))
 ) ORDER BY scope, scope_key`

func (p PostgresBackend) ApplicableBudgets(ctx context.Context, namespaceID, node, machine, declarationID string) ([]Budget, error) {
	rows, err := p.Store.Pool().Query(ctx, applicableBudgetsSQL, namespaceID, node, machine, declarationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Budget
	for rows.Next() {
		var b Budget
		var maxSessions *int
		var maxUncachedInput *int64
		if err := rows.Scan(&b.Scope, &b.Key, &maxSessions, &maxUncachedInput); err != nil {
			return nil, err
		}
		if maxSessions != nil {
			b.MaxSessions = *maxSessions
		}
		if maxUncachedInput != nil {
			b.MaxUncachedInput = *maxUncachedInput
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// budgetSpendSQL reads both units for one (scope, key) budget over the
// declaration_budget_spend rows that match it. The session count is a plain
// count of matching firings (one row each, migration 0063); the
// uncached-input aggregate joins through to node_runs/attempts exactly the
// way postgres.RunUncachedInput does for one run, here summed over every
// firing this scope has spent. The subquery keeps the session count correct
// even when a firing has zero or several attempts (a LEFT JOIN would
// otherwise multiply or drop it); COALESCE keeps every aggregate defined
// over an empty scope (unspent budget) instead of returning NULL rows.
//
// AND NOT EXISTS (... sup.supersedes = attempts.id) is ADR 0012 §3's
// superseded-row exclusion (internal/store/postgres/latereconcile.go
// attemptCurrentUnaliasedSQL), inlined rather than imported: this package
// already talks to Postgres with its own raw SQL throughout (RecentFirings,
// SubjectInFlight, ExpireDue) rather than through the postgres package's
// private helpers.
const budgetSpendSQL = `WITH scoped AS (
  SELECT firing_id FROM declaration_budget_spend s
  WHERE s.namespace_id = $1 AND (
    ($2 = 'node' AND s.node_name = $3) OR
    ($2 = 'machine' AND s.machine = $3) OR
    ($2 = 'declaration' AND s.declaration_id = $3) OR
    ($2 = 'alias' AND s.firing_id IN (
      SELECT dba.firing_id FROM declaration_budget_spend_aliases dba
      JOIN declaration_aliases al ON al.id = dba.alias_id
      WHERE al.namespace_id = $1 AND al.name = $3))
  )
)
SELECT
  (SELECT count(*) FROM scoped),
  COALESCE(SUM(a.usage_input_tokens - COALESCE(a.usage_cached_input_tokens, 0)), 0),
  COUNT(*) FILTER (WHERE a.usage_input_tokens IS NOT NULL AND a.usage_cached_input_tokens IS NULL),
  COUNT(*) FILTER (WHERE a.usage_input_tokens IS NULL)
FROM scoped sc
LEFT JOIN node_runs nr ON nr.run_id = sc.firing_id
LEFT JOIN attempts a ON a.node_run_id = nr.id
  AND NOT EXISTS (SELECT 1 FROM attempts sup WHERE sup.supersedes = a.id)`

func (p PostgresBackend) BudgetSpend(ctx context.Context, namespaceID string, scope BudgetScope, key string) (BudgetSpend, error) {
	var s BudgetSpend
	err := p.Store.Pool().QueryRow(ctx, budgetSpendSQL, namespaceID, string(scope), key).Scan(
		&s.Sessions, &s.UncachedInputTokens, &s.AttemptsWithoutCacheTelemetry, &s.AttemptsNotReported)
	if err != nil {
		return BudgetSpend{}, err
	}
	return s, nil
}

// ChargeBudgetSpend records one dispatched firing's charge row plus its
// alias memberships. ON CONFLICT DO NOTHING on the firing row makes this
// idempotent the same way migration 0023's RecordSessionStart is: a caller
// that re-enters the same dispatch charges it once.
func (p PostgresBackend) ChargeBudgetSpend(ctx context.Context, namespaceID, firingID, node, machine, declarationID string) error {
	tag, err := p.Store.Pool().Exec(ctx, `INSERT INTO declaration_budget_spend(firing_id,namespace_id,node_name,machine,declaration_id)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT (firing_id) DO NOTHING`, firingID, namespaceID, node, machine, declarationID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Already charged by an earlier entry into this same dispatch.
		return nil
	}
	_, err = p.Store.Pool().Exec(ctx, `INSERT INTO declaration_budget_spend_aliases(firing_id,alias_id)
 SELECT $1,m.alias_id FROM declaration_alias_members m WHERE m.namespace_id=$2 AND m.declaration_id=$3`,
		firingID, namespaceID, declarationID)
	return err
}

// EmitBudgetExhausted appends action.budget_exhausted as an ordinary
// signal_events row -- the same shape ExpireDue (node.go) writes for
// node.expired -- so Handle's ordinary matching loop picks it up when the
// caller runs the returned id back through it.
func (p PostgresBackend) EmitBudgetExhausted(ctx context.Context, namespaceID, node, reason string) (string, error) {
	eventID, payload, err := budgetExhaustedEventPayload(node, reason)
	if err != nil {
		return "", err
	}
	if _, err := p.Store.Pool().Exec(ctx, `INSERT INTO signal_events(id,namespace_id,name,payload,emitter) VALUES($1,$2,$3,$4,$5)`,
		eventID, namespaceID, ActionTriggerBudgetExhausted, payload, DeclarationEngineActorID); err != nil {
		return "", err
	}
	return eventID, nil
}

// budgetExhaustedEventPayload mints a fresh event id and its JSON payload
// for one action.budget_exhausted emission -- split out from
// EmitBudgetExhausted so the id and payload construction is unit-testable
// without a database.
func budgetExhaustedEventPayload(node, reason string) (eventID string, payload []byte, err error) {
	eventID = store.NewULID()
	payload, err = json.Marshal(map[string]any{"node_name": node, "reason": reason})
	return eventID, payload, err
}
