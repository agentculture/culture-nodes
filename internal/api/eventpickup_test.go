package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
)

// Event-route pickup end to end (issue #43 task t21; parallel-tokens design
// §6.1, decisions D9/D10/D13). These are the design's T22–T27 rows, driven
// through the real HTTP delivery route against a real run, because what is
// under test is a delivery transaction creating engine state — a fixture that
// hand-wrote the routes would prove nothing about CreateRun materializing
// them.

// pickupFixture publishes an event-pickup workflow, creates a run of it, and
// hands back the run plus the event-auth-configured server.
func pickupFixture(t *testing.T, workflow string) (*fixture, apipkg.RunOut) {
	t.Helper()
	f := newFixtureWithEventAuth(t, eventTokenSecret)

	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(readFixtureWorkflow(t, workflow))}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)
	return f, run
}

func activeRouteCount(t *testing.T, f *fixture, runID string) int {
	t.Helper()
	var n int
	if err := f.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM event_routes WHERE run_id = $1 AND status = 'active'`, runID).Scan(&n); err != nil {
		t.Fatalf("count active routes: %v", err)
	}
	return n
}

func runEventTypeCount(t *testing.T, f *fixture, runID, eventType string) int {
	t.Helper()
	var n int
	if err := f.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE aggregate_id = $1 AND event_type = $2`, runID, eventType).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", eventType, err)
	}
	return n
}

// deliver posts one event and returns the delivery result.
func deliver(t *testing.T, f *fixture, name string, payload json.RawMessage) apipkg.EventDeliveryOut {
	t.Helper()
	var out apipkg.EventDeliveryOut
	resp, body := postEvent(t, f, eventTokenSecret, map[string]any{
		"name": name, "payload": payload, "emitter": "test",
	}, &out)
	requireStatus(t, resp, body, http.StatusCreated)
	return out
}

// TestCreateRunMaterializesEventRoutes: a committed run always carries
// exactly the routes its pinned definition declares, so there is no window in
// which a run exists but an event delivered to it finds nothing to match.
func TestCreateRunMaterializesEventRoutes(t *testing.T) {
	f, run := pickupFixture(t, "event-pickup.workflow.yaml")
	if got := activeRouteCount(t, f, run.ID); got != 2 {
		t.Fatalf("active routes after CreateRun = %d, want 2 (both onEvent edges)", got)
	}
}

// TestEventDeliveryPicksUpAtAnyNodeAndSplits is design tests T22 and T23 in
// one: one delivery creates a token, a ready node run, and a work item at
// EACH matching route's target — which is what "any node can pick one up and
// continue from it, including splits" means operationally.
func TestEventDeliveryPicksUpAtAnyNodeAndSplits(t *testing.T) {
	f, run := pickupFixture(t, "event-pickup.workflow.yaml")

	out := deliver(t, f, "review-requested", json.RawMessage(`{"severity":"high"}`))
	if len(out.PickedUp) != 2 {
		t.Fatalf("picked_up = %+v, want both routes", out.PickedUp)
	}

	targets := map[string]apipkg.EventPickupOut{}
	for _, p := range out.PickedUp {
		if !p.Admitted {
			t.Fatalf("route %s (%s) was refused: %s %s", p.RouteID, p.NodeID, p.Refusal, p.Detail)
		}
		if p.TokenID == "" || p.NodeRunID == "" {
			t.Errorf("admitted pickup %+v created no token or node run", p)
		}
		targets[p.NodeID] = p
	}
	if _, ok := targets["notify"]; !ok {
		t.Error("the unguarded route did not pick up")
	}
	if _, ok := targets["escalate"]; !ok {
		t.Error("the guarded route did not pick up on a high-severity event")
	}

	// Each pickup is claimable work, not a completed anything: delivery never
	// completes a node run, it only makes work claimable.
	view := getRunView(t, f, run.ID)
	ready := 0
	for _, nr := range view.NodeRuns {
		if nr.NodeID == "notify" || nr.NodeID == "escalate" {
			if nr.State != string(engine.NodeRunReady) {
				t.Errorf("pickup node run %s status = %q, want ready", nr.NodeID, nr.State)
			}
			ready++
		}
	}
	if ready != 2 {
		t.Errorf("pickup node runs = %d, want 2", ready)
	}

	// A worker really can claim one — the point of creating the work item.
	f.release(f.claim("pickup-worker", targets["notify"].NodeRunID).ID)

	if got := runEventTypeCount(t, f, run.ID, engine.TypeEventPickedUp); got != 2 {
		t.Errorf("event.picked-up events = %d, want 2", got)
	}
	// Routes are multi-fire (design D10): a match does not retire them.
	if got := activeRouteCount(t, f, run.ID); got != 2 {
		t.Errorf("active routes after a delivery = %d, want 2 — routes are multi-fire", got)
	}
}

