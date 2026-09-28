package declactions_test

// Lineage through human.ask and code.run (issue #328, task t38c; cortex
// review C1/C2; deviation d3). Neither action goes to a stamping bridge: a
// human.ask is served by the control plane's own human-task surface and a
// code.run by a runner:// service, so nothing external ever reports the
// artifact it produced with the firing's marker on it. The control plane is
// the stamper for both: the scheduler Driver's reaction pass binds the
// produced artifact (the decided human task, the runner operation) to the
// firing's minted marker and emits the human.decision / code.result reaction
// event carrying that marker, so a declaration reacting to it continues the
// SAME lineage -- MUST links hold -- instead of starting a fresh one.
//
// Everything here runs through the real surfaces: the production engine
// (declengine.NewPostgres: ShadowGate over WorkerDispatcher), the real worker
// and runner-service protocol (declHarness), the real human decision path
// (engine.DecideHumanTask / ExpireHumanTask) and the scheduler's Driver.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// reactionHarness is declHarness with the production engine behind the
// switch, and the Driver that runs the reaction pass.
type reactionHarness struct {
	*declHarness
	producer string
	sw       declengine.PostgresSwitchStore
	driver   declengine.Driver
}

func newReactionHarness(t *testing.T) *reactionHarness {
	t.Helper()
	h := newDeclHarness(t)
	r := &reactionHarness{declHarness: h, sw: declengine.PostgresSwitchStore{Store: h.db}}
	if err := h.db.Pool().QueryRow(h.ctx, `SELECT id FROM actors WHERE namespace_id=$1 AND actor_key='engine/declarations'`, h.ns).Scan(&r.producer); err != nil {
		t.Fatal(err)
	}
	eng, err := declengine.NewPostgres(declengine.Config{MarkerKeyEnv: declMarkerKeyEnv}, h.db, r.producer)
	if err != nil {
		t.Fatal(err)
	}
	h.engine = eng
	r.driver = declengine.Driver{Engine: eng, Store: h.db}
	r.flip(declengine.ModeAfter)
	return r
}

func (r *reactionHarness) flip(mode string) {
	r.t.Helper()
	if _, err := r.sw.Flip(r.ctx, r.ns, mode, "human:ops", "t38c"); err != nil {
		r.t.Fatal(err)
	}
}

