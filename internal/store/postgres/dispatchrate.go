package postgres

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/pacing"
)

// The dispatch pacing control's durable state (migration 0022, task t10 of
// the economy-discord-graphs plan; issue #48 item 2, spec claims c5/c43,
// honesty conditions h4/h36).
//
// This file is the durable half of a rate limiter whose arithmetic lives in
// internal/pacing. The split is deliberate: the arithmetic is a pure function
// of (config, state, now) and is unit-tested without a database, while
// everything difficult about a rate limiter for a horizontally-scaled worker
// fleet -- that the state is SHARED, that the clock is shared, and that two
// workers racing for the last slot must not both get it -- is here.
//
// THREE PROPERTIES, EACH ENFORCED HERE RATHER THAN LEFT TO CALLERS:
//
//   - THE CLOCK IS THE DATABASE'S. now() is read inside the transaction and
//     handed to the arithmetic, exactly as Store.ActivePause compares against
//     now() in SQL rather than against the caller's clock (see
//     actoravailability.go). Two workers on two machines with skewed clocks
//     must agree on which session window they are in; the database is the
//     only clock they share. Because it is transaction_timestamp(), every
//     scope in one decision is also evaluated at ONE instant.
//   - A DECISION IS ALL-OR-NOTHING ACROSS SCOPES. A dispatch consults the
//     global rate and its actor's rate, and needs headroom in both. If the
//     second refuses, the first must not have been spent -- otherwise the
//     installation's session budget drains on dispatches that never happened.
//     So all scopes are consumed in one transaction and a refusal rolls the
//     whole thing back.
//   - ASKING IS FREE. A refusal writes nothing. A work item deferred by
//     pacing will ask again in a few minutes, and again after that; if each
//     question cost something, the pacing would meter its own overhead.
//
// WHY A ROW LOCK RATHER THAN A CLEVER SINGLE STATEMENT. The trip in
// actoravailability.go is one idempotent upsert because its conflict rule
// ("keep the later deadline") fits in a WHERE clause. This decision does not:
// it depends on which window the row belongs to, what the window has already
// consumed, when the next slot opens, and a configuration the row does not
// carry. Expressing that in SQL would duplicate internal/pacing in a
// dialect nothing can unit-test, and the two copies would drift. So the row
// is locked (an upsert whose conflict branch is a no-op UPDATE, which takes
// the lock and returns the current values), the Go arithmetic decides, and
// the write happens under the same lock. Contention is per scope key and the
// critical section is two statements long.

// Rate scopes. A dispatch consults every scope that applies to it and needs
// headroom in all of them.
const (
	// RateScopeGlobal is the whole installation's session rate. Its scope key
	// is the empty string -- there is exactly one.
	RateScopeGlobal = "global"
	// RateScopeActor is one actor key's own rate. Keyed by actor_key rather
	// than by an actors-table row id for the same reason
	// actor_availability is (migration 0020's header): the rate belongs to
	// the identity, not to one append-only registration revision, and the
	// dispatch site can always produce a key.
	RateScopeActor = "actor"
)

// RateRequest is one scope a dispatch must find headroom in.
type RateRequest struct {
	Scope    string
	ScopeKey string
	Config   pacing.Config
}

// DispatchRateDecision is the answer to "may this dispatch go now".
//
// On a refusal it names the scope that refused, why, and when to ask again --
// everything the dispatch site needs to defer the work item and record an
// event a human can read. On an admission the slot has already been consumed
// durably; there is no second call to confirm, because a two-phase reservation
// would need a compensating release on every path that can fail afterwards.
type DispatchRateDecision struct {
	Allowed bool
	// Scope/ScopeKey name the scope that refused. Empty when Allowed.
	Scope    string
	ScopeKey string
	// Reason is one of internal/pacing's Reason* constants.
	Reason string
	// RetryAt is when the refusing scope is worth asking again.
	RetryAt time.Time
	// Window is the refusing scope's current session window, and Limit,
	// Dispatched and Allowance describe its state at the moment of the
	// decision -- rendered into the deferral event so "why did this not
	// dispatch" is answerable from the run's own event stream.
	Window     pacing.Window
	Limit      int
	Dispatched int
	Allowance  int
	// Now is the database clock instant the decision was made at.
	Now time.Time
}

