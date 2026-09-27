package declengine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// declFor builds a chain declaration like engine_postgres_test.go's decl3
// closures, but package-level so every t12 test can share it.
func declFor(name, startNode, startDeadline, landingNode, landingDeadline, triggerKind, with string) decl.Declaration {
	d := active(name).Declaration
	d.Condition = "true"
	d.Trigger.Kind = triggerKind
	d.StartNode = decl.Node{Name: startNode, Deadline: startDeadline}
	d.LandingNode = decl.Node{Name: landingNode, Deadline: landingDeadline}
	d.Action.With = json.RawMessage(with)
	return d
}

// reactEvent builds a reaction Event carrying the verified origin marker for
// prev's firing -- the same shape engine_postgres_test.go's TestPostgres
// ThreeDeclarationChain builds inline, lifted out so this file's tests share
// it. Node is left empty deliberately: acceptance criterion 2/4 exercise
// Handle deriving it from the landing node prev opened.
func reactEvent(t *testing.T, db *postgres.Store, ns, eventID string, prev postgres.DeclarationFiring, kind string, vars map[string]any) Event {
	t.Helper()
	var nonce, mac string
	if err := db.Pool().QueryRow(context.Background(), `SELECT nonce,mac FROM declaration_minted_markers WHERE namespace_id=$1 AND firing_id=$2`, ns, prev.ID).Scan(&nonce, &mac); err != nil {
		t.Fatal(err)
	}
	return Event{NamespaceID: ns, ID: eventID, Kind: kind, Variables: vars,
		Origin: OriginEvent{Marker: "cn1:" + prev.ID + ":github.pr:" + nonce + ":" + mac, ArtifactKind: "github.pr", ArtifactID: "artifact-" + prev.ID}}
}

func firingByEventDecl(t *testing.T, db *postgres.Store, ns, eventID, declarationID string) postgres.DeclarationFiring {
	t.Helper()
	var f postgres.DeclarationFiring
	if err := db.Pool().QueryRow(context.Background(), `SELECT id,declaration_version,trigger_digest,condition_digest,action_digest FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3`,
		ns, eventID, declarationID).Scan(&f.ID, &f.DeclarationVersion, &f.TriggerDigest, &f.ConditionDigest, &f.ActionDigest); err != nil {
		t.Fatalf("firing of %s for event %s: %v", declarationID, eventID, err)
	}
	return f
}

