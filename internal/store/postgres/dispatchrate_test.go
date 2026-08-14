package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/pacing"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The dispatch pacing control's durable state (task t10, spec claims c5/c43,
// honesty conditions h4/h36).
//
// h4's acceptance is a claim about PROCESSES, not about a function: "an
// operator-declared session rate is honored across horizontally-scaled
// workers (durable state, not per-process memory)". So the headline test
// here does not call the store twice in a row and check the second answer --
// it opens a SECOND connection pool, which is what a second worker process
// actually is from this database's point of view, and races both of them at
// the same rate row. An in-memory limiter passes the sequential version of
// this test and fails this one, which is the whole point.

// secondWorkerStore opens an independent *Store against the same database.
// Two pools, two sets of connections, no shared Go state -- as close to a
// second worker process as one test binary gets.
func secondWorkerStore(t *testing.T, s *postgres.Store) *postgres.Store {
	t.Helper()
	other, err := postgres.Connect(context.Background(), s.Pool().Config().ConnString())
	if err != nil {
		t.Fatalf("connect a second worker's store: %v", err)
	}
	t.Cleanup(other.Close)
	return other
}

// freshRate is a rate anchored a minute ago with a day-long window, so every
// test runs at the very start of a window: the full declared budget is
// available and no test's runtime can cross a reset. Anchoring relative to
// now is deliberate -- a fixed historical anchor would make these tests pass
// or fail depending on what time of day CI ran them, which is precisely the
// reset-clock sensitivity the control exists to model.
func freshRate(limit int) pacing.Config {
	return pacing.Config{
		Limit:  limit,
		Window: 24 * time.Hour,
		// Truncated to the database's own timestamp resolution so the
		// anchor this test asserts on is the anchor the row can store.
		Anchor: time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute),
	}
}

func globalRequest(cfg pacing.Config) postgres.RateRequest {
	return postgres.RateRequest{Scope: postgres.RateScopeGlobal, Config: cfg}
}

func actorRequest(actorKey string, cfg pacing.Config) postgres.RateRequest {
	return postgres.RateRequest{Scope: postgres.RateScopeActor, ScopeKey: actorKey, Config: cfg}
}

// openTheNextSlot brings a scope's spacing deadline forward to now, which is
// what the passage of real time would do. Tests use it instead of sleeping
// through a pace interval: the arithmetic that PRODUCES the deadline is
// pinned in internal/pacing's own unit tests, and what these tests are about
// is whether the durable state is honored jointly.
func openTheNextSlot(t *testing.T, s *postgres.Store, namespaceID, scope, scopeKey string) {
	t.Helper()
	if _, err := s.Pool().Exec(context.Background(), `
		UPDATE dispatch_rate_state SET next_dispatch_at = now()
		WHERE namespace_id = $1 AND scope = $2 AND scope_key = $3
	`, namespaceID, scope, scopeKey); err != nil {
		t.Fatalf("open the next slot: %v", err)
	}
}

func rateRow(t *testing.T, s *postgres.Store, namespaceID, scope, scopeKey string) postgres.DispatchRateState {
	t.Helper()
	states, err := s.ListDispatchRates(context.Background(), namespaceID)
	if err != nil {
		t.Fatalf("ListDispatchRates: %v", err)
	}
	for _, st := range states {
		if st.Scope == scope && st.ScopeKey == scopeKey {
			return st
		}
	}
	t.Fatalf("no dispatch_rate_state row for scope %q/%q (have %+v)", scope, scopeKey, states)
	return postgres.DispatchRateState{}
}

