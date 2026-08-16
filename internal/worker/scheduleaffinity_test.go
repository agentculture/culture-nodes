package worker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/scheduler"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// The whole chain, in one test (issue #107, task t33, acceptance criteria 1,
// 3 and 4):
//
//	a declared schedule comes due
//	  -> the scheduler tick appends a signal event, with nobody typing anything
//	  -> the workflow's trigger (task t17b) creates a run from it
//	  -> the run records the actor its declared affinity chose
//	  -> the worker dispatches the developer node to THAT actor
//
// Two separate actor servers stand in for two registered actors, because the
// only way to demonstrate routing rather than assert it is to show which of
// two endpoints actually received the invocation.
//
// The clock is a variable this test assigns to. Nothing here sleeps, and
// nothing waits for a cadence to elapse.

const upkeepWithAffinity = `apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow
metadata: {name: upkeep-affine, version: 1.0.0, ownerRef: team/platform-ai}
spec:
  entry: fix
  triggers:
    - onEvent: upkeep.finding
      when: size(event.payload.findings) > 0
  affinity:
    - name: security-findings
      node: fix
      actor: actor://company/security-developer
      when: event.payload.findings.exists(f, f.kind == "security")
    - name: general-findings
      node: fix
      actor: actor://company/developer
  contract:
    input:
      schema:
        type: object
        required: [findings]
        properties:
          findings: {type: array, minItems: 1}
    output: {schema: {type: object}}
  nodes:
    fix:
      kind: agent
      ownerRef: team/platform-ai
      uses: actor://company/unrouted@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
      input: {from: /run/input}
      contract: {outcomes: {completed: {schema: {type: object}}}}
    finish: {kind: end, ownerRef: team/platform-ai, output: {from: /nodes/fix/output}}
  edges: [{from: fix.completed, to: finish}]
`

// namedActor is one registered actor endpoint that remembers it was called.
type namedActor struct {
	name   string
	server *httptest.Server

	mu   sync.Mutex
	hits []actors.InvocationRequest
}

func newNamedActor(t *testing.T, name string) *namedActor {
	t.Helper()
	a := &namedActor{name: name}
	a.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req actors.InvocationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		a.hits = append(a.hits, req)
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"succeeded","outcome":"completed","output":{"by":"` + name + `"}}`))
	}))
	t.Cleanup(a.server.Close)
	return a
}

func (a *namedActor) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.hits)
}

// upkeepFixture is one namespace with the affinity workflow published, two
// registered actors, a scheduler and a worker -- i.e. a control plane.
type upkeepFixture struct {
	store    *storepg.Store
	ns       storepg.Namespace
	sched    *scheduler.Scheduler
	worker   *worker.Worker
	general  *namedActor
	security *namedActor
	clock    *time.Time
}

func newUpkeepFixture(t *testing.T) *upkeepFixture {
	t.Helper()
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "upkeep-affinity")
	ctx := context.Background()

	cw, diags, err := compiler.Compile([]byte(upkeepWithAffinity), compiler.FormatYAML)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, d := range diags {
		if d.Level == compiler.LevelError {
			t.Fatalf("%s %s: %s", d.Path, d.Code, d.Message)
		}
	}
	if _, err := s.CreateWorkflowVersion(ctx, storepg.CreateWorkflowVersionInput{
		NamespaceID: ns.ID, WorkflowKey: "upkeep-affine", SourceFormat: "yaml",
		Source: upkeepWithAffinity, NormalizedIR: cw.Normalized, ContentDigest: cw.Digest,
	}); err != nil {
		t.Fatalf("CreateWorkflowVersion: %v", err)
	}

	eng, err := storepg.NewEngine(s, ns.ID, engine.WithRetryDelays(0, 0))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	signer, err := actors.NewTokenSigner([]byte(testSecret))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}

	f := &upkeepFixture{
		store: s, ns: ns,
		general:  newNamedActor(t, "general"),
		security: newNamedActor(t, "security"),
		clock:    new(time.Time),
	}
	f.sched = scheduler.New(s, scheduler.Options{Now: func() time.Time { return *f.clock }})

	callbacks, err := storepg.NewCallbackStore(s, ns.ID)
	if err != nil {
		t.Fatalf("NewCallbackStore: %v", err)
	}
	callbackServer := httptest.NewServer(actors.NewCallbackHandler(actors.CallbackDeps{
		Store: callbacks, Engine: eng, Signer: signer,
	}))
	t.Cleanup(callbackServer.Close)

	wk, err := worker.New(s, eng, worker.Options{
		WorkerID: "worker-" + t.Name(), NamespaceID: ns.ID,
		ClaimBatch: 4, LeaseDuration: 30 * time.Second,
		HeartbeatInterval: time.Second, PollInterval: 10 * time.Millisecond,
		Registry: worker.StaticRegistry{
			"actor://company/developer":          {URL: f.general.server.URL},
			"actor://company/security-developer": {URL: f.security.server.URL},
			// Deliberately registered too: if affinity did not apply, the
			// dispatch would succeed against this endpoint rather than fail,
			// and the test would have to distinguish "routed" from "could not
			// resolve". Registering it means the only way the security actor
			// gets the call is because affinity routed it there.
			"actor://company/unrouted": {URL: f.general.server.URL},
		},
		Signer:          signer,
		CallbackBaseURL: callbackServer.URL,
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	f.worker = wk
	return f
}

// runAffinityColumn reads what the run actually recorded -- the column, not
// the engine's in-memory value.
func runAffinityColumn(t *testing.T, s *storepg.Store, runID string) string {
	t.Helper()
	var raw []byte
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT actor_affinity FROM runs WHERE id = $1`, runID).Scan(&raw); err != nil {
		t.Fatalf("read actor_affinity for run %s: %v", runID, err)
	}
	return string(raw)
}

