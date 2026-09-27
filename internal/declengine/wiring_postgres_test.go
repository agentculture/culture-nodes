package declengine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// Task t38 (#328): the wiring obligations t9-t18 left, proven against
// PostgreSQL. See deliver.go for the wiring itself.

func firingsFor(t *testing.T, db *postgres.Store, ns, eventID string) int {
	t.Helper()
	var n int
	if err := db.Pool().QueryRow(context.Background(), `SELECT count(*) FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2`, ns, eventID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func terminalOutcome(t *testing.T, db *postgres.Store, ns, eventID, declarationID string) (outcome, reason string) {
	t.Helper()
	if err := db.Pool().QueryRow(context.Background(), `SELECT outcome,reason FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3 ORDER BY created_at DESC,id DESC LIMIT 1`,
		ns, eventID, declarationID).Scan(&outcome, &reason); err != nil {
		t.Fatalf("terminal outcome of %s for %s: %v", declarationID, eventID, err)
	}
	return outcome, reason
}

// Acceptance 4 (t10 x t12): with a per-subject cap of 1, a second event on
// the same subject is deferred; every closing path of the first node --
// expired, consumed, orphaned by upgrade -- makes the deferred event fire,
// because the close drains the opening declaration's queue once committed.
func TestPostgresDrainOnCloseReplaysDeferredSubject(t *testing.T) {
	for _, path := range []string{"expired", "consumed", "orphaned"} {
		t.Run(path, func(t *testing.T) {
			db := pgtest.RequireStore(t, markerTestStore)
			ctx := context.Background()
			ns := pgtest.MustNamespace(t, db, "tca-drain-close").ID

			a := declFor("dc-a", "intake", "none", "waiting", "1h", "timer", `{"uses":"actor://test"}`)
			a.Trigger.MaxConcurrentSubject = 1
			b := declFor("dc-b", "waiting", "none", "done", "none", "timer", `{"uses":"actor://test"}`)
			va := publishActive(t, db, ns, a)
			vb := publishActive(t, db, ns, b)

			t.Setenv("TCA_DRAIN_CLOSE_KEY", strings.Repeat("d", 32))
			e, err := New(Config{MarkerKeyEnv: "TCA_DRAIN_CLOSE_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db},
				&chainDispatcher{rendered: map[string]string{}, vars: map[string]map[string]any{}})
			if err != nil {
				t.Fatal(err)
			}
			fresh := func(id string) Event {
				return Event{NamespaceID: ns, ID: id, Kind: "timer", Node: "intake", Subject: "ISSUE-1"}
			}
			first, second := deliver(t, db, ns), deliver(t, db, ns)
			if err := e.Handle(ctx, fresh(first)); err != nil {
				t.Fatal(err)
			}
			if err := e.Handle(ctx, fresh(second)); err != nil {
				t.Fatal(err)
			}
			if got, _ := terminalOutcome(t, db, ns, second, va.DeclarationID); got != OutcomeDeferred {
				t.Fatalf("second event on the subject = %q, want %q", got, OutcomeDeferred)
			}
			if n := firingsFor(t, db, ns, second); n != 0 {
				t.Fatalf("deferred event claimed %d firings", n)
			}
			fa := firingByEventDecl(t, db, ns, first, va.DeclarationID)

			switch path {
			case "expired":
				if _, err := e.ExpireDue(ctx, ns, time.Now().UTC().Add(2*time.Hour), 10); err != nil {
					t.Fatal(err)
				}
			case "consumed":
				react := deliver(t, db, ns)
				if err := e.Handle(ctx, reactEvent(t, db, ns, react, fa, "timer", nil)); err != nil {
					t.Fatal(err)
				}
			case "orphaned":
				// b reacted when the node opened; upgrading it so it no
				// longer starts on "waiting" orphans the node on the next
				// reaction.
				publishActive(t, db, ns, declFor("dc-b", "elsewhere", "none", "done", "none", "timer", `{"uses":"actor://test"}`))
				react := deliver(t, db, ns)
				if err := e.Handle(ctx, reactEvent(t, db, ns, react, fa, "timer", nil)); err != nil {
					t.Fatal(err)
				}
			}
			node, found, err := (PostgresBackend{db}).NodeByFiring(ctx, ns, fa.ID)
			wantReason := map[string]string{"expired": NodeReasonExpired, "consumed": NodeReasonConsumed, "orphaned": NodeReasonOrphanedByUpgrade}[path]
			if err != nil || !found || node.State != NodeStateClosed || node.ClosedReason != wantReason {
				t.Fatalf("first node=%+v err=%v, want closed/%s", node, err, wantReason)
			}
			if node.OpeningDeclarationID != va.DeclarationID {
				t.Fatalf("node's opening declaration = %q, want %q", node.OpeningDeclarationID, va.DeclarationID)
			}
			if got, _ := terminalOutcome(t, db, ns, second, va.DeclarationID); got != OutcomeFired {
				t.Fatalf("after the %s close the deferred event's outcome = %q, want %q", path, got, OutcomeFired)
			}
			if n := firingsFor(t, db, ns, second); n != 1 {
				t.Fatalf("deferred event fired %d times after the close, want 1", n)
			}
			var queued int
			if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_subject_deferrals WHERE namespace_id=$1`, ns).Scan(&queued); err != nil || queued != 0 {
				t.Fatalf("deferrals left=%d err=%v", queued, err)
			}
			_ = vb
		})
	}
}

// Acceptance 5 (t27 remainder): WorkerDispatcher refuses an artifact-creating,
// marker-carrying dispatch to an actor whose newest registration does not
// advertise {"stamping": {"marker": "cn1"}}, with a visible evaluation naming
// actor and revision and nothing queued; once a revision advertises it, the
// same declaration dispatches.
func TestPostgresWorkerDispatcherRefusesAnActorThatDoesNotStamp(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-stamping").ID
	producer := store.NewULID()
	if _, err := db.Pool().Exec(ctx, `INSERT INTO actors(id,namespace_id,actor_key,revision,kind,protocol) VALUES($1,$2,'engine/declarations',1,'engine','internal')`, producer, ns); err != nil {
		t.Fatal(err)
	}
	register := func(revision int, caps string) {
		t.Helper()
		if _, err := db.Pool().Exec(ctx, `INSERT INTO actors(id,namespace_id,actor_key,revision,kind,protocol,capabilities) VALUES($1,$2,'test/bridge',$3,'agent','http',$4::jsonb)`,
			store.NewULID(), ns, revision, caps); err != nil {
			t.Fatal(err)
		}
	}
	// Revision 1 advertised stamping; revision 2 -- the current one -- does
	// not. The refusal must read the current revision, and name it.
	register(1, `{"stamping":{"marker":"cn1","version":1}}`)
	register(2, `{"preflight":{"enabled":true}}`)
	d := declFor("stamp-a", "intake", "none", "waiting", "1h", "timer", `{"uses":"actor://test/bridge@sha256:bbbbbb","input":{"text":"x"}}`)
	v := publishActive(t, db, ns, d)
	t.Setenv("TCA_STAMPING_KEY", strings.Repeat("s", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_STAMPING_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db}, WorkerDispatcher{Store: db, ProducerActorID: producer})
	if err != nil {
		t.Fatal(err)
	}

	refused := deliver(t, db, ns)
	err = e.Handle(ctx, Event{NamespaceID: ns, ID: refused, Kind: "timer", Node: "intake"})
	var refusal *StampingRefusal
	if !errors.As(err, &refusal) || refusal.ActorKey != "test/bridge" || refusal.Revision != 2 {
		t.Fatalf("Handle err = %v, want a StampingRefusal for test/bridge revision 2", err)
	}
	outcome, reason := terminalOutcome(t, db, ns, refused, v.DeclarationID)
	if outcome != OutcomeStampingRefused || !strings.Contains(reason, `"test/bridge" revision 2`) {
		t.Fatalf("recorded evaluation = %q / %q, want %q naming actor and revision", outcome, reason, OutcomeStampingRefused)
	}
	firing := firingByEventDecl(t, db, ns, refused, v.DeclarationID)
	var runs int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM runs WHERE namespace_id=$1 AND id=$2`, ns, firing.ID).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("a refused dispatch queued %d runs (err=%v)", runs, err)
	}

	// A new revision that advertises stamping: the next event dispatches.
	register(3, `{"stamping":{"marker":"cn1","version":1}}`)
	accepted := deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: accepted, Kind: "timer", Node: "intake"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := terminalOutcome(t, db, ns, accepted, v.DeclarationID); got != OutcomeFired {
		t.Fatalf("stamping actor: outcome %q, want %q", got, OutcomeFired)
	}
	ok := firingByEventDecl(t, db, ns, accepted, v.DeclarationID)
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM runs WHERE namespace_id=$1 AND id=$2`, ns, ok.ID).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("stamping actor: %d runs for the firing (err=%v), want 1", runs, err)
	}
}

// Acceptance 6: a producer identity missing from the actors table fails the
// FIRST firing loudly -- an error from Handle and a dispatch-failed record
// naming the register-actor.sh hand-turn -- and queues nothing.
func TestPostgresUnregisteredProducerFailsTheFirstFiringLoudly(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-producer").ID
	if _, err := db.Pool().Exec(ctx, `INSERT INTO actors(id,namespace_id,actor_key,revision,kind,protocol,capabilities) VALUES($1,$2,'test/bridge',1,'agent','http','{"stamping":{"marker":"cn1"}}')`, store.NewULID(), ns); err != nil {
		t.Fatal(err)
	}
	v := publishActive(t, db, ns, declFor("prod-a", "intake", "none", "waiting", "1h", "timer", `{"uses":"actor://test/bridge"}`))
	t.Setenv("TCA_PRODUCER_KEY", strings.Repeat("p", 32))
	missing := "engine_unregistered_" + store.NewULID()
	e, err := New(Config{MarkerKeyEnv: "TCA_PRODUCER_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db}, WorkerDispatcher{Store: db, ProducerActorID: missing})
	if err != nil {
		t.Fatal(err)
	}
	eventID := deliver(t, db, ns)
	err = e.Handle(ctx, Event{NamespaceID: ns, ID: eventID, Kind: "timer", Node: "intake"})
	if !errors.Is(err, ErrProducerNotRegistered) {
		t.Fatalf("Handle err = %v, want ErrProducerNotRegistered", err)
	}
	outcome, reason := terminalOutcome(t, db, ns, eventID, v.DeclarationID)
	if outcome != OutcomeDispatchFailed || !strings.Contains(reason, "register-actor.sh --engine "+missing) {
		t.Fatalf("recorded evaluation = %q / %q, want a dispatch failure naming the registration hand-turn", outcome, reason)
	}
	var runs int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM runs WHERE namespace_id=$1`, ns).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("an unregistered producer still queued %d runs (err=%v)", runs, err)
	}
}

// originPayload renders ev's origin the way a delivered event carries it
// (EventFromSignal's "origin" payload key).
func originPayload(t *testing.T, ev Event) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"origin": map[string]string{
		"marker": ev.Origin.Marker, "artifact_kind": ev.Origin.ArtifactKind, "artifact_id": ev.Origin.ArtifactID}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Acceptances 1 and 2, the freeze half: in 'before' the router records
// nothing for an ordinary event -- not even a marker rejection -- but stores
// a reaction that targets a frozen node; a forward flip whose own replay
// never ran (the crash between flip and replay) is recovered by the
// scheduler's Driver, and so is a replay that died after thawing.
func TestPostgresRouterBeforeStoresFrozenAndDriverReplays(t *testing.T) {
	for _, crash := range []string{"before thaw", "after thaw"} {
		t.Run(crash, func(t *testing.T) {
			db := pgtest.RequireStore(t, markerTestStore)
			ctx := context.Background()
			ns := pgtest.MustNamespace(t, db, "tca-router-freeze").ID
			va := publishActive(t, db, ns, declFor("rf-a", "intake", "none", "waiting", "1h", "timer", `{"uses":"actor://test"}`))
			vb := publishActive(t, db, ns, declFor("rf-b", "waiting", "none", "done", "none", "timer", `{"uses":"actor://test"}`))

			sw := PostgresSwitchStore{Store: db}
			fb := PostgresBackend{db}
			t.Setenv("TCA_ROUTER_KEY", strings.Repeat("q", 32))
			e, err := New(Config{MarkerKeyEnv: "TCA_ROUTER_KEY"}, fb, PostgresMarkerStore{db},
				ShadowGate{Switch: sw, Underlying: &chainDispatcher{rendered: map[string]string{}, vars: map[string]map[string]any{}}})
			if err != nil {
				t.Fatal(err)
			}
			router := Router{Engine: e, Switch: sw}
			deliverVia := func(payload json.RawMessage) postgres.SignalDelivery {
				t.Helper()
				d, err := db.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{NamespaceID: ns, Name: "timer", Payload: payload, Emitter: "test", Declarations: router})
				if err != nil || d.DeclarationErr != nil {
					t.Fatalf("deliver: err=%v declarationErr=%v", err, d.DeclarationErr)
				}
				return d
			}
			if _, err := sw.Flip(ctx, ns, ModeAfter, "human:ops", "start"); err != nil {
				t.Fatal(err)
			}
			first := deliverVia(json.RawMessage(`{"node":"intake"}`))
			fa := firingByEventDecl(t, db, ns, first.Event.ID, va.DeclarationID)
			if _, err := sw.Flip(ctx, ns, ModeBefore, "human:ops", "rollback", FreezeHook(fb)); err != nil {
				t.Fatal(err)
			}

			// 'before', an ordinary event and a forged marker: nothing at all.
			plain := deliverVia(json.RawMessage(`{"node":"intake","origin":{"marker":"cn1:forged","artifact_kind":"github.pr","artifact_id":"x"}}`))
			var evals int
			if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2`, ns, plain.Event.ID).Scan(&evals); err != nil || evals != 0 {
				t.Fatalf("'before' recorded %d evaluations for an ordinary event (err=%v), want 0", evals, err)
			}

			// 'before', a reaction to the frozen node: stored, not evaluated.
			reactID := store.NewULID() // placeholder id only to build the origin
			reaction := deliverVia(originPayload(t, reactEvent(t, db, ns, reactID, fa, "timer", nil)))
			node, _, err := fb.NodeByFiring(ctx, ns, fa.ID)
			if err != nil || node.State != NodeStateFrozen {
				t.Fatalf("node=%+v err=%v, want frozen", node, err)
			}
			stored, err := fb.FrozenEvents(ctx, ns, node.ID)
			if err != nil || len(stored) != 1 || stored[0].ID != reaction.Event.ID {
				t.Fatalf("stored=%+v err=%v, want the reaction %s", stored, err, reaction.Event.ID)
			}
			if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2`, ns, reaction.Event.ID).Scan(&evals); err != nil || evals != 0 {
				t.Fatalf("'before' recorded %d evaluations for the stored reaction (err=%v), want 0", evals, err)
			}

			// Forward flip committed; its replay never ran.
			if _, err := sw.Flip(ctx, ns, ModeAfter, "human:ops", "forward"); err != nil {
				t.Fatal(err)
			}
			if crash == "after thaw" {
				// The replay died after thawing: the node is open again but
				// its stored event was never replayed.
				if _, err := fb.ThawFrozenNodes(ctx, ns, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			driver := Driver{Engine: e, Store: db}
			for i := 0; i < 2; i++ { // re-runnable: a second pass changes nothing
				if err := driver.Drive(ctx, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			if got, _ := terminalOutcome(t, db, ns, reaction.Event.ID, vb.DeclarationID); got != OutcomeFired {
				t.Fatalf("replayed reaction outcome = %q, want %q", got, OutcomeFired)
			}
			if n := firingsFor(t, db, ns, reaction.Event.ID); n != 1 {
				t.Fatalf("replayed reaction fired %d times, want exactly 1", n)
			}
			closed, _, err := fb.NodeByFiring(ctx, ns, fa.ID)
			if err != nil || closed.State != NodeStateClosed || closed.ClosedReason != NodeReasonConsumed {
				t.Fatalf("node after replay=%+v err=%v, want closed/consumed", closed, err)
			}
			if left, err := fb.FrozenEvents(ctx, ns, node.ID); err != nil || len(left) != 0 {
				t.Fatalf("stored events left=%+v err=%v", left, err)
			}
		})
	}
}

// Acceptance 2: the Driver runs ExpireDue on the caller's clock in 'shadow'
// and 'after' and does nothing in 'before' (frozen nodes are paused, and the
// declaration engine records nothing there).
func TestPostgresDriverExpiresOnTheCallersClockPerMode(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-driver-expiry").ID
	va := publishActive(t, db, ns, declFor("dx-a", "intake", "none", "waiting", "1h", "timer", `{"uses":"actor://test"}`))
	sw := PostgresSwitchStore{Store: db}
	fb := PostgresBackend{db}
	t.Setenv("TCA_DRIVER_KEY", strings.Repeat("v", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_DRIVER_KEY"}, fb, PostgresMarkerStore{db},
		ShadowGate{Switch: sw, Underlying: &chainDispatcher{rendered: map[string]string{}, vars: map[string]map[string]any{}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sw.Flip(ctx, ns, ModeShadow, "human:ops", "shadow"); err != nil {
		t.Fatal(err)
	}
	first := deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: first, Kind: "timer", Node: "intake"}); err != nil {
		t.Fatal(err)
	}
	fa := firingByEventDecl(t, db, ns, first, va.DeclarationID)
	driver := Driver{Engine: e, Store: db}

	// Not yet due on the caller's clock: nothing expires.
	if err := driver.Drive(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := fb.NodeByFiring(ctx, ns, fa.ID); n.State != NodeStateOpen {
		t.Fatalf("node closed before its deadline: %+v", n)
	}
	// 'before' freezes it (deadline paused): a far-future clock expires nothing.
	if _, err := sw.Flip(ctx, ns, ModeBefore, "human:ops", "rollback", FreezeHook(fb)); err != nil {
		t.Fatal(err)
	}
	if err := driver.Drive(ctx, time.Now().UTC().Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := fb.NodeByFiring(ctx, ns, fa.ID); n.State != NodeStateFrozen {
		t.Fatalf("'before' touched the node: %+v", n)
	}
	// Back to shadow and a clock past the deadline: exactly one node.expired.
	// (A shadow flip does not thaw -- only 'after' replays -- so thaw first
	// the way a forward flip would, to have an open node to expire.)
	if _, err := sw.Flip(ctx, ns, ModeAfter, "human:ops", "forward"); err != nil {
		t.Fatal(err)
	}
	if err := driver.Drive(ctx, time.Now().UTC()); err != nil { // thaws
		t.Fatal(err)
	}
	if n, _, _ := fb.NodeByFiring(ctx, ns, fa.ID); n.State != NodeStateOpen {
		t.Fatalf("driver did not thaw the node in 'after': %+v", n)
	}
	if err := driver.Drive(ctx, time.Now().UTC().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	n, _, _ := fb.NodeByFiring(ctx, ns, fa.ID)
	if n.State != NodeStateClosed || n.ClosedReason != NodeReasonExpired {
		t.Fatalf("node after a due tick = %+v, want closed/expired", n)
	}
	var expired int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM signal_events WHERE namespace_id=$1 AND name='node.expired' AND payload->>'node_id'=$2`, ns, n.ID).Scan(&expired); err != nil || expired != 1 {
		t.Fatalf("node.expired emitted %d times (err=%v), want 1", expired, err)
	}
}

// Acceptance 2: the Driver emits action.* results for a failed dispatched
// run once the namespace is out of 'before'.
func TestPostgresDriverEmitsActionResults(t *testing.T) {
	db, e, ns, firing, _ := actionResultFixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad input", http.StatusBadRequest)
	})
	ctx := context.Background()
	driver := Driver{Engine: e, Store: db}
	count := func() int {
		var n int
		if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM signal_events WHERE namespace_id=$1 AND run_id=$2 AND name LIKE 'action.%'`, ns, firing.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := driver.Drive(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("'before' emitted %d action results, want 0 (kept for later)", n)
	}
	if _, err := (PostgresSwitchStore{Store: db}).Flip(ctx, ns, ModeAfter, "human:ops", "after"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := driver.Drive(ctx, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(); n != 1 {
		t.Fatalf("action results emitted %d times across two passes, want exactly 1", n)
	}
}
