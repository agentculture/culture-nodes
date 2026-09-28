package declengine

// Task t38g (#328): the two lineage-forgery findings of the qwen cortex
// review of t38c/t38e/t38d (A1, A2). Both tests drive the engine with a
// genuine marker a forger could have copied from a public artifact, and
// deliver the forged event through EventFromSignal exactly as a delivery
// would, so they pin behaviour, not wiring.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// appendRawEvent writes a signal_events row directly, as the engine's own
// emitters do -- the ingress check (kinds.CheckExternalEvent) refuses a
// reserved name through DeliverSignalEvent, and this is how a row with any
// emitter reaches Handle all the same (a stored or replayed event).
func appendRawEvent(t *testing.T, db *postgres.Store, ns, name, emitter string, payload map[string]any) postgres.SignalEvent {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	ev := postgres.SignalEvent{ID: store.NewULID(), NamespaceID: ns, Name: name, Payload: raw, Emitter: emitter}
	if _, err := db.Pool().Exec(context.Background(), `INSERT INTO signal_events(id,namespace_id,name,payload,emitter) VALUES($1,$2,$3,$4,$5)`,
		ev.ID, ev.NamespaceID, ev.Name, ev.Payload, ev.Emitter); err != nil {
		t.Fatal(err)
	}
	return ev
}