func TestAScheduledFindingReachesARunAndTheDeclaredActorWithNoOperatorAction(t *testing.T) {
	f := newUpkeepFixture(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC)
	*f.clock = base.Add(-time.Minute)

	// The ONLY operator action in this test: declaring the schedule. After
	// this line nothing issues a command, posts an event, or creates a run.
	if _, err := f.store.CreateSchedule(ctx, storepg.CreateScheduleInput{
		NamespaceID: f.ns.ID, Name: "nightly-upkeep", EventName: "upkeep.finding",
		Emitter: "schedule:nightly-upkeep", Interval: 24 * time.Hour, FirstFireAt: base,
		Payload: json.RawMessage(`{"findings":[{"kind":"security","detail":"unpinned base image"}]}`),
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	// --- no human action from here down ---

	*f.clock = base
	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("scheduler Tick: %v", err)
	}

	var runID, eventID string
	if err := f.store.Pool().QueryRow(ctx, `
		SELECT r.id, s.last_event_id FROM runs r, schedules s
		WHERE r.namespace_id = $1 AND s.namespace_id = $1 AND s.name = 'nightly-upkeep'`,
		f.ns.ID).Scan(&runID, &eventID); err != nil {
		t.Fatalf("the schedule did not produce a run: %v", err)
	}
	t.Logf("schedule fired: event %s -> run %s", eventID, runID)

	// Criterion 3, first clause: the affinity is recorded ON THE RUN.
	const wantAffinity = `{"fix": {"rule": "security-findings", "actor": "actor://company/security-developer"}}`
	got := runAffinityColumn(t, f.store, runID)
	if got == "" || got == "null" {
		t.Fatalf("the run recorded no actor affinity; the comparative record has nothing to read")
	}
	var recorded map[string]struct{ Actor, Rule string }
	if err := json.Unmarshal([]byte(got), &recorded); err != nil {
		t.Fatalf("actor_affinity is not readable JSON (%s): %v", got, err)
	}
	if recorded["fix"].Actor != "actor://company/security-developer" || recorded["fix"].Rule != "security-findings" {
		t.Fatalf("actor_affinity = %s, want the security rule (compare %s)", got, wantAffinity)
	}

	// Criterion 3, second clause: it actually ROUTES.
	if _, err := f.worker.Tick(ctx); err != nil {
		t.Fatalf("worker Tick: %v", err)
	}
	if f.security.count() != 1 {
		t.Fatalf("the security actor received %d invocations, want 1 — affinity did not route", f.security.count())
	}
	if f.general.count() != 0 {
		t.Fatalf("the general actor received %d invocations; the node's declared `uses` won over the affinity",
			f.general.count())
	}
}

// TestADifferentFindingRoutesToTheOtherDeclaredActor is the other half of the
// same claim: routing that always picks the same actor is not routing.
func TestADifferentFindingRoutesToTheOtherDeclaredActor(t *testing.T) {
	f := newUpkeepFixture(t)
	ctx := context.Background()

	base := time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC)
	*f.clock = base
	if _, err := f.store.CreateSchedule(ctx, storepg.CreateScheduleInput{
		NamespaceID: f.ns.ID, Name: "nightly-upkeep", EventName: "upkeep.finding",
		Emitter: "schedule:nightly-upkeep", Interval: 24 * time.Hour, FirstFireAt: base,
		Payload: json.RawMessage(`{"findings":[{"kind":"dependency","detail":"stale lockfile"}]}`),
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	if err := f.sched.Tick(ctx); err != nil {
		t.Fatalf("scheduler Tick: %v", err)
	}
	if _, err := f.worker.Tick(ctx); err != nil {
		t.Fatalf("worker Tick: %v", err)
	}
	if f.general.count() != 1 {
		t.Fatalf("the general actor received %d invocations, want 1", f.general.count())
	}
	if f.security.count() != 0 {
		t.Fatalf("a dependency finding reached the security actor %d times, want 0", f.security.count())
	}
}