// DispatchRateState is one dispatch_rate_state row: the rate a scope is being
// held to and what it has consumed. It is the operator read surface's type
// (GET /v1alpha1/dispatch-rates and the actors surface's per-actor block).
type DispatchRateState struct {
	NamespaceID string
	Scope       string
	ScopeKey    string
	// The configuration the last consuming worker enforced. See migration
	// 0022's header for why the row carries it.
	Limit  int
	Window time.Duration
	Anchor time.Time
	// WindowStartedAt is the window Dispatched counts; a row whose window is
	// older than the current one has consumed nothing in this one.
	WindowStartedAt time.Time
	Dispatched      int
	// NextDispatchAt is the pace made durable: the earliest instant the next
	// dispatch in this scope may go. Nil when nothing has dispatched yet.
	NextDispatchAt *time.Time
	LastDispatchAt *time.Time
	UpdatedAt      time.Time
}

// Config reconstructs the pacing configuration this row was written under.
func (d DispatchRateState) Config() pacing.Config {
	return pacing.Config{Limit: d.Limit, Window: d.Window, Anchor: d.Anchor}
}

// ConsumedAt is how much of the window containing now this scope has
// consumed: the stored counter when the row belongs to that window, zero when
// it belongs to an older one. A stale row is history, not consumption -- the
// same "presence is not the predicate" rule actor_availability follows.
func (d DispatchRateState) ConsumedAt(now time.Time) int {
	if d.Config().WindowAt(now).Start.After(d.WindowStartedAt) {
		return 0
	}
	return d.Dispatched
}

// Remaining is how many more dispatches this scope may admit before its
// window ends: the remaining-window capacity capped by the remaining budget
// (see pacing.Config.Allowance).
func (d DispatchRateState) Remaining(now time.Time) int {
	return d.Config().Allowance(now, d.ConsumedAt(now))
}

// State renders this row as the arithmetic's input.
func (d DispatchRateState) State() pacing.State {
	s := pacing.State{WindowStart: d.WindowStartedAt, Dispatched: d.Dispatched}
	if d.NextDispatchAt != nil {
		s.NextDispatchAt = *d.NextDispatchAt
	}
	return s
}

// lockRateStateSQL materialises and row-locks one scope, returning whatever
// state it already had.
//
// The ON CONFLICT branch is a no-op update (it writes updated_at back to
// itself) purely for its lock: PostgreSQL takes a row lock on the conflicting
// row and RETURNING then yields the row's CURRENT values, which is exactly
// "lock this row and tell me what it says" in one round trip. A plain SELECT
// ... FOR UPDATE cannot do the same job, because it locks nothing when the
// row does not exist yet and two workers would race to insert it.
const lockRateStateSQL = `
INSERT INTO dispatch_rate_state (
	namespace_id, scope, scope_key,
	window_anchor, window_seconds, limit_per_window,
	window_started_at, dispatched, updated_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, 0, now())
ON CONFLICT (namespace_id, scope, scope_key) DO UPDATE
SET updated_at = dispatch_rate_state.updated_at
RETURNING window_started_at, dispatched, next_dispatch_at`

// consumeRateSlotSQL records an admitted dispatch. It also refreshes the
// recorded configuration, so the operator surface reports the rate that is
// actually being enforced rather than the one the first worker ever to
// dispatch believed in.
const consumeRateSlotSQL = `
UPDATE dispatch_rate_state
SET window_anchor     = $4,
    window_seconds    = $5,
    limit_per_window  = $6,
    window_started_at = $7,
    dispatched        = $8,
    next_dispatch_at  = $9,
    last_dispatch_at  = $10,
    updated_at        = now()
WHERE namespace_id = $1 AND scope = $2 AND scope_key = $3`