// Acceptance 1 and 2: a node's state and reason are queryable through both
// NodeByFiring and NodeStatus; its deadline expires exactly once under a
// caller-supplied (fake) clock, emitting node.expired for a declaration
// that takes it as a trigger; and a same-kind reaction that arrives after
// that close is recorded on the node-lifecycle pseudo-declaration, never
// fired.
func TestPostgresNodeExpiryExactlyOnceAndLateReactionRecorded(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-expiry").ID

	a := declFor("exp-a", "intake", "none", "waiting", "1h", "timer", `{"uses":"actor://test"}`)
	b := declFor("exp-b", "waiting", "none", "done", "1h", "timer", `{"uses":"actor://test"}`)              // late reaction vehicle
	c := declFor("exp-c", "waiting", "none", "closed-out", "1h", "node.expired", `{"uses":"actor://test"}`) // proves node.expired is a usable trigger
	va := publishActive(t, db, ns, a)
	publishActive(t, db, ns, b)
	vc := publishActive(t, db, ns, c)

	dispatcher := &chainDispatcher{rendered: map[string]string{}, vars: map[string]map[string]any{}}
	t.Setenv("TCA_EXPIRY_KEY", strings.Repeat("e", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_EXPIRY_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db}, dispatcher)
	if err != nil {
		t.Fatal(err)
	}

	first := deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: first, Kind: "timer", Node: "intake"}); err != nil {
		t.Fatal(err)
	}
	fa := firingByEventDecl(t, db, ns, first, va.DeclarationID)

	backend := PostgresBackend{db}
	node, found, err := backend.NodeByFiring(ctx, ns, fa.ID)
	if err != nil || !found || node.State != NodeStateOpen || node.ClosedReason != "" || node.Deadline.IsZero() {
		t.Fatalf("opened node=%+v found=%v err=%v", node, found, err)
	}
	// h40/c82: queryable by its own id too.
	byID, found, err := backend.NodeStatus(ctx, ns, node.ID)
	if err != nil || !found || byID != node {
		t.Fatalf("NodeStatus mismatch: %+v vs %+v (err=%v)", byID, node, err)
	}

	// A fake clock well past the 1h deadline: expiry is driven off the
	// caller-supplied now, never a real wall clock read inside this package.
	fakeNow := time.Now().UTC().Add(2 * time.Hour)
	expired, err := e.ExpireDue(ctx, ns, fakeNow, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].NodeID != node.ID || expired[0].NodeName != "waiting" {
		t.Fatalf("expired=%+v", expired)
	}
	closed, found, err := backend.NodeByFiring(ctx, ns, fa.ID)
	if err != nil || !found || closed.State != NodeStateClosed || closed.ClosedReason != NodeReasonExpired {
		t.Fatalf("closed node=%+v err=%v", closed, err)
	}
	// c's start node is "waiting" and its trigger is node.expired: the
	// emission ExpireDue produced must have run back through Handle and
	// fired it.
	cVersion := firingByEventDecl(t, db, ns, expired[0].EventID, vc.DeclarationID)
	if cVersion.ID == "" {
		t.Fatal("node.expired did not fire the declaration that takes it as a trigger")
	}

	// Exactly once: calling ExpireDue again over the same due window never
	// re-closes THIS node (c's own landing node, opened only moments ago by
	// real wall-clock time, may legitimately still be due under the same
	// fake "now" and is not what this assertion is about).
	again, err := e.ExpireDue(ctx, ns, fakeNow, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range again {
		if ev.NodeID == node.ID {
			t.Fatalf("second ExpireDue re-closed the same node: %+v", ev)
		}
	}
	var expiredEventCount int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM signal_events WHERE namespace_id=$1 AND name='node.expired' AND payload->>'node_id'=$2`, ns, node.ID).Scan(&expiredEventCount); err != nil {
		t.Fatal(err)
	}
	if expiredEventCount != 1 {
		t.Fatalf("node.expired emitted %d times, want exactly 1", expiredEventCount)
	}

	// c82: a late reaction against the now-closed node is recorded, never
	// fired -- b, whose start node is "waiting" and whose trigger kind is
	// "timer", would have matched had the node still been open.
	late := deliver(t, db, ns)
	if err := e.Handle(ctx, reactEvent(t, db, ns, late, fa, "timer", nil)); err != nil {
		t.Fatal(err)
	}
	var bFired int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2`, ns, late).Scan(&bFired); err != nil {
		t.Fatal(err)
	}
	if bFired != 0 {
		t.Fatalf("late reaction fired %d declarations, want 0", bFired)
	}
	var note string
	if err := db.Pool().QueryRow(ctx, `SELECT reason FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3 AND outcome=$4`,
		ns, late, nodeLifecycleDeclarationID, OutcomeNodeClosed).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "waiting") || !strings.Contains(note, NodeReasonExpired) {
		t.Fatalf("late reaction note = %q", note)
	}
}

