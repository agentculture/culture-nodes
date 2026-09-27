package declengine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// publishActive publishes d and activates that version, as a human would.
func publishActive(t *testing.T, db *postgres.Store, ns string, d decl.Declaration) postgres.DeclarationVersion {
	t.Helper()
	body, err := d.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	v, err := db.PublishDeclaration(context.Background(), postgres.PublishDeclarationInput{NamespaceID: ns, Name: d.Name, Body: body, Author: "human"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDeclarationActivation(context.Background(), ns, v.ID, "activate", "human", ""); err != nil {
		t.Fatal(err)
	}
	return v
}

func deliver(t *testing.T, db *postgres.Store, ns string) string {
	t.Helper()
	ev, err := db.DeliverSignalEvent(context.Background(), postgres.DeliverSignalEventInput{NamespaceID: ns, Name: "timer", Emitter: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return ev.Event.ID
}

func outcomes(t *testing.T, db *postgres.Store, ns, eventID, declarationID string) []string {
	t.Helper()
	rows, err := db.Pool().Query(context.Background(), `SELECT outcome FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3 ORDER BY created_at,id`, ns, eventID, declarationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
	return out
}

// workerFiring drives one declaration firing through the real worker, whose
// actor answers with a single ledger record claimed at authority. It returns
// the firing id after the worker has driven the run to a terminal state.
func workerFiring(t *testing.T, authority ledger.Authority) (*postgres.Store, string, *Engine, postgres.DeclarationVersion, string) {
	t.Helper()
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-worker")
	// The firing's derived decision record needs a registered engine identity;
	// generated because actors.id is global across these shared-database tests.
	producer := store.NewULID()
	if _, err := db.Pool().Exec(ctx, `INSERT INTO actors(id,namespace_id,actor_key,revision,kind,protocol) VALUES($1,$2,'engine/declarations',1,'validator','internal')`, producer, ns.ID); err != nil {
		t.Fatal(err)
	}
	agent := store.NewULID()
	if _, err := db.Pool().Exec(ctx, `INSERT INTO actors(id,namespace_id,actor_key,revision,kind,protocol) VALUES($1,$2,'test/worker',1,'agent','test')`, agent, ns.ID); err != nil {
		t.Fatal(err)
	}
	d := active("worker-test").Declaration
	d.Condition = "true"
	d.Action.With = json.RawMessage(`{"uses":"actor://test/worker@sha256:aaaaaa","input":{"text":"hello"}}`)
	version := publishActive(t, db, ns.ID, d)
	eventID := deliver(t, db, ns.ID)

	t.Setenv("TCA_PG_KEY", strings.Repeat("k", 32))
	e, err := New(Config{MarkerKeyEnv: "TCA_PG_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db}, WorkerDispatcher{Store: db, ProducerActorID: producer})
	if err != nil {
		t.Fatal(err)
	}
	ev := Event{NamespaceID: ns.ID, ID: eventID, Kind: "timer", Node: "ready", Variables: map[string]any{"key": "CHAIN"}}
	// Delivered twice: acceptance 5, one firing and one actor invocation.
	for i := 0; i < 2; i++ {
		if err := e.Handle(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	var firing string
	if err := db.Pool().QueryRow(ctx, `SELECT id FROM declaration_firings WHERE namespace_id=$1`, ns.ID).Scan(&firing); err != nil {
		t.Fatal(err)
	}

	calls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req actors.InvocationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var input map[string]any
		_ = json.Unmarshal(req.Input, &input)
		if input["origin_marker"] == nil || input["text"] != "hello" {
			t.Errorf("worker input lost the rendered action or the marker: %s", req.Input)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(actors.InvocationResult{Outcome: "completed", Output: json.RawMessage(`{"artifact_id":"pr-42","z":"RESULT"}`),
			LedgerDelta: &actors.LedgerDelta{Records: []ledger.Record{{RecordType: ledger.RecordClaim, Authority: authority, Origin: ledger.Origin{Kind: ledger.OriginAgent, ActorID: agent}, Data: json.RawMessage(`{"statement":"action result"}`)}}}})
	}))
	t.Cleanup(srv.Close)
	graph, err := postgres.NewEngine(db, ns.ID)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := actors.NewTokenSigner([]byte(strings.Repeat("s", 32)))
	if err != nil {
		t.Fatal(err)
	}
	wk, err := worker.New(db, graph, worker.Options{NamespaceID: ns.ID, WorkerID: "tca-" + t.Name(), Signer: signer, CallbackBaseURL: srv.URL,
		Registry: worker.StaticRegistry{"actor://test/worker": actors.Endpoint{URL: srv.URL}}, OnError: func(err error) { t.Error(err) }})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		var state string
		if err := db.Pool().QueryRow(ctx, `SELECT status FROM runs WHERE namespace_id=$1 AND id=$2`, ns.ID, firing).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "running" {
			break
		}
		if _, err := wk.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("actor invoked %d times, want exactly once", calls.Load())
	}
	return db, firing, e, version, ns.ID
}

// Acceptance 1, 3 and 4 through the existing worker/actor path: one actor
// invocation, the landing node opened, the firing's ledger record naming the
// declaration and its three component digests, and the agent's result
// proposed while the firing decision itself is the engine's derived record.
func TestPostgresFiringThroughExistingWorker(t *testing.T) {
	db, firing, e, version, ns := workerFiring(t, ledger.AuthorityProposed)
	ctx := context.Background()
	var nodes int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_nodes WHERE namespace_id=$1 AND opening_firing_id=$2 AND node_name='waiting' AND state='open' AND deadline IS NOT NULL`, ns, firing).Scan(&nodes); err != nil || nodes != 1 {
		t.Fatalf("landing nodes=%d err=%v", nodes, err)
	}
	var eventID string
	if err := db.Pool().QueryRow(ctx, `SELECT event_id FROM declaration_firings WHERE id=$1`, firing).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(outcomes(t, db, ns, eventID, version.DeclarationID), ",")
	want := strings.Join([]string{OutcomeMatched, OutcomeLineageChecked, OutcomeConditionTrue, OutcomeDispatching, OutcomeFired,
		OutcomeMatched, OutcomeLineageChecked, OutcomeConditionTrue, OutcomeDuplicate}, ",")
	if got != want {
		t.Fatalf("evaluation steps %s, want %s", got, want)
	}
	if err := e.Reconcile(ctx, db, ns, firing); err != nil {
		t.Fatal(err)
	}
	ancestors, err := (PostgresBackend{db}).Lineage(ctx, ns, firing)
	if err != nil {
		t.Fatal(err)
	}
	if len(ancestors) != 1 || ancestors[0].Variables["z"] != "RESULT" || ancestors[0].Variables["key"] != "CHAIN" {
		t.Fatalf("ancestry=%+v", ancestors)
	}
	ls, err := postgres.NewLedgerStore(db, ns)
	if err != nil {
		t.Fatal(err)
	}
	records, err := ls.RunRecords(ctx, firing)
	if err != nil {
		t.Fatal(err)
	}
	var decision, result bool
	for _, r := range records {
		var data map[string]any
		_ = json.Unmarshal(r.Data, &data)
		switch r.Origin.Kind {
		case ledger.OriginEngine:
			decision = r.Authority == ledger.AuthorityDerived && data["declaration_id"] == version.DeclarationID && data["declaration_version"] == version.ID
			for _, key := range []string{"trigger_digest", "condition_digest", "action_digest"} {
				if s, _ := data[key].(string); !strings.HasPrefix(s, "sha256:") {
					t.Errorf("firing record %s = %v", key, data[key])
				}
			}
		case ledger.OriginAgent:
			result = r.Authority == ledger.AuthorityProposed
		}
	}
	if !decision || !result {
		t.Fatalf("decision=%v proposed result=%v in %+v", decision, result, records)
	}
	var artifact string
	if err := db.Pool().QueryRow(ctx, `SELECT artifact_id FROM declaration_minted_markers WHERE namespace_id=$1 AND firing_id=$2`, ns, firing).Scan(&artifact); err != nil || artifact != "pr-42" {
		t.Fatalf("artifact=%s err=%v", artifact, err)
	}
}

// Acceptance 4: an agent actor that claims observed evidence gets nothing
// above proposed into the ledger — the existing authority matrix refuses it.
func TestPostgresAgentCannotObserve(t *testing.T) {
	db, firing, _, _, ns := workerFiring(t, ledger.AuthorityObserved)
	var promoted int
	if err := db.Pool().QueryRow(context.Background(), `SELECT count(*) FROM ledger_records WHERE namespace_id=$1 AND run_id=$2 AND origin_kind='agent' AND authority<>'proposed'`, ns, firing).Scan(&promoted); err != nil || promoted != 0 {
		t.Fatalf("agent records above proposed=%d err=%v", promoted, err)
	}
}

// chainDispatcher answers synchronously, as an action that created an
// artifact would: its id binds the marker, and it returns variables.
type chainDispatcher struct {
	vars     map[string]map[string]any
	rendered map[string]string
}

func (c *chainDispatcher) Dispatch(_ context.Context, r DispatchRequest) (DispatchResult, error) {
	c.rendered[r.Declaration.Name] = string(r.Action.With)
	return DispatchResult{ArtifactID: "artifact-" + r.Firing.ID, Variables: c.vars[r.Declaration.Name]}, nil
}

// Acceptance 2, 3 and 4 against PostgreSQL: a three-declaration chain linked
// only by verified origin markers renders {1:x}, {2:y} and {a:z} from the
// lineage SQL; 'must' refuses an unmarked event and records why; an upgrade
// between two firings of one declaration pins two different versions.
func TestPostgresThreeDeclarationChain(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-chain").ID
	decl3 := func(name, start, landing, with string) decl.Declaration {
		d := active(name).Declaration
		d.Condition = "true"
		d.StartNode.Name, d.LandingNode.Name = start, landing
		d.Action.With = json.RawMessage(with)
		return d
	}
	va := publishActive(t, db, ns, decl3("a", "intake", "pr-open", `{"uses":"actor://test"}`))
	vb := publishActive(t, db, ns, decl3("b", "pr-open", "reviewed", `{"uses":"actor://test"}`))
	vc := publishActive(t, db, ns, decl3("c", "reviewed", "done", `{"uses":"actor://test","input":{"text":"{1:x}|{2:y}|{a:z}|{b:missing:none}"}}`))
	if err := db.LinkDeclarations(ctx, ns, vb.DeclarationID, va.DeclarationID, "must"); err != nil {
		t.Fatal(err)
	}
	if err := db.LinkDeclarations(ctx, ns, vc.DeclarationID, vb.DeclarationID, "can"); err != nil {
		t.Fatal(err)
	}
	d := &chainDispatcher{rendered: map[string]string{}, vars: map[string]map[string]any{
		"a": {"y": "AY", "z": "AZ"}, "b": {"x": "BX"},
	}}
	t.Setenv("TCA_CHAIN_KEY", strings.Repeat("c", 40))
	e, err := New(Config{MarkerKeyEnv: "TCA_CHAIN_KEY"}, PostgresBackend{db}, PostgresMarkerStore{db}, d)
	if err != nil {
		t.Fatal(err)
	}

	// 'b must appear after a': an unmarked event on b's start node has no a
	// in its lineage, so b does not fire, and the evaluation says why.
	orphan := deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: orphan, Kind: "timer", Node: "pr-open"}); err != nil {
		t.Fatal(err)
	}
	if got := outcomes(t, db, ns, orphan, vb.DeclarationID); strings.Join(got, ",") != OutcomeMatched+","+OutcomeLineageMissing {
		t.Fatalf("unmarked must evaluation %q", got)
	}

	var firingOf = func(eventID, declarationID string) postgres.DeclarationFiring {
		t.Helper()
		var f postgres.DeclarationFiring
		if err := db.Pool().QueryRow(ctx, `SELECT id,declaration_version,trigger_digest,condition_digest,action_digest FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3`,
			ns, eventID, declarationID).Scan(&f.ID, &f.DeclarationVersion, &f.TriggerDigest, &f.ConditionDigest, &f.ActionDigest); err != nil {
			t.Fatalf("firing of %s: %v", declarationID, err)
		}
		return f
	}
	// Each reaction carries the marker the previous action stamped into the
	// artifact it created, and that artifact's id.
	react := func(prev postgres.DeclarationFiring, node string) string {
		t.Helper()
		var nonce, mac string
		if err := db.Pool().QueryRow(ctx, `SELECT nonce,mac FROM declaration_minted_markers WHERE namespace_id=$1 AND firing_id=$2`, ns, prev.ID).Scan(&nonce, &mac); err != nil {
			t.Fatal(err)
		}
		id := deliver(t, db, ns)
		origin := OriginEvent{Marker: "cn1:" + prev.ID + ":github.pr:" + nonce + ":" + mac, ArtifactKind: "github.pr", ArtifactID: "artifact-" + prev.ID}
		if err := e.Handle(ctx, Event{NamespaceID: ns, ID: id, Kind: "timer", Node: node, Origin: origin}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: first, Kind: "timer", Node: "intake", Variables: map[string]any{"issue": "SCRUM-1"}}); err != nil {
		t.Fatal(err)
	}
	fa := firingOf(first, va.DeclarationID)
	second := react(fa, "pr-open")
	fb := firingOf(second, vb.DeclarationID)
	third := react(fb, "reviewed")
	firingOf(third, vc.DeclarationID)
	if got, want := d.rendered["c"], `"text":"BX|AY|AZ|none"`; !strings.Contains(got, want) {
		t.Fatalf("c rendered %s, want %s", got, want)
	}
	lineage, err := (PostgresBackend{db}).Lineage(ctx, ns, fb.ID)
	if err != nil || len(lineage) != 2 || lineage[0].Name != "b" || lineage[1].Name != "a" || lineage[1].Variables["issue"] != "SCRUM-1" {
		t.Fatalf("lineage=%+v err=%v", lineage, err)
	}

	// Upgrade a mid-flight: its next firing pins the new version and the new
	// condition and action digests; the first firing's pins are unchanged.
	upgraded := decl3("a", "intake", "pr-open", `{"uses":"actor://test","input":{"text":"v2"}}`)
	upgraded.Condition = "event.issue != ''"
	va2 := publishActive(t, db, ns, upgraded)
	next := deliver(t, db, ns)
	if err := e.Handle(ctx, Event{NamespaceID: ns, ID: next, Kind: "timer", Node: "intake", Variables: map[string]any{"issue": "SCRUM-2"}}); err != nil {
		t.Fatal(err)
	}
	fa2 := firingOf(next, va.DeclarationID)
	if fa.DeclarationVersion != va.ID || fa2.DeclarationVersion != va2.ID || fa.ConditionDigest == fa2.ConditionDigest ||
		fa.ActionDigest == fa2.ActionDigest || fa.TriggerDigest != fa2.TriggerDigest {
		t.Fatalf("pins not per firing: %+v %+v", fa, fa2)
	}
	if again := firingOf(first, va.DeclarationID); again != fa {
		t.Fatalf("upgrade rewrote history: %+v -> %+v", fa, again)
	}
}

func TestPostgresFiringCatalogForeignKeys(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "tca-fks")
	a := active("a")
	body, _ := a.Declaration.CanonicalJSON()
	v, err := db.PublishDeclaration(ctx, postgres.PublishDeclarationInput{NamespaceID: ns.ID, Name: "a", Body: body, Author: "test"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.PublishDeclaration(ctx, postgres.PublishDeclarationInput{NamespaceID: ns.ID, Name: "b", Body: body, Author: "test"})
	if err != nil {
		t.Fatal(err)
	}
	eventID := deliver(t, db, ns.ID)
	in := postgres.DeclarationFiringInput{NamespaceID: ns.ID, EventID: eventID, DeclarationID: v.DeclarationID, DeclarationVersion: other.ID, TriggerDigest: "t", ConditionDigest: "c", ActionDigest: "a"}
	if _, _, err := db.RecordDeclarationFiring(ctx, in); err == nil {
		t.Fatal("accepted another declaration's version")
	}
	in.DeclarationID = "missing"
	in.DeclarationVersion = v.ID
	if _, _, err := db.RecordDeclarationFiring(ctx, in); err == nil {
		t.Fatal("accepted nonexistent declaration")
	}
	foreign := pgtest.MustNamespace(t, db, "tca-fks-other")
	in.NamespaceID, in.DeclarationID = foreign.ID, v.DeclarationID
	in.EventID = deliver(t, db, foreign.ID)
	if _, _, err := db.RecordDeclarationFiring(ctx, in); err == nil {
		t.Fatal("accepted another namespace's declaration")
	}
	in.NamespaceID, in.EventID = ns.ID, eventID
	f, created, err := db.RecordDeclarationFiring(ctx, in)
	if err != nil || !created {
		t.Fatalf("valid firing: %v %v", created, err)
	}
	// Acceptance 5: a re-mint is a second physical firing of one logical
	// entry, and lineage (hence re-entry) counts it once.
	in.RemintOfID = f.ID
	retry, created, err := db.RecordDeclarationFiring(ctx, in)
	if err != nil || !created {
		t.Fatalf("remint: %v %v", created, err)
	}
	lineage, err := (PostgresBackend{db}).Lineage(ctx, ns.ID, retry.ID)
	if err != nil || len(lineage) != 1 || lineage[0].CanonicalID != f.ID {
		t.Fatalf("lineage=%v err=%v", lineage, err)
	}
}

// BenchmarkPostgresLineage10000 is the plan's lineage-lookup-cost risk: the
// real recursive SQL and indexes over a 10,000-firing chain. depth=10000
// resolves from its tip (the worst case: every row is an ancestor). Setup is
// excluded.
func BenchmarkPostgresLineage10000(b *testing.B) {
	if markerTestStore == nil {
		b.Skip("requires PostgreSQL (NODES_TEST_DATABASE_URL or Docker)")
	}
	db := markerTestStore
	ctx := context.Background()
	prefix := store.NewULID()
	ns, err := db.CreateNamespace(ctx, "bench-"+prefix, "lineage benchmark")
	if err != nil {
		b.Fatal(err)
	}
	d := active("bench").Declaration
	body, _ := d.CanonicalJSON()
	v, err := db.PublishDeclaration(ctx, postgres.PublishDeclarationInput{NamespaceID: ns.ID, Name: "bench", Body: body, Author: "test"})
	if err != nil {
		b.Fatal(err)
	}
	_, err = db.Pool().Exec(ctx, `INSERT INTO signal_events(id,namespace_id,name,emitter) SELECT $1||'event-'||n::text,$2,'timer','benchmark' FROM generate_series(1,10000) n`, prefix, ns.ID)
	if err != nil {
		b.Fatal(err)
	}
	_, err = db.Pool().Exec(ctx, `INSERT INTO declaration_firings(id,namespace_id,event_id,declaration_id,declaration_version,trigger_digest,condition_digest,action_digest,lineage_id,canonical_firing_id,created_at)
 SELECT $1||n::text,$2,$1||'event-'||n::text,$3,$4,'t','c','a',$1||'1',$1||n::text,$5::timestamptz+n*interval '1 microsecond' FROM generate_series(1,10000) n`, prefix, ns.ID, v.DeclarationID, v.ID, time.Now())
	if err != nil {
		b.Fatal(err)
	}
	_, err = db.Pool().Exec(ctx, `INSERT INTO declaration_lineage_edges(namespace_id,parent_firing_id,child_firing_id) SELECT $1,$2||(n-1)::text,$2||n::text FROM generate_series(2,10000) n`, ns.ID, prefix)
	if err != nil {
		b.Fatal(err)
	}
	// Every firing carries a fired evaluation with variables, as in production.
	_, err = db.Pool().Exec(ctx, `INSERT INTO declaration_evaluations(id,namespace_id,event_id,declaration_id,declaration_version,outcome,reason,firing_id,variables)
 SELECT $1||'eval-'||n::text,$2,$1||'event-'||n::text,$3,$4,'fired','bench',$1||n::text,jsonb_build_object('n',n) FROM generate_series(1,10000) n`, prefix, ns.ID, v.DeclarationID, v.ID)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `ANALYZE declaration_firings, declaration_lineage_edges, declaration_evaluations`); err != nil {
		b.Fatal(err)
	}
	backend := PostgresBackend{db}
	// depth=20 resolves from the 20th firing of the same 10,000-row table:
	// the hop limit (default 20) bounds a real lineage to about that depth.
	for _, depth := range []int{10000, 20} {
		b.Run(fmt.Sprint("depth=", depth), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				lineage, err := backend.Lineage(ctx, ns.ID, fmt.Sprint(prefix, depth))
				if err != nil || len(lineage) != depth || lineage[0].Variables["n"] != float64(depth) {
					b.Fatalf("len=%d err=%v", len(lineage), err)
				}
			}
		})
	}
}