// TestTwoWorkersSharingOneDatabaseJointlyHonorOneRate is h4.
//
// Twenty-four concurrent consumers spread across two independent stores race
// for the same rate. Exactly one may win each round -- the pace admits one
// dispatch and then closes until its slot elapses -- and after the window's
// declared budget is spent, every one of them is refused with the reason
// that says so.
func TestTwoWorkersSharingOneDatabaseJointlyHonorOneRate(t *testing.T) {
	s := requireStore(t)
	ns := mustNamespace(t, s, "test-dispatch-rate-concurrent")
	other := secondWorkerStore(t, s)

	const limit = 4
	cfg := freshRate(limit)

	admitted := 0
	for round := 1; round <= limit+1; round++ {
		if round > 1 {
			// Simulate the pace interval elapsing; the budget is untouched.
			openTheNextSlot(t, s, ns.ID, postgres.RateScopeGlobal, "")
		}

		const racers = 24
		var (
			start    sync.WaitGroup
			done     sync.WaitGroup
			mu       sync.Mutex
			allows   int
			reasons  = map[string]int{}
			failures []error
		)
		start.Add(1)
		done.Add(racers)
		for i := 0; i < racers; i++ {
			store := s
			if i%2 == 1 {
				store = other // the other "worker process"
			}
			go func(store *postgres.Store) {
				defer done.Done()
				start.Wait()
				d, err := store.ConsumeDispatchSlots(context.Background(), ns.ID,
					[]postgres.RateRequest{globalRequest(cfg)})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err != nil:
					failures = append(failures, err)
				case d.Allowed:
					allows++
				default:
					reasons[d.Reason]++
				}
			}(store)
		}
		start.Done()
		done.Wait()

		if len(failures) > 0 {
			t.Fatalf("round %d: concurrent consumers must never error: %v", round, failures)
		}

		if round <= limit {
			if allows != 1 {
				t.Fatalf("round %d: %d of %d concurrent consumers were admitted, want exactly 1 "+
					"(h4: one rate, jointly honored, not one per process)", round, allows, racers)
			}
			admitted++
			// Everybody else is told why, and the reason is the true one:
			// paced while budget remains, budget-exhausted once the round
			// that spends the last slot has spent it.
			wantReason := pacing.ReasonSpacing
			if round == limit {
				wantReason = pacing.ReasonWindowBudget
			}
			if reasons[wantReason] != racers-1 {
				t.Errorf("round %d: refusal reasons = %v, want %d %q", round, reasons, racers-1, wantReason)
			}
			continue
		}

		// The budget is spent: the rate now refuses everybody, and says why.
		if allows != 0 {
			t.Errorf("round %d: %d consumers admitted past the declared budget of %d", round, allows, limit)
		}
		if reasons[pacing.ReasonWindowBudget] != racers {
			t.Errorf("round %d: refusal reasons = %v, want all %d %q", round, reasons, racers, pacing.ReasonWindowBudget)
		}
	}

	if admitted != limit {
		t.Errorf("admitted %d dispatches in the window, want exactly the declared %d", admitted, limit)
	}
	row := rateRow(t, s, ns.ID, postgres.RateScopeGlobal, "")
	if row.Dispatched != limit {
		t.Errorf("durable dispatched = %d, want %d", row.Dispatched, limit)
	}
	if row.Limit != limit || row.Window != cfg.Window {
		t.Errorf("row records rate %d/%s, want %d/%s", row.Limit, row.Window, limit, cfg.Window)
	}
}

// A second store must see the first's consumption -- the property an
// in-memory limiter does not have.
func TestDispatchRateStateIsSharedAcrossStores(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-dispatch-rate-shared")
	other := secondWorkerStore(t, s)
	cfg := freshRate(2)

	first, err := s.ConsumeDispatchSlots(ctx, ns.ID, []postgres.RateRequest{globalRequest(cfg)})
	if err != nil {
		t.Fatalf("ConsumeDispatchSlots: %v", err)
	}
	if !first.Allowed {
		t.Fatalf("the first dispatch must be admitted: %+v", first)
	}

	second, err := other.ConsumeDispatchSlots(ctx, ns.ID, []postgres.RateRequest{globalRequest(cfg)})
	if err != nil {
		t.Fatalf("ConsumeDispatchSlots on the second store: %v", err)
	}
	if second.Allowed {
		t.Fatalf("a second worker process must be paced by the first's dispatch: %+v", second)
	}
	if second.Reason != pacing.ReasonSpacing {
		t.Errorf("reason = %q, want %q", second.Reason, pacing.ReasonSpacing)
	}
	if !second.RetryAt.After(time.Now().UTC()) {
		t.Errorf("retry at = %s, want a future instant", second.RetryAt)
	}
}

