package worker_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	idstore "github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// The clarify-then-commit gate meets liveness routing (Qodo High, PR #326).
//
// Lane liveness picks the actor PER CLAIM: a node run whose primary lane
// goes dead between one claim and the next is rerouted to the fallback its
// registration names (liveness.go). The gate reads its briefing from
// dispatch_preflights, and a briefing is composed FOR one lane — it states
// that lane's host facts, and the acknowledgement against it is that lane's
// claim to have read them.
//
// Selecting that briefing by node run alone therefore let the lane that was
// never briefed ride the acknowledgement of the lane that was: the primary
// acknowledged, went dead, and the fallback dispatched under an
// authorization composed for another host and answered by another actor —
// the gate's whole point, bypassed by the routing that was supposed to
// rescue the run. The lookup is by node run AND actor key, so a rerouted
// lane is simply unbriefed and gets a briefing of its own.

// withGatedLanes registers BOTH lanes with the gate enabled: the node's own
// company/analyzer, naming company/fallback in its registration, and the
// fallback pointing at a second actor server. It is withLivenessLanes with
// capabilities and gate metadata on both rows, which the gate's own
// configuration doors require together (internal/preflight.CheckConfiguration,
// migration 0026's CHECK).
func withGatedLanes(t *testing.T, fallbackURL string, out *livenessLane) harnessOption {
	t.Helper()
	return func(o *worker.Options) {
		base, ok := o.Registry.(worker.StaticRegistry)
		if !ok {
			t.Fatalf("harness registry is %T, want a StaticRegistry", o.Registry)
		}
		ctx := context.Background()
		store, err := storepg.NewEngineStore(testStore, o.NamespaceID)
		if err != nil {
			t.Fatalf("NewEngineStore: %v", err)
		}
		primary, err := store.RegisterActor(ctx, storepg.RegisterActorParams{
			ActorKey: "company/analyzer", Kind: "agent", Protocol: "http",
			EndpointRef:  base["actor://company/analyzer"].URL,
			Capabilities: json.RawMessage(testHostCapabilities),
			Metadata: json.RawMessage(
				`{"preflight_gate":{"enabled":true},"fallback_actor":"company/fallback"}`),
		})
		if err != nil {
			t.Fatalf("RegisterActor(primary): %v", err)
		}
		out.primaryRowID = primary.ID

		fb, err := store.RegisterActor(ctx, storepg.RegisterActorParams{
			ActorKey: "company/fallback", Kind: "agent", Protocol: "http",
			EndpointRef:  fallbackURL,
			Capabilities: json.RawMessage(testHostCapabilities),
			Metadata:     json.RawMessage(`{"preflight_gate":{"enabled":true}}`),
		})
		if err != nil {
			t.Fatalf("RegisterActor(fallback): %v", err)
		}
		out.fallbackRowID = fb.ID

		registry, err := worker.NewDBRegistry(testStore, o.NamespaceID)
		if err != nil {
			t.Fatalf("NewDBRegistry: %v", err)
		}
		o.Registry = registry

		gateActorID := "engine-dispatch-gate-" + idstore.NewULID()
		if _, err := testStore.Pool().Exec(ctx, `
			INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
			VALUES ($1, $2, $3, 1, 'engine', 'internal')
		`, gateActorID, o.NamespaceID, gateActorID); err != nil {
			t.Fatalf("register the dispatch gate producer: %v", err)
		}
		o.DispatchGateActorID = gateActorID
		out.gateActorID = gateActorID
	}
}

func TestAFallbackLaneDoesNotDispatchOnThePrimarysAcknowledgement(t *testing.T) {
	fb := newFallbackActor(t)
	var lanes livenessLane
	h := newHarness(t, completesSynchronously, withGatedLanes(t, fb.server.URL, &lanes))

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	if _, err := h.worker.Tick(h.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	rows := preflightRows(t, h, run.ID)
	if len(rows) != 1 || rows[0].ActorKey != "company/analyzer" {
		t.Fatalf("run has %d preflight rows (%+v), want exactly 1 for company/analyzer", len(rows), rows)
	}
	primaryPreflight := rows[0]
	acknowledge(t, h, primaryPreflight)

	// ...and then the lane it was composed for stops being able to start a
	// session at all, so the next claim is routed to the fallback.
	observeNotLive(t, h, "company/analyzer", time.Now().UTC())
	releaseDeferred(t, h, run.ID, "analyze")
	tick(t, h, 3)

	if got := fb.hits.Load(); got != 0 {
		t.Errorf("the fallback lane was invoked %d times, want 0: it acknowledged nothing, and the "+
			"briefing it would have ridden states another host's facts", got)
	}
	if got := len(h.invocations()); got != 0 {
		t.Errorf("the primary lane was invoked %d times, want 0: its session cannot start", got)
	}

	// The primary's acknowledgement is still unspent — a dispatch that never
	// happened may not have consumed it — and the fallback has a briefing of
	// its own, unacknowledged, holding the work exactly as the primary's did.
	after := preflightRows(t, h, run.ID)
	if len(after) != 2 {
		t.Fatalf("run has %d preflight rows, want 2: one per lane (worker errors: %v)",
			len(after), h.workerErrors())
	}
	if after[0].ID != primaryPreflight.ID || after[0].Consumed() {
		t.Errorf("the primary's briefing %+v was consumed by a dispatch to another lane", after[0])
	}
	fallbackPreflight := after[1]
	if fallbackPreflight.ActorKey != "company/fallback" {
		t.Errorf("the second briefing is for %q, want company/fallback", fallbackPreflight.ActorKey)
	}
	if fallbackPreflight.ActorID != lanes.fallbackRowID {
		t.Errorf("the second briefing names actor row %q, want the fallback's %q",
			fallbackPreflight.ActorID, lanes.fallbackRowID)
	}
	if fallbackPreflight.Acknowledged() {
		t.Error("the fallback's own briefing reports itself acknowledged; nobody answered it")
	}

	// And the fallback dispatches once it answers for itself: the gate holds
	// the rerouted work, it does not kill it.
	acknowledge(t, h, fallbackPreflight)
	releaseDeferred(t, h, run.ID, "analyze")
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if got := fb.hits.Load(); got != 1 {
		t.Fatalf("the fallback lane was invoked %d times after acknowledging, want 1 (worker errors: %v)",
			got, h.workerErrors())
	}
	spent := preflightRows(t, h, run.ID)
	if !spent[1].Consumed() || spent[1].ConsumedByAttemptID == "" {
		t.Errorf("the fallback's briefing %+v was not spent by the dispatch it authorized", spent[1])
	}
	if spent[0].Consumed() {
		t.Error("the primary's briefing was consumed after all; only its own lane may spend it")
	}
}