// ConsumeDispatchSlots asks every configured scope for headroom and, when all
// of them have it, consumes one slot in each.
//
// It returns an admission or the first refusal, never an error for being
// refused: pacing is a scheduling answer, not a failure. Requests whose
// configuration is disabled (no limit or no window) are skipped entirely --
// an installation that has not configured pacing does no database work here
// at all, which is what makes this safe to call on every dispatch.
//
// Scopes are consulted in a deterministic order (by scope, then key) so two
// workers consuming the same pair of scopes always take their row locks in
// the same order and cannot deadlock against each other.
func (s *Store) ConsumeDispatchSlots(ctx context.Context, namespaceID string, requests []RateRequest) (DispatchRateDecision, error) {
	if namespaceID == "" {
		return DispatchRateDecision{}, fmt.Errorf("postgres: ConsumeDispatchSlots: namespaceID is required")
	}

	active := make([]RateRequest, 0, len(requests))
	for _, req := range requests {
		if req.Scope == "" {
			return DispatchRateDecision{}, fmt.Errorf("postgres: ConsumeDispatchSlots: a rate request must name a scope")
		}
		if req.Config.Enabled() {
			active = append(active, req)
		}
	}
	if len(active) == 0 {
		return DispatchRateDecision{Allowed: true}, nil
	}
	sort.Slice(active, func(i, j int) bool {
		if active[i].Scope != active[j].Scope {
			return active[i].Scope < active[j].Scope
		}
		return active[i].ScopeKey < active[j].ScopeKey
	})

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DispatchRateDecision{}, fmt.Errorf("postgres: ConsumeDispatchSlots: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return DispatchRateDecision{}, fmt.Errorf("postgres: ConsumeDispatchSlots: read the database clock: %w", err)
	}
	now = now.UTC()

	decisions := make([]pacing.Decision, len(active))
	for i, req := range active {
		state, err := lockRateState(ctx, tx, namespaceID, req, now)
		if err != nil {
			return DispatchRateDecision{}, err
		}
		d := req.Config.Decide(now, state)
		if !d.Allowed {
			// Roll back everything, including any slot an earlier scope in
			// this loop had already been given: no dispatch is happening, so
			// nothing may have been spent on it.
			return DispatchRateDecision{
				Scope:      req.Scope,
				ScopeKey:   req.ScopeKey,
				Reason:     d.Reason,
				RetryAt:    d.RetryAt,
				Window:     d.Window,
				Limit:      req.Config.Limit,
				Dispatched: d.Dispatched,
				Allowance:  d.Allowance,
				Now:        now,
			}, nil
		}
		decisions[i] = d
	}

	for i, req := range active {
		if err := writeRateSlot(ctx, tx, namespaceID, req, decisions[i], now); err != nil {
			return DispatchRateDecision{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return DispatchRateDecision{}, fmt.Errorf("postgres: ConsumeDispatchSlots: commit: %w", err)
	}
	return DispatchRateDecision{Allowed: true, Now: now}, nil
}

func lockRateState(ctx context.Context, tx pgx.Tx, namespaceID string, req RateRequest, now time.Time) (pacing.State, error) {
	window := req.Config.WindowAt(now)
	var (
		windowStart pgtype.Timestamptz
		dispatched  int32
		next        pgtype.Timestamptz
	)
	err := tx.QueryRow(ctx, lockRateStateSQL,
		namespaceID, req.Scope, req.ScopeKey,
		req.Config.Anchor.UTC(), int32(req.Config.Window/time.Second), int32(req.Config.Limit),
		window.Start,
	).Scan(&windowStart, &dispatched, &next)
	if err != nil {
		return pacing.State{}, fmt.Errorf("postgres: ConsumeDispatchSlots: lock rate scope %s/%s: %w",
			req.Scope, req.ScopeKey, err)
	}
	return pacing.State{
		WindowStart:    tsValue(windowStart),
		Dispatched:     int(dispatched),
		NextDispatchAt: tsValue(next),
	}, nil
}

func writeRateSlot(ctx context.Context, tx pgx.Tx, namespaceID string, req RateRequest, d pacing.Decision, now time.Time) error {
	_, err := tx.Exec(ctx, consumeRateSlotSQL,
		namespaceID, req.Scope, req.ScopeKey,
		req.Config.Anchor.UTC(), int32(req.Config.Window/time.Second), int32(req.Config.Limit),
		d.Next.WindowStart, int32(d.Next.Dispatched), d.Next.NextDispatchAt, now,
	)
	if err != nil {
		return fmt.Errorf("postgres: ConsumeDispatchSlots: consume rate scope %s/%s: %w",
			req.Scope, req.ScopeKey, err)
	}
	return nil
}

const dispatchRateColumns = `namespace_id, scope, scope_key, window_anchor, window_seconds, limit_per_window,
	window_started_at, dispatched, next_dispatch_at, last_dispatch_at, updated_at`

// ListDispatchRates returns every rate scope with recorded state in this
// namespace, ordered by (scope, key).
//
// A scope with no row has never admitted a dispatch under pacing -- which is
// not the same as "not configured", and the read surface says so rather than
// inventing a zeroed row for a rate nobody has exercised yet.
func (s *Store) ListDispatchRates(ctx context.Context, namespaceID string) ([]DispatchRateState, error) {
	if namespaceID == "" {
		return nil, fmt.Errorf("postgres: ListDispatchRates: namespaceID is required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+dispatchRateColumns+` FROM dispatch_rate_state
		 WHERE namespace_id = $1 ORDER BY scope, scope_key`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("postgres: ListDispatchRates: %w", err)
	}
	defer rows.Close()
	return scanDispatchRates(rows, "ListDispatchRates")
}

// DispatchRates is the namespace-bound mirror of ListDispatchRates for the
// read/write API surface (internal/api/dispatchrates.go), the same shape
// engineQueries.ActorPauses has for the breaker.
func (eq engineQueries) DispatchRates(ctx context.Context) ([]DispatchRateState, error) {
	rows, err := eq.q.Query(ctx,
		`SELECT `+dispatchRateColumns+` FROM dispatch_rate_state
		 WHERE namespace_id = $1 ORDER BY scope, scope_key`, eq.namespaceID)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: DispatchRates: %w", err)
	}
	defer rows.Close()
	return scanDispatchRates(rows, "engine: DispatchRates")
}

func scanDispatchRates(rows pgx.Rows, what string) ([]DispatchRateState, error) {
	out := make([]DispatchRateState, 0)
	for rows.Next() {
		state, err := scanDispatchRate(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: %s: scan: %w", what, err)
		}
		out = append(out, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: %s: %w", what, err)
	}
	return out, nil
}

func scanDispatchRate(row interface{ Scan(dest ...any) error }) (DispatchRateState, error) {
	var (
		d                        DispatchRateState
		windowSeconds, limit     int32
		dispatched               int32
		anchor, windowStartedAt  pgtype.Timestamptz
		nextDispatch, lastDispat pgtype.Timestamptz
		updatedAt                pgtype.Timestamptz
	)
	if err := row.Scan(
		&d.NamespaceID, &d.Scope, &d.ScopeKey, &anchor, &windowSeconds, &limit,
		&windowStartedAt, &dispatched, &nextDispatch, &lastDispat, &updatedAt,
	); err != nil {
		return DispatchRateState{}, err
	}
	d.Limit = int(limit)
	d.Window = time.Duration(windowSeconds) * time.Second
	d.Anchor = tsValue(anchor)
	d.WindowStartedAt = tsValue(windowStartedAt)
	d.Dispatched = int(dispatched)
	d.NextDispatchAt = tsPtr(nextDispatch)
	d.LastDispatchAt = tsPtr(lastDispat)
	d.UpdatedAt = tsValue(updatedAt)
	return d, nil
}