// TestPickupTokensRenderHonestly is review finding D4, made executable. A
// pickup token is a ROOT — nothing in this run handed it control — but it is
// an EXPLAINED root: it names the fact that created it, so the run-detail
// surface can render it without either inventing a parent or showing an
// unaccountable orphan.
func TestPickupTokensRenderHonestly(t *testing.T) {
	f, run := pickupFixture(t, "event-pickup.workflow.yaml")
	out := deliver(t, f, "review-requested", json.RawMessage(`{"severity":"low"}`))
	var pickup apipkg.EventPickupOut
	for _, p := range out.PickedUp {
		if p.Admitted {
			pickup = p
		}
	}
	if pickup.TokenID == "" {
		t.Fatalf("picked_up = %+v, want one admitted pickup", out.PickedUp)
	}

	view := getRunView(t, f, run.ID)
	var found bool
	for _, tok := range view.Tokens {
		if tok.ID != pickup.TokenID {
			continue
		}
		found = true
		if tok.ParentTokenID != "" {
			t.Errorf("pickup token claims parent %q; nothing in this run handed it control", tok.ParentTokenID)
		}
		if tok.OriginEventID != out.Event.ID {
			t.Errorf("pickup token origin_event_id = %q, want the delivered fact %s", tok.OriginEventID, out.Event.ID)
		}
	}
	if !found {
		t.Fatalf("the pickup token %s is not on the run detail surface at all", pickup.TokenID)
	}

	// Every other token of the run is unaffected: origin_event_id is empty
	// exactly where there was no event.
	for _, tok := range view.Tokens {
		if tok.ID != pickup.TokenID && tok.OriginEventID != "" {
			t.Errorf("token %s (node %s) carries origin_event_id %q but no event created it",
				tok.ID, tok.NodeID, tok.OriginEventID)
		}
	}
}

// TestEventEdgeGuardFiltersPickup is design test T26: a route whose guard
// declines creates nothing, and says so.
func TestEventEdgeGuardFiltersPickup(t *testing.T) {
	f, run := pickupFixture(t, "event-pickup.workflow.yaml")

	out := deliver(t, f, "review-requested", json.RawMessage(`{"severity":"low"}`))
	admitted, refused := 0, 0
	for _, p := range out.PickedUp {
		if p.Admitted {
			admitted++
			continue
		}
		refused++
		if p.NodeID != "escalate" {
			t.Errorf("refused route targets %q, want escalate", p.NodeID)
		}
		if p.Refusal != engine.PickupRefusedGuard {
			t.Errorf("refusal = %q, want %q", p.Refusal, engine.PickupRefusedGuard)
		}
	}
	if admitted != 1 || refused != 1 {
		t.Fatalf("picked_up = %+v, want one admitted and one guard-declined", out.PickedUp)
	}
	if got := runStatus(t, f, run.ID); got != "running" {
		t.Errorf("run status after a declined guard = %q, want running", got)
	}
}