// Acceptance 1 and 4: a node closes 'consumed' when the reaction that was
// meant to consume it fires, and a node whose reacting declaration was
// upgraded (its trigger changed so it no longer matches) out from under an
// in-flight node closes 'orphaned by upgrade', with a record naming both
// the version that reacted when the node opened and the version active now.
func TestPostgresNodeConsumedAndOrphanedByUpgrade(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-orphan").ID

	a := declFor("orph-a", "intake", "none", "pr-open", "1h", "timer", `{"uses":"actor://test"}`)
	b := declFor("orph-b", "pr-open", "none", "done", "1h", "timer", `{"uses":"actor://test"}`)
	va := publishActive(t, db, ns, a)
	vb := publishActive(t, db, ns, b)

	dispatcher := &chainDispatcher{rendered: map[string]string{}, vars: map[string]map[string]any{}}
	t.Setenv("TCA_ORPHAN_KEY", strings.Repeat("o", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_ORPHAN_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db}, dispatcher)
	if err != nil {
		t.Fatal(err)
	}
	backend := PostgresBackend{db}

	// --- consumed ---
	first := deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: first, Kind: "timer", Node: "intake"}); err != nil {
		t.Fatal(err)
	}
	fa1 := firingByEventDecl(t, db, ns, first, va.DeclarationID)
	node1, found, err := backend.NodeByFiring(ctx, ns, fa1.ID)
	if err != nil || !found || node1.State != NodeStateOpen {
		t.Fatalf("node1=%+v found=%v err=%v", node1, found, err)
	}
	if node1.ReactorDeclarationID != vb.DeclarationID || node1.ReactorDeclarationVersion != vb.ID {
		t.Fatalf("node1 reactor=%+v, want %s/%s", node1, vb.DeclarationID, vb.ID)
	}
	react1 := deliver(t, db, ns)
	if err := e.Handle(ctx, reactEvent(t, db, ns, react1, fa1, "timer", nil)); err != nil {
		t.Fatal(err)
	}
	consumed, found, err := backend.NodeByFiring(ctx, ns, fa1.ID)
	if err != nil || !found || consumed.State != NodeStateClosed || consumed.ClosedReason != NodeReasonConsumed {
		t.Fatalf("consumed node=%+v err=%v", consumed, err)
	}

	// --- orphaned by upgrade ---
	second := deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: second, Kind: "timer", Node: "intake"}); err != nil {
		t.Fatal(err)
	}
	fa2 := firingByEventDecl(t, db, ns, second, va.DeclarationID)
	node2, found, err := backend.NodeByFiring(ctx, ns, fa2.ID)
	if err != nil || !found || node2.State != NodeStateOpen || node2.ReactorDeclarationID != vb.DeclarationID {
		t.Fatalf("node2=%+v found=%v err=%v", node2, found, err)
	}

	// Upgrade b: same declaration, a new version whose start node no
	// longer names "pr-open" -- it can never again react to node2.
	upgraded := declFor("orph-b", "elsewhere", "none", "done", "1h", "timer", `{"uses":"actor://test"}`)
	vb2 := publishActive(t, db, ns, upgraded)
	if vb2.DeclarationID != vb.DeclarationID || vb2.ID == vb.ID {
		t.Fatalf("upgrade did not land a new version of the same declaration: %+v vs %+v", vb2, vb)
	}

	react2 := deliver(t, db, ns)
	if err := e.Handle(ctx, reactEvent(t, db, ns, react2, fa2, "timer", nil)); err != nil {
		t.Fatal(err)
	}
	orphaned, found, err := backend.NodeStatus(ctx, ns, node2.ID)
	if err != nil || !found || orphaned.State != NodeStateClosed || orphaned.ClosedReason != NodeReasonOrphanedByUpgrade {
		t.Fatalf("orphaned node=%+v err=%v", orphaned, err)
	}
	var reason string
	if err := db.Pool().QueryRow(ctx, `SELECT reason FROM declaration_evaluations WHERE namespace_id=$1 AND declaration_id=$2 AND outcome=$3 ORDER BY created_at DESC LIMIT 1`,
		ns, nodeLifecycleDeclarationID, OutcomeNodeOrphan).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, vb.ID) || !strings.Contains(reason, vb2.ID) {
		t.Fatalf("orphan record %q does not name both versions %s/%s", reason, vb.ID, vb2.ID)
	}

	// No firing was ever created for the orphaned reaction.
	var fired int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2`, ns, react2).Scan(&fired); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Fatalf("orphaned reaction fired %d declarations, want 0", fired)
	}
}

// actionResultFixture drives one declaration's action through the real
// worker/actor path (mirroring engine_postgres_test.go's workerFiring) with
// an actor endpoint the caller controls, so its HTTP response's status and
// body decide the §13.5 class the completed run's failed attempt carries.
func actionResultFixture(t *testing.T, handler http.HandlerFunc) (*postgres.Store, *Engine, string, postgres.DeclarationFiring, string) {
	t.Helper()
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-action-result")
	producer := store.NewULID()
	if _, err := db.Pool().Exec(ctx, `INSERT INTO actors(id,namespace_id,actor_key,revision,kind,protocol) VALUES($1,$2,'engine/declarations',1,'validator','internal')`, producer, ns.ID); err != nil {
		t.Fatal(err)
	}

	d := declFor("act-f", "ready", "none", "waiting", "1h", "timer", `{"uses":"actor://test/worker@sha256:aaaaaa","input":{"text":"hello"}}`)
	version := publishActive(t, db, ns.ID, d)
	eventID := deliver(t, db, ns.ID)

	t.Setenv("TCA_ACTION_KEY", strings.Repeat("f", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_ACTION_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db}, WorkerDispatcher{Store: db, ProducerActorID: producer})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Handle(ctx, Event{NamespaceID: ns.ID, ID: eventID, Kind: "timer", Node: "ready"}); err != nil {
		t.Fatal(err)
	}
	firing := firingByEventDecl(t, db, ns.ID, eventID, version.DeclarationID)

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	graph, err := postgres.NewEngine(db, ns.ID)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := actors.NewTokenSigner([]byte(strings.Repeat("s", 32)))
	if err != nil {
		t.Fatal(err)
	}
	wk, err := worker.New(db, graph, worker.Options{NamespaceID: ns.ID, WorkerID: "tca-action-" + t.Name(), Signer: signer, CallbackBaseURL: srv.URL,
		Registry: worker.StaticRegistry{"actor://test/worker": actors.Endpoint{URL: srv.URL}}, OnError: func(err error) { t.Error(err) }})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		var state string
		if err := db.Pool().QueryRow(ctx, `SELECT status FROM runs WHERE namespace_id=$1 AND id=$2`, ns.ID, firing.ID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "failed" || state == "completed" || state == "cancelled" {
			break
		}
		if _, err := wk.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var runStatus string
	if err := db.Pool().QueryRow(ctx, `SELECT status FROM runs WHERE namespace_id=$1 AND id=$2`, ns.ID, firing.ID).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "failed" {
		t.Fatalf("run status = %q, want failed", runStatus)
	}
	return db, e, ns.ID, firing, version.DeclarationID
}

// Acceptance 3: a non-success actor status produces exactly one action.*
// trigger linked to the node the failed firing opened, mapped from the
// internal/actors §13.5 error class the failed attempt carries.
func TestPostgresActionResultTriggersExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{
			name: "rejected", want: ActionTriggerRejected,
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "bad input", http.StatusBadRequest)
			},
		},
		{
			name: "capacity_exhausted", want: ActionTriggerCapacityExhausted,
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "quota", "class": "capacity_exhausted"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, e, ns, firing, declarationID := actionResultFixture(t, tc.handler)
			ctx := context.Background()

			node, found, err := (PostgresBackend{db}).NodeByFiring(ctx, ns, firing.ID)
			if err != nil || !found {
				t.Fatalf("landing node not opened: found=%v err=%v", found, err)
			}

			// A declaration whose trigger is the exact action.* kind this
			// class maps to, so firing it back through Handle is provable.
			reactor := declFor("act-reactor-"+strings.ReplaceAll(tc.name, "_", "-"), node.Name, "none", "handled", "1h", tc.want, `{"uses":"actor://test"}`)
			publishActive(t, db, ns, reactor)

			results, err := e.EmitActionResults(ctx, ns, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].Trigger != tc.want || results[0].NodeName != node.Name || results[0].FiringID != firing.ID {
				t.Fatalf("results=%+v want trigger=%s node=%s firing=%s", results, tc.want, node.Name, firing.ID)
			}
			var reactorFired int
			if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2`, ns, results[0].EventID).Scan(&reactorFired); err != nil {
				t.Fatal(err)
			}
			if reactorFired != 1 {
				t.Fatalf("action.* event did not fire its reactor: %d firings", reactorFired)
			}

			// Exactly once: emitting again produces nothing further, and
			// exactly one action.* signal event exists for this run.
			again, err := e.EmitActionResults(ctx, ns, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(again) != 0 {
				t.Fatalf("second EmitActionResults re-emitted: %+v", again)
			}
			var count int
			if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM signal_events WHERE namespace_id=$1 AND run_id=$2 AND name LIKE 'action.%'`, ns, firing.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("action.* emitted %d times, want exactly 1", count)
			}
			_ = declarationID
		})
	}
}