// publish publishes and activates d, listing and approving any step-0
// sensitivity widening (exposeWidenings, approveWidenings).
func (r *reactionHarness) publish(d decl.Declaration) postgres.DeclarationVersion {
	t := r.t
	t.Helper()
	if d.Trigger.ReentryLimit == 0 {
		d.Trigger.ReentryLimit, d.Trigger.HopLimit, d.Trigger.RateCeiling = 3, 20, "30/h"
	}
	if d.Condition == "" {
		d.Condition = "true"
	}
	d = exposeWidenings(t, d)
	body, err := d.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.db.PublishDeclaration(r.ctx, postgres.PublishDeclarationInput{NamespaceID: r.ns, Name: d.Name, Body: body, Author: "human"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.db.RecordDeclarationActivation(r.ctx, r.ns, v.ID, "activate", "human", ""); err != nil {
		t.Fatal(err)
	}
	r.approveWidenings(v, d)
	return v
}

func (r *reactionHarness) must(from, to postgres.DeclarationVersion) {
	r.t.Helper()
	if err := r.db.LinkDeclarations(r.ctx, r.ns, from.DeclarationID, to.DeclarationID, "must"); err != nil {
		r.t.Fatal(err)
	}
}

// start delivers the timer event that fires the chain's first declaration.
func (r *reactionHarness) start(node string) {
	t := r.t
	t.Helper()
	ev, err := r.db.DeliverSignalEvent(r.ctx, postgres.DeliverSignalEventInput{NamespaceID: r.ns, Name: "timer", Emitter: "conformance"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Handle(r.ctx, declengine.Event{NamespaceID: r.ns, ID: ev.Event.ID, Kind: "timer", Node: node,
		Variables: map[string]any{"subject": "SCRUM-7", "note": "rendered-from-event"}}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

type firingRow struct{ id, parent, lineage, eventID string }

// firings returns v's firings, oldest first.
func (r *reactionHarness) firings(v postgres.DeclarationVersion) []firingRow {
	t := r.t
	t.Helper()
	rows, err := r.db.Pool().Query(r.ctx, `SELECT f.id,COALESCE((SELECT e.parent_firing_id FROM declaration_lineage_edges e
		WHERE e.namespace_id=f.namespace_id AND e.child_firing_id=f.id LIMIT 1),''),f.lineage_id,f.event_id FROM declaration_firings f
		WHERE f.namespace_id=$1 AND f.declaration_id=$2 ORDER BY f.created_at,f.id`, r.ns, v.DeclarationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []firingRow
	for rows.Next() {
		var f firingRow
		if err := rows.Scan(&f.id, &f.parent, &f.lineage, &f.eventID); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func (r *reactionHarness) onlyFiring(v postgres.DeclarationVersion) firingRow {
	r.t.Helper()
	fs := r.firings(v)
	if len(fs) != 1 {
		r.t.Fatalf("declaration %s has %d firings, want exactly 1: %+v", v.DeclarationID, len(fs), fs)
	}
	return fs[0]
}

// settle drives the firing's run until it leaves 'running', ticking the
// worker and sampling runner operations; it returns the terminal status (or
// 'running' when the run is parked on a human task).
func (r *reactionHarness) settle(firing string, wantParked bool) string {
	t := r.t
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if err := r.db.Pool().QueryRow(r.ctx, `SELECT status FROM runs WHERE namespace_id=$1 AND id=$2`, r.ns, firing).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "running" {
			return state
		}
		if wantParked && r.pendingTask(firing) != "" {
			return state
		}
		if _, err := r.worker.Tick(r.ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := r.worker.SampleRunnerOperations(r.ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("firing run %s did not settle", firing)
	return ""
}

func (r *reactionHarness) pendingTask(run string) string {
	var id string
	err := r.db.Pool().QueryRow(r.ctx, `SELECT id FROM human_tasks WHERE namespace_id=$1 AND run_id=$2 AND status='pending'`, r.ns, run).Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

// decide answers the firing's human task through the real decision path
// (or expires it through the real expiry path), returning the task id.
func (r *reactionHarness) decide(firing, outcome string) string {
	t := r.t
	t.Helper()
	if state := r.settle(firing, true); state != "running" {
		t.Fatalf("human.ask run %s is %s before any decision, want parked on its task", firing, state)
	}
	task := r.pendingTask(firing)
	if task == "" {
		t.Fatalf("human.ask firing %s opened no pending human task", firing)
	}
	graph, err := postgres.NewEngine(r.db, r.ns)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == engine.OutcomeExpired {
		_, err = graph.ExpireHumanTask(r.ctx, engine.ExpireHumanTaskRequest{HumanTaskID: task, Reason: "deadline", ProducerActorID: r.producer})
	} else {
		l, lerr := postgres.NewLedger(r.db, r.ns)
		if lerr != nil {
			t.Fatal(lerr)
		}
		version, verr := l.LedgerVersion(r.ctx, firing)
		if verr != nil {
			t.Fatal(verr)
		}
		decider := store.NewULID()
		if _, err := r.db.Pool().Exec(r.ctx, `INSERT INTO actors(id,namespace_id,actor_key,revision,kind,protocol) VALUES($1,$2,$3,1,'human','http')`,
			decider, r.ns, "human/"+decider); err != nil {
			t.Fatal(err)
		}
		_, err = graph.DecideHumanTask(r.ctx, engine.HumanTaskDecisionRequest{HumanTaskID: task, Outcome: outcome,
			Response: json.RawMessage(`{"comment":"t38c"}`), DeciderActorID: decider, ExpectedLedgerVersion: version})
	}
	if err != nil {
		t.Fatalf("resolve human task %s as %s: %v", task, outcome, err)
	}
	if state := r.settle(firing, false); state != "completed" {
		rows, _ := r.db.Pool().Query(r.ctx, `SELECT event_type,data::text FROM events WHERE aggregate_id=$1 ORDER BY sequence`, firing)
		for rows.Next() {
			var a, b string
			_ = rows.Scan(&a, &b)
			t.Logf("event %s %s", a, b)
		}
		rows.Close()
		t.Fatalf("human.ask run %s ended %s after %s, want completed", firing, state, outcome)
	}
	return task
}

func (r *reactionHarness) drive() {
	r.t.Helper()
	if err := r.driver.Drive(r.ctx, time.Now().UTC()); err != nil {
		r.t.Fatalf("Drive: %v", err)
	}
}

type reaction struct {
	ID      string
	Payload struct {
		Node    string         `json:"node"`
		Outcome string         `json:"outcome"`
		Result  map[string]any `json:"result"`
		Origin  struct {
			Marker       string `json:"marker"`
			ArtifactKind string `json:"artifact_kind"`
			ArtifactID   string `json:"artifact_id"`
		} `json:"origin"`
	}
}

// reactions returns the reaction events emitted for run under name.
func (r *reactionHarness) reactions(run, name string) []reaction {
	t := r.t
	t.Helper()
	rows, err := r.db.Pool().Query(r.ctx, `SELECT id,payload FROM signal_events WHERE namespace_id=$1 AND run_id=$2 AND name=$3 ORDER BY created_at,id`, r.ns, run, name)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []reaction
	for rows.Next() {
		var x reaction
		var raw []byte
		if err := rows.Scan(&x.ID, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &x.Payload); err != nil {
			t.Fatalf("reaction payload %s: %v", raw, err)
		}
		out = append(out, x)
	}
	return out
}

func (r *reactionHarness) lineageLen(firing string) int {
	r.t.Helper()
	lineage, err := (declengine.PostgresBackend{Store: r.db}).Lineage(r.ctx, r.ns, firing)
	if err != nil {
		r.t.Fatal(err)
	}
	return len(lineage)
}

func (r *reactionHarness) lastOutcome(eventID string, v postgres.DeclarationVersion) string {
	r.t.Helper()
	var outcome string
	if err := r.db.Pool().QueryRow(r.ctx, `SELECT outcome FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3
		ORDER BY created_at DESC,id DESC LIMIT 1`, r.ns, eventID, v.DeclarationID).Scan(&outcome); err != nil {
		r.t.Fatalf("no evaluation of %s for event %s: %v", v.DeclarationID, eventID, err)
	}
	return outcome
}

// assertContinues pins the heart of t38c: exactly one reaction for the
// parent's run, and the child firing hangs off the parent in one lineage.
func (r *reactionHarness) assertContinues(parent firingRow, child postgres.DeclarationVersion, name, wantOutcome, wantArtifact string) firingRow {
	t := r.t
	t.Helper()
	rx := r.reactions(parent.id, name)
	if len(rx) != 1 {
		t.Fatalf("%d %s reactions for run %s, want exactly 1", len(rx), name, parent.id)
	}
	p := rx[0].Payload
	if p.Outcome != wantOutcome || p.Origin.ArtifactKind != name || p.Origin.ArtifactID != wantArtifact ||
		!strings.HasPrefix(p.Origin.Marker, "cn1:"+parent.id+":"+name+":") || p.Node == "" {
		t.Fatalf("reaction payload = %+v, want outcome %s, artifact %s/%s and the firing's marker", p, wantOutcome, name, wantArtifact)
	}
	c := r.onlyFiring(child)
	if c.parent != parent.id || c.lineage != parent.lineage || c.eventID != rx[0].ID {
		t.Fatalf("child firing = %+v, want parent %s in lineage %s via reaction %s", c, parent.id, parent.lineage, rx[0].ID)
	}
	return c
}

func askDecl(name, start, landing string) decl.Declaration {
	return decl.Declaration{Name: name, Trigger: decl.Trigger{Kind: "timer"},
		Action:    decl.Action{Kind: "human.ask", With: json.RawMessage(`{"approver_ref":"group/reviewers","input":{"question":"merge?"}}`)},
		StartNode: decl.Node{Name: start, Deadline: "none"}, LandingNode: decl.Node{Name: landing, Deadline: "1h"}}
}

func codeDecl(name, trigger, start, landing string) decl.Declaration {
	with := `{"uses":"` + codeRunnerRef + `","input":{"note":"x"},"operation":{"image":"python:3.12-slim@` + codeImageDigest +
		`","argv":["python3","-V"],"network":"none","allowedOutputPaths":[]}}`
	return decl.Declaration{Name: name, Trigger: decl.Trigger{Kind: trigger},
		Action:    decl.Action{Kind: "code.run", With: json.RawMessage(with)},
		StartNode: decl.Node{Name: start, Deadline: "none"}, LandingNode: decl.Node{Name: landing, Deadline: "1h"}}
}

func jiraDecl(name, trigger, start, landing string) decl.Declaration {
	return decl.Declaration{Name: name, Trigger: decl.Trigger{Kind: trigger},
		Action: decl.Action{Kind: "jira.comment", With: json.RawMessage(`{"uses":"actor://culture/jira@sha256:` + strings.Repeat("a", 64) +
			`","input":{"issue":"SCRUM-7","body":"next"}}`)},
		StartNode: decl.Node{Name: start, Deadline: "none"}, LandingNode: decl.Node{Name: landing, Deadline: "1h"}}
}

// A human.ask firing, decided through the real decision path, is followed
// by a human.decision reaction that continues its lineage: B (MUST after A)
// fires with A as its parent and a two-entry lineage. Every decision
// outcome -- approved, rejected, and the engine's own expired -- is carried.
func TestHumanAskDecisionContinuesTheLineage(t *testing.T) {
	for _, outcome := range []string{"approved", "rejected", engine.OutcomeExpired} {
		t.Run(outcome, func(t *testing.T) {
			r := newReactionHarness(t)
			a := r.publish(askDecl("rx-ask", "ready", "asked"))
			b := r.publish(jiraDecl("rx-after-ask", "human.decision", "asked", "done"))
			r.must(b, a)
			r.start("ready")
			fa := r.onlyFiring(a)
			task := r.decide(fa.id, outcome)
			// Two passes: the reaction is emitted once per run, not per tick.
			r.drive()
			r.drive()
			fb := r.assertContinues(fa, b, "human.decision", outcome, task)
			if n := r.lineageLen(fb.id); n != 2 {
				t.Fatalf("B's lineage has %d entries, want 2 (B and A)", n)
			}
			if got := r.lastOutcome(fb.eventID, b); got != declengine.OutcomeFired {
				t.Fatalf("B outcome = %q, want %q", got, declengine.OutcomeFired)
			}
		})
	}
}

// A code.run firing through the runner service is followed by a code.result
// reaction whose artifact is the runner operation, continuing the lineage.
func TestCodeRunResultContinuesTheLineage(t *testing.T) {
	r := newReactionHarness(t)
	a := r.publish(codeDecl("rx-code", "timer", "ready", "ran"))
	b := r.publish(jiraDecl("rx-after-code", "code.result", "ran", "done"))
	r.must(b, a)
	r.start("ready")
	fa := r.onlyFiring(a)
	if state := r.settle(fa.id, false); state != "completed" {
		t.Fatalf("code.run run ended %s, want completed", state)
	}
	ops := r.runner.operations()
	if len(ops) != 1 {
		t.Fatalf("runner received %d operations, want 1", len(ops))
	}
	r.drive()
	r.drive()
	fb := r.assertContinues(fa, b, "code.result", "passed", ops[0].OperationID)
	if n := r.lineageLen(fb.id); n != 2 {
		t.Fatalf("B's lineage has %d entries, want 2", n)
	}
	if rx := r.reactions(fa.id, "code.result"); rx[0].Payload.Result == nil {
		t.Fatalf("code.result reaction carries no result: %+v", rx[0].Payload)
	}
}

// A nonzero exit is the code step's domain answer: the run completes and
// the code.result reaction carries outcome failed, still in the lineage.
func TestCodeRunFailureIsAReactionOutcome(t *testing.T) {
	r := newReactionHarness(t)
	r.runner.exitCode = 1
	a := r.publish(codeDecl("rx-code-fail", "timer", "ready", "ran"))
	b := r.publish(jiraDecl("rx-after-code-fail", "code.result", "ran", "done"))
	r.must(b, a)
	r.start("ready")
	fa := r.onlyFiring(a)
	if state := r.settle(fa.id, false); state != "completed" {
		t.Fatalf("code.run run with exit 1 ended %s, want completed with outcome failed", state)
	}
	r.drive()
	r.assertContinues(fa, b, "code.result", "failed", r.runner.operations()[0].OperationID)
}

// 'before' emits nothing (the run's reaction is kept for later, exactly as
// an action.* result is); 'shadow' emits and evaluates it, recording a
// would-fire firing whose action is never dispatched.
func TestReactionPassObeysTheSwitch(t *testing.T) {
	r := newReactionHarness(t)
	a := r.publish(askDecl("rx-sw-ask", "ready", "asked"))
	b := r.publish(jiraDecl("rx-sw-after", "human.decision", "asked", "done"))
	r.must(b, a)
	r.start("ready")
	fa := r.onlyFiring(a)
	r.decide(fa.id, "approved")

	r.flip(declengine.ModeBefore)
	r.drive()
	if rx := r.reactions(fa.id, "human.decision"); len(rx) != 0 {
		t.Fatalf("'before' emitted %d reactions, want 0", len(rx))
	}
	r.flip(declengine.ModeShadow)
	r.drive()
	r.drive()
	fb := r.assertContinues(fa, b, "human.decision", "approved", r.taskOf(fa.id))
	if got := r.lastOutcome(fb.eventID, b); got != declengine.OutcomeShadow {
		t.Fatalf("B outcome in 'shadow' = %q, want %q", got, declengine.OutcomeShadow)
	}
	var runs int
	if err := r.db.Pool().QueryRow(r.ctx, `SELECT count(*) FROM runs WHERE namespace_id=$1 AND id=$2`, r.ns, fb.id).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("'shadow' dispatched B: %d runs (err=%v)", runs, err)
	}
	if n := len(r.bridges["culture/jira"].received()); n != 0 {
		t.Fatalf("'shadow' invoked the jira bridge %d times", n)
	}
}

func (r *reactionHarness) taskOf(run string) string {
	r.t.Helper()
	var id string
	if err := r.db.Pool().QueryRow(r.ctx, `SELECT id FROM human_tasks WHERE namespace_id=$1 AND run_id=$2`, r.ns, run).Scan(&id); err != nil {
		r.t.Fatal(err)
	}
	return id
}

// A forged reaction -- the right name and artifact, a marker the engine did
// not mint -- is rejected and starts a fresh lineage, so B's MUST link is
// not satisfied; the real reaction still continues the lineage afterwards.
func TestForgedReactionStartsAFreshLineage(t *testing.T) {
	r := newReactionHarness(t)
	a := r.publish(askDecl("rx-forge-ask", "ready", "asked"))
	b := r.publish(jiraDecl("rx-forge-after", "human.decision", "asked", "done"))
	r.must(b, a)
	r.start("ready")
	fa := r.onlyFiring(a)
	task := r.decide(fa.id, "approved")

	forged := "cn1:" + fa.id + ":human.decision:" + strings.Repeat("0", 48) + ":" + strings.Repeat("f", 64)
	payload, _ := json.Marshal(map[string]any{"node": "asked", "outcome": "approved",
		"origin": map[string]string{"marker": forged, "artifact_kind": "human.decision", "artifact_id": task}})
	d, err := r.db.DeliverSignalEvent(r.ctx, postgres.DeliverSignalEventInput{NamespaceID: r.ns, Name: "human.decision", Payload: payload,
		Emitter: "forger", Declarations: declengine.Router{Engine: r.engine, Switch: r.sw}})
	if err != nil || d.DeclarationErr != nil {
		t.Fatalf("deliver forged: err=%v declarationErr=%v", err, d.DeclarationErr)
	}
	if got := r.lastOutcome(d.Event.ID, b); got != declengine.OutcomeLineageMissing {
		t.Fatalf("forged reaction: B outcome %q, want %q (fresh lineage, MUST link unmet)", got, declengine.OutcomeLineageMissing)
	}
	if fs := r.firings(b); len(fs) != 0 {
		t.Fatalf("forged reaction fired B: %+v", fs)
	}
	r.drive()
	r.assertContinues(fa, b, "human.decision", "approved", task)
}