// Scopes compose: a dispatch needs headroom in the global rate AND in its
// actor's. The atomicity that matters is the refusal case -- a global slot
// must not be spent on a dispatch the actor's own rate then refuses, or the
// installation's budget would drain on dispatches that never happened.
func TestARefusedActorScopeDoesNotSpendTheGlobalSlot(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-dispatch-rate-scopes")

	globalCfg := freshRate(10)
	actorCfg := freshRate(1)
	reqs := []postgres.RateRequest{globalRequest(globalCfg), actorRequest("company/analyzer", actorCfg)}

	first, err := s.ConsumeDispatchSlots(ctx, ns.ID, reqs)
	if err != nil {
		t.Fatalf("ConsumeDispatchSlots: %v", err)
	}
	if !first.Allowed {
		t.Fatalf("the first dispatch must be admitted: %+v", first)
	}

	// Open the global pace so ONLY the actor scope can refuse the next one.
	openTheNextSlot(t, s, ns.ID, postgres.RateScopeGlobal, "")

	second, err := s.ConsumeDispatchSlots(ctx, ns.ID, reqs)
	if err != nil {
		t.Fatalf("ConsumeDispatchSlots: %v", err)
	}
	if second.Allowed {
		t.Fatalf("the actor's own budget of 1 must refuse the second dispatch: %+v", second)
	}
	if second.Scope != postgres.RateScopeActor || second.ScopeKey != "company/analyzer" {
		t.Errorf("refusal names scope %q/%q, want the actor scope that refused", second.Scope, second.ScopeKey)
	}
	if second.Reason != pacing.ReasonWindowBudget {
		t.Errorf("reason = %q, want %q", second.Reason, pacing.ReasonWindowBudget)
	}

	if got := rateRow(t, s, ns.ID, postgres.RateScopeGlobal, "").Dispatched; got != 1 {
		t.Errorf("global dispatched = %d, want 1: a refused dispatch must not spend another scope's slot", got)
	}
	if got := rateRow(t, s, ns.ID, postgres.RateScopeActor, "company/analyzer").Dispatched; got != 1 {
		t.Errorf("actor dispatched = %d, want 1", got)
	}
}

// Two actors are paced independently: one exhausted actor does not stop the
// other, which is the difference between a rate control and a stop button.
func TestActorScopesArePacedIndependently(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-dispatch-rate-per-actor")
	cfg := freshRate(1)

	if d, err := s.ConsumeDispatchSlots(ctx, ns.ID, []postgres.RateRequest{actorRequest("company/analyzer", cfg)}); err != nil || !d.Allowed {
		t.Fatalf("first analyzer dispatch: allowed=%v err=%v", d.Allowed, err)
	}
	if d, err := s.ConsumeDispatchSlots(ctx, ns.ID, []postgres.RateRequest{actorRequest("company/analyzer", cfg)}); err != nil || d.Allowed {
		t.Fatalf("second analyzer dispatch must be refused: allowed=%v err=%v", d.Allowed, err)
	}
	d, err := s.ConsumeDispatchSlots(ctx, ns.ID, []postgres.RateRequest{actorRequest("company/reviewer", cfg)})
	if err != nil {
		t.Fatalf("ConsumeDispatchSlots: %v", err)
	}
	if !d.Allowed {
		t.Errorf("a second actor must have its own rate: %+v", d)
	}
}

