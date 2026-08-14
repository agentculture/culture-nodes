package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/pacing"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The dispatch pacing control's operator surface (task t10; issue #48 item
// 2, spec claim c5).
//
// The control itself is proved in internal/worker/pacing_test.go against a
// real backlog. What these tests own is the operator's half, and the bar t9
// set for the breaker applies unchanged: a rate that cannot be read is a rate
// nobody can reason about, so the configured rate AND the current consumption
// must both be visible -- which one is binding, how much of the window is
// left, and when the next dispatch may go.

// dispatchRateWire mirrors components.schemas.DispatchRate. Declared here
// rather than imported so the test reads the JSON contract, not the Go struct
// that produces it.
type dispatchRateWire struct {
	Scope           string `json:"scope"`
	ScopeKey        string `json:"scope_key"`
	LimitPerWindow  int    `json:"limit_per_window"`
	WindowSeconds   int    `json:"window_seconds"`
	WindowAnchor    string `json:"window_anchor"`
	WindowStartedAt string `json:"window_started_at"`
	WindowEndsAt    string `json:"window_ends_at"`
	Dispatched      int    `json:"dispatched"`
	Remaining       int    `json:"remaining"`
	NextDispatchAt  string `json:"next_dispatch_at"`
	LastDispatchAt  string `json:"last_dispatch_at"`
	UpdatedAt       string `json:"updated_at"`
}

type dispatchRateListWire struct {
	Items []dispatchRateWire `json:"items"`
}

type actorWithRateWire struct {
	ID           string            `json:"id"`
	ActorKey     string            `json:"actor_key"`
	DispatchRate *dispatchRateWire `json:"dispatch_rate"`
}

// consumeSlots drives the real store method, so these tests read exactly what
// a worker's dispatch writes -- no hand-built fixture rows.
func consumeSlots(t *testing.T, f *fixture, requests ...postgres.RateRequest) {
	t.Helper()
	d, err := f.store.ConsumeDispatchSlots(context.Background(), f.nsID, requests)
	if err != nil {
		t.Fatalf("ConsumeDispatchSlots: %v", err)
	}
	if !d.Allowed {
		t.Fatalf("fixture dispatch was refused by the rate: %+v", d)
	}
}

func testRate(limit int) pacing.Config {
	return pacing.Config{
		Limit:  limit,
		Window: 6 * time.Hour,
		Anchor: time.Now().UTC().Truncate(time.Microsecond),
	}
}

func TestDispatchRatesSurfaceReportsConfiguredRateAndConsumption(t *testing.T) {
	f := newFixture(t)
	actorID := f.insertActor("paced-analyzer")
	actorKey := actorKeyOfRow(t, f, actorID)

	consumeSlots(t, f,
		postgres.RateRequest{Scope: postgres.RateScopeGlobal, Config: testRate(8)},
		postgres.RateRequest{Scope: postgres.RateScopeActor, ScopeKey: actorKey, Config: testRate(3)},
	)

	var list dispatchRateListWire
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/dispatch-rates"), nil, &list)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET dispatch-rates: %d: %s", resp.StatusCode, body)
	}
	if len(list.Items) != 2 {
		t.Fatalf("dispatch-rates returned %d scopes, want the global rate and the actor's: %s", len(list.Items), body)
	}

	byScope := map[string]dispatchRateWire{}
	for _, item := range list.Items {
		byScope[item.Scope+"/"+item.ScopeKey] = item
	}

	global, ok := byScope["global/"]
	if !ok {
		t.Fatalf("no global scope in %s", body)
	}
	if global.LimitPerWindow != 8 {
		t.Errorf("global limit_per_window = %d, want the configured 8", global.LimitPerWindow)
	}
	if global.WindowSeconds != int((6 * time.Hour).Seconds()) {
		t.Errorf("global window_seconds = %d, want 21600", global.WindowSeconds)
	}
	if global.Dispatched != 1 {
		t.Errorf("global dispatched = %d, want 1", global.Dispatched)
	}
	if global.Remaining != 7 {
		t.Errorf("global remaining = %d, want 7", global.Remaining)
	}
	if global.NextDispatchAt == "" {
		t.Error("global next_dispatch_at is empty; an operator asking 'when does the next one go' gets no answer")
	}
	if global.WindowEndsAt == "" || global.WindowAnchor == "" {
		t.Errorf("global rate carries no window: %+v", global)
	}

	actor, ok := byScope["actor/"+actorKey]
	if !ok {
		t.Fatalf("no actor scope for %q in %s", actorKey, body)
	}
	if actor.LimitPerWindow != 3 || actor.Dispatched != 1 || actor.Remaining != 2 {
		t.Errorf("actor rate = %d/window, %d dispatched, %d remaining; want 3/1/2",
			actor.LimitPerWindow, actor.Dispatched, actor.Remaining)
	}
}