func countRows(t *testing.T, db *postgres.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Pool().QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func forgeryDecl(name, trigger, start, landing string) decl.Declaration {
	d := active(name).Declaration
	d.Trigger.Kind, d.Condition = trigger, "true"
	d.Action.With = json.RawMessage(`{"uses":"actor://test/worker@sha256:aaaaaa","input":{"text":"hello"}}`)
	d.StartNode = decl.Node{Name: start, Deadline: "none"}
	d.LandingNode = decl.Node{Name: landing, Deadline: "1h"}
	return d
}

// A1: a firing whose dispatch failed after the run was created (the ledger
// append failing, say) never reached Finish, so it opened no landing node,
// yet its run executes and its marker can be exposed and bound. A forger
// holding that copied marker used to name ANY node in the payload and have
// it honoured. Now a verified parent with no landing node records a note
// and fires nothing, whatever the payload names.
func TestPostgresVerifiedParentWithoutLandingNodeFiresNothing(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "t38g-nolanding").ID
	va := publishActive(t, db, ns, forgeryDecl("t38g-parent", "timer", "ready", "waiting"))
	vc := publishActive(t, db, ns, forgeryDecl("t38g-victim", "timer", "elsewhere", "done"))

	var marker string
	failing := true
	t.Setenv("TCA_T38G_KEY", strings.Repeat("g", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_T38G_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db}, dispatchFunc(func(_ context.Context, r DispatchRequest) (DispatchResult, error) {
		if failing {
			marker = r.Marker
			return DispatchResult{}, errors.New("ledger append failed after the run was created")
		}
		return DispatchResult{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	first := deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: first, Kind: "timer", Node: "ready"}); err == nil {
		t.Fatal("the failing dispatch did not surface")
	}
	if n := countRows(t, db, `SELECT count(*) FROM declaration_nodes WHERE namespace_id=$1`, ns); n != 0 || marker == "" {
		t.Fatalf("setup: %d landing nodes, marker %q; want none and a minted marker", n, marker)
	}
	// The run executed and its artifact was bound to the marker, which is
	// now public.
	if err := e.markers.RecordArtifact(ctx, ns, marker, "pr-exposed"); err != nil {
		t.Fatal(err)
	}
	failing = false

	forged := appendRawEvent(t, db, ns, "timer", "forger", map[string]any{"node": "elsewhere",
		"origin": map[string]any{"marker": marker, "artifact_kind": "github.pr", "artifact_id": "pr-exposed"}})
	if err := e.Handle(ctx, EventFromSignal(forged)); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2`, ns, forged.ID); n != 0 {
		t.Fatalf("the forged event naming node 'elsewhere' fired %d declarations, want 0", n)
	}
	for _, d := range []string{va.DeclarationID, vc.DeclarationID} {
		if got := outcomes(t, db, ns, forged.ID, d); len(got) != 0 {
			t.Fatalf("declaration %s evaluated the forged event: %v", d, got)
		}
	}
	var reason string
	if err := db.Pool().QueryRow(ctx, `SELECT reason FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2
		AND declaration_id='node-lifecycle' AND outcome='parent opened no landing node'`, ns, forged.ID).Scan(&reason); err != nil {
		t.Fatalf("no 'parent opened no landing node' note for the event: %v", err)
	}
	if !strings.Contains(reason, "not fired") {
		t.Fatalf("note reason %q does not say the reaction was not fired", reason)
	}
}

// A2: a human.decision marker is shown to the task's recipient, and it
// verifies for any human.decision event. A human.decision whose emitter is
// not the control plane is rejected before it can land on the open node,
// whatever its marker proves; the engine's own reaction still continues
// the lineage afterwards.
func TestPostgresReservedReactionFromOutsideTheEngineIsRejected(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "t38g-impersonate").ID
	ask := forgeryDecl("t38g-ask", "timer", "ready", "asked")
	ask.Action = decl.Action{Kind: "human.ask", With: json.RawMessage(`{"approver_ref":"group/reviewers","input":{"question":"merge?"}}`)}
	publishActive(t, db, ns, ask)
	vb := publishActive(t, db, ns, forgeryDecl("t38g-after-ask", "human.decision", "asked", "done"))

	var marker string
	t.Setenv("TCA_T38G_KEY", strings.Repeat("g", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_T38G_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db}, dispatchFunc(func(_ context.Context, r DispatchRequest) (DispatchResult, error) {
		if strings.Contains(r.Marker, ":human.decision:") {
			marker = r.Marker
		}
		return DispatchResult{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: deliver(t, db, ns), Kind: "timer", Node: "ready"}); err != nil {
		t.Fatal(err)
	}
	if marker == "" {
		t.Fatal("setup: the human.ask firing was not dispatched with a human.decision marker")
	}
	if err := e.markers.RecordArtifact(ctx, ns, marker, "task-1"); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"outcome": "approved", "origin": map[string]any{"marker": marker, "artifact_kind": "human.decision", "artifact_id": "task-1"}}

	forged := appendRawEvent(t, db, ns, "human.decision", "forger", payload)
	if err := e.Handle(ctx, EventFromSignal(forged)); err != nil {
		t.Fatal(err)
	}
	if got := outcomes(t, db, ns, forged.ID, vb.DeclarationID); len(got) != 0 {
		t.Fatalf("the forged human.decision was evaluated by the waiting declaration: %v", got)
	}
	if n := countRows(t, db, `SELECT count(*) FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2 AND outcome='reserved event rejected'`, ns, forged.ID); n != 1 {
		t.Fatalf("the forged human.decision left %d rejection records, want 1", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM declaration_nodes WHERE namespace_id=$1 AND node_name='asked' AND state='open'`, ns); n != 1 {
		t.Fatalf("the forgery closed the waiting node (open 'asked' nodes: %d)", n)
	}

	genuine := appendRawEvent(t, db, ns, "human.decision", DeclarationEngineActorID, payload)
	if err := e.Handle(ctx, EventFromSignal(genuine)); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM declaration_firings f JOIN declaration_lineage_edges e ON e.namespace_id=f.namespace_id AND e.child_firing_id=f.id
		WHERE f.namespace_id=$1 AND f.event_id=$2 AND f.declaration_id=$3`,
		ns, genuine.ID, vb.DeclarationID); n != 1 {
		t.Fatalf("the engine's own human.decision fired the waiting declaration %d times with a parent, want 1", n)
	}
}