// The window rolls on the anchor clock alone: no sweep, no reaper, and a row
// left over from a spent window admits again the moment its window is past.
func TestDispatchRateWindowRollsWithoutASweep(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-dispatch-rate-roll")
	cfg := pacing.Config{Limit: 1, Window: time.Hour, Anchor: time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)}

	if d, err := s.ConsumeDispatchSlots(ctx, ns.ID, []postgres.RateRequest{globalRequest(cfg)}); err != nil || !d.Allowed {
		t.Fatalf("first dispatch: allowed=%v err=%v", d.Allowed, err)
	}
	if d, err := s.ConsumeDispatchSlots(ctx, ns.ID, []postgres.RateRequest{globalRequest(cfg)}); err != nil || d.Allowed {
		t.Fatalf("second dispatch in the same window must be refused: allowed=%v err=%v", d.Allowed, err)
	}

	// Age the row into the previous window, exactly as the clock would.
	if _, err := s.Pool().Exec(ctx, `
		UPDATE dispatch_rate_state
		SET window_started_at = window_started_at - interval '1 hour',
		    next_dispatch_at  = next_dispatch_at - interval '1 hour'
		WHERE namespace_id = $1 AND scope = $2
	`, ns.ID, postgres.RateScopeGlobal); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	d, err := s.ConsumeDispatchSlots(ctx, ns.ID, []postgres.RateRequest{globalRequest(cfg)})
	if err != nil {
		t.Fatalf("ConsumeDispatchSlots: %v", err)
	}
	if !d.Allowed {
		t.Fatalf("a new window must admit again with no sweep and no operator action: %+v", d)
	}
	if row := rateRow(t, s, ns.ID, postgres.RateScopeGlobal, ""); row.Dispatched != 1 {
		t.Errorf("dispatched after the roll = %d, want 1 (the previous window's count is not carried)", row.Dispatched)
	}
}

// Pacing that is not configured costs nothing: no row, no transaction, no
// refusal. The overwhelming majority of deployments run this path.
func TestDisabledPacingTouchesNothing(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-dispatch-rate-disabled")

	d, err := s.ConsumeDispatchSlots(ctx, ns.ID, []postgres.RateRequest{globalRequest(pacing.Config{})})
	if err != nil {
		t.Fatalf("ConsumeDispatchSlots: %v", err)
	}
	if !d.Allowed {
		t.Errorf("an unconfigured rate must admit everything: %+v", d)
	}
	states, err := s.ListDispatchRates(ctx, ns.ID)
	if err != nil {
		t.Fatalf("ListDispatchRates: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("disabled pacing wrote %d rows, want none: %+v", len(states), states)
	}
}

// The operator read surface reports the configured rate AND the current
// consumption, which is what "the configured rate and current consumption
// should be readable" means in practice.
func TestListDispatchRatesReportsConfigurationAndConsumption(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-dispatch-rate-read")
	cfg := freshRate(5)

	if _, err := s.ConsumeDispatchSlots(ctx, ns.ID, []postgres.RateRequest{
		globalRequest(cfg), actorRequest("company/analyzer", freshRate(2)),
	}); err != nil {
		t.Fatalf("ConsumeDispatchSlots: %v", err)
	}

	states, err := s.ListDispatchRates(ctx, ns.ID)
	if err != nil {
		t.Fatalf("ListDispatchRates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("ListDispatchRates returned %d rows, want the global and actor scopes: %+v", len(states), states)
	}
	// Ordered by scope then key: 'actor' sorts before 'global'.
	if states[0].Scope != postgres.RateScopeActor || states[1].Scope != postgres.RateScopeGlobal {
		t.Errorf("rows = %q/%q, want a stable (scope, key) order", states[0].Scope, states[1].Scope)
	}

	g := states[1]
	if g.Limit != 5 || g.Window != cfg.Window || !g.Anchor.Equal(cfg.Anchor.UTC()) {
		t.Errorf("global row records rate %d per %s anchored %s, want 5 per %s anchored %s",
			g.Limit, g.Window, g.Anchor, cfg.Window, cfg.Anchor)
	}
	if g.Dispatched != 1 {
		t.Errorf("global dispatched = %d, want 1", g.Dispatched)
	}
	if g.NextDispatchAt == nil || !g.NextDispatchAt.After(time.Now().UTC()) {
		t.Errorf("next_dispatch_at = %v, want the future slot this dispatch scheduled", g.NextDispatchAt)
	}
	if g.LastDispatchAt == nil {
		t.Errorf("last_dispatch_at is NULL after an admitted dispatch")
	}
	// Measured from the window's own start so the assertion is about the
	// arithmetic rather than about what time the test ran.
	if got := g.Remaining(g.WindowStartedAt); got != 4 {
		t.Errorf("remaining allowance = %d, want 4", got)
	}
}