// TestPickupAtTheParallelTokenCapIsRefusedNotFatal is design test T24 and
// decision D13: an external event arriving while a run is at its cap must not
// fail a healthy run. The pickup is skipped, the refusal is recorded, the
// fact stays appended, and the run carries on.
func TestPickupAtTheParallelTokenCapIsRefusedNotFatal(t *testing.T) {
	f, run := pickupFixture(t, "event-pickup-capped.workflow.yaml")

	// maxParallelTokens is 1 and the entry token is active, so there is no
	// headroom for a second token.
	out := deliver(t, f, "review-requested", json.RawMessage(`{}`))
	if len(out.PickedUp) != 1 {
		t.Fatalf("picked_up = %+v, want the one route", out.PickedUp)
	}
	got := out.PickedUp[0]
	if got.Admitted {
		t.Fatal("a pickup was admitted past maxParallelTokens")
	}
	if got.Refusal != string(engine.BoundParallelTokens) {
		t.Errorf("refusal = %q, want %q", got.Refusal, engine.BoundParallelTokens)
	}

	// The fact is still a fact — refusal is not rejection of the event.
	if out.Event.ID == "" {
		t.Error("a refused pickup dropped the event fact")
	}
	if runStatus(t, f, run.ID) != "running" {
		t.Errorf("run status = %q, want running: an external event must not fail a healthy run (design D13)", runStatus(t, f, run.ID))
	}
	if n := runEventTypeCount(t, f, run.ID, engine.TypeEventPickupRefused); n != 1 {
		t.Errorf("event.pickup-refused events = %d, want 1 — the refusal is the only trace an operator gets", n)
	}
	if n := runEventTypeCount(t, f, run.ID, engine.TypeRunFailed); n != 0 {
		t.Errorf("the run recorded %d failures; a refused pickup is not a run failure", n)
	}
}

// TestMultiFireRouteIsBoundedByVisits is design test T25: a standing route
// fires as often as events arrive, and what stops a runaway is §9.7, not a
// one-shot route.
func TestMultiFireRouteIsBoundedByVisits(t *testing.T) {
	f, run := pickupFixture(t, "event-pickup.workflow.yaml")

	// maxVisitsPerNode is 4 in this fixture; complete nothing, just keep
	// delivering. Each delivery adds one token at `notify` (the guard keeps
	// `escalate` out of it), so the parallel-token cap of 8 is not the first
	// thing hit.
	var refusals int
	for i := 0; i < 6; i++ {
		out := deliver(t, f, "review-requested", json.RawMessage(`{"severity":"low"}`))
		for _, p := range out.PickedUp {
			if p.NodeID == "notify" && !p.Admitted {
				refusals++
				if p.Refusal != string(engine.BoundVisits) && p.Refusal != string(engine.BoundParallelTokens) {
					t.Errorf("refusal %d = %q, want a §9.7 bound", i, p.Refusal)
				}
			}
		}
	}
	if refusals == 0 {
		t.Fatal("six deliveries into a 4-visit node produced no bound refusal; the route is unbounded")
	}
	if runStatus(t, f, run.ID) != "running" {
		t.Errorf("run status = %q, want running", runStatus(t, f, run.ID))
	}
}

// TestRunCancellationRetiresEventRoutes is half of design test T27: a dead
// run stops observing, and a post-terminal delivery matches nothing. A route
// left active would let a delivery create claimable work inside a cancelled
// run — the re-dispatch zombie issue #19 closed for every other kind of
// waiting state.
func TestRunCancellationRetiresEventRoutes(t *testing.T) {
	f, run := pickupFixture(t, "event-pickup.workflow.yaml")

	var cancelled apipkg.RunOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/cancel"), nil, &cancelled)
	requireStatus(t, resp, body, http.StatusOK)

	if got := activeRouteCount(t, f, run.ID); got != 0 {
		t.Fatalf("active routes after cancel = %d, want 0", got)
	}
	out := deliver(t, f, "review-requested", json.RawMessage(`{"severity":"high"}`))
	if len(out.PickedUp) != 0 {
		t.Errorf("a cancelled run picked up %+v", out.PickedUp)
	}
}

// TestRunCompletionRetiresEventRoutes is T27's other half, and the one that
// does not come free from the cancel REAP: a run that finishes normally must
// also stop observing.
func TestRunCompletionRetiresEventRoutes(t *testing.T) {
	f, run := pickupFixture(t, "event-pickup.workflow.yaml")
	ctx := context.Background()

	entry := getRunView(t, f, run.ID).NodeRuns[0].ID
	result := f.completeThroughClaim(ctx, entry, "completed", `{"done":true}`)
	if result.RunState != engine.RunCompleted {
		t.Fatalf("run state = %q, want completed (diagnostic %q)", result.RunState, result.Diagnostic)
	}

	if got := activeRouteCount(t, f, run.ID); got != 0 {
		t.Fatalf("active routes after the run completed = %d, want 0", got)
	}
	out := deliver(t, f, "review-requested", json.RawMessage(`{"severity":"high"}`))
	if len(out.PickedUp) != 0 {
		t.Errorf("a completed run picked up %+v", out.PickedUp)
	}
}