// The actors surface carries the per-actor rate the same way t9's breaker
// state rides on it -- resolved by actor KEY, so every registration revision
// of one identity reports the same rate.
func TestActorSurfaceRendersItsDispatchRate(t *testing.T) {
	f := newFixture(t)
	actorID := f.insertActor("rate-limited-analyzer")
	actorKey := actorKeyOfRow(t, f, actorID)
	consumeSlots(t, f, postgres.RateRequest{Scope: postgres.RateScopeActor, ScopeKey: actorKey, Config: testRate(2)})

	var got actorWithRateWire
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors/"+actorID), nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET actor: %d: %s", resp.StatusCode, body)
	}
	if got.DispatchRate == nil {
		t.Fatalf("actor payload carries no dispatch_rate block: %s", body)
	}
	if got.DispatchRate.LimitPerWindow != 2 || got.DispatchRate.Dispatched != 1 {
		t.Errorf("dispatch_rate = %+v, want the configured 2 with 1 consumed", got.DispatchRate)
	}
	if got.DispatchRate.Scope != postgres.RateScopeActor || got.DispatchRate.ScopeKey != actorKey {
		t.Errorf("dispatch_rate names scope %q/%q, want the actor scope", got.DispatchRate.Scope, got.DispatchRate.ScopeKey)
	}
}

// An actor nobody has paced carries no rate block at all -- the same
// distinction the availability block draws between "never paused" and
// "paused, then released".
func TestAnUnpacedActorCarriesNoDispatchRate(t *testing.T) {
	f := newFixture(t)
	actorID := f.insertActor("unpaced-analyzer")

	var got actorWithRateWire
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors/"+actorID), nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET actor: %d: %s", resp.StatusCode, body)
	}
	if got.DispatchRate != nil {
		t.Errorf("an actor under no declared rate reports %+v, want no dispatch_rate block", got.DispatchRate)
	}

	var list dispatchRateListWire
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/dispatch-rates"), nil, &list)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET dispatch-rates: %d: %s", resp.StatusCode, body)
	}
	if len(list.Items) != 0 {
		t.Errorf("dispatch-rates returned %d scopes with no pacing configured, want none: %s", len(list.Items), body)
	}
}

// A row left over from a spent window reads as a fresh window, because that
// is what it is: the counter belongs to a window that has ended, and the
// surface must not report last window's consumption as this window's.
func TestASpentWindowReadsAsFreshOnceItHasRolled(t *testing.T) {
	f := newFixture(t)
	cfg := pacing.Config{Limit: 1, Window: time.Hour, Anchor: time.Now().UTC().Truncate(time.Microsecond)}
	consumeSlots(t, f, postgres.RateRequest{Scope: postgres.RateScopeGlobal, Config: cfg})

	if _, err := f.store.Pool().Exec(context.Background(), `
		UPDATE dispatch_rate_state
		SET window_started_at = window_started_at - interval '2 hours',
		    window_anchor     = window_anchor - interval '2 hours'
		WHERE namespace_id = $1`, f.nsID); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	var list dispatchRateListWire
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/dispatch-rates"), nil, &list)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET dispatch-rates: %d: %s", resp.StatusCode, body)
	}
	if len(list.Items) != 1 {
		t.Fatalf("want one scope: %s", body)
	}
	if list.Items[0].Dispatched != 0 {
		t.Errorf("dispatched = %d after the window rolled, want 0: last window's count is not this window's",
			list.Items[0].Dispatched)
	}
	if list.Items[0].Remaining != 1 {
		t.Errorf("remaining = %d after the window rolled, want the whole budget back", list.Items[0].Remaining)
	}
	if list.Items[0].LastDispatchAt == "" {
		t.Error("last_dispatch_at was dropped; the row's provenance survives a window roll")
	}
}
