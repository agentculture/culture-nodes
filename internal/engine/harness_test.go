package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// The harness stands in for the two processes the engine does not contain: a
// worker that claims work and reports completions, and the publisher that
// puts a definition in front of it. Everything it does goes through the real
// claiming path (Store.ClaimWork, leases, fencing tokens), because a test
// that hand-wrote work_items rows would prove the engine works against a
// fixture rather than against the store it actually runs on.

const testLease = 2 * time.Minute

type fixture struct {
	t      *testing.T
	ctx    context.Context
	store  *storepg.Store
	ns     storepg.Namespace
	actor  string
	engine *engine.Engine
	cw     *compiler.CompiledWorkflow
}

func newFixture(t *testing.T, workflowFile string, opts ...engine.Option) *fixture {
	t.Helper()
	s := pgtest.RequireStore(t, testStore)
	return newFixtureOn(t, s, workflowFile, opts...)
}

func newFixtureOn(t *testing.T, s *storepg.Store, workflowFile string, opts ...engine.Option) *fixture {
	t.Helper()

	ns := pgtest.MustNamespace(t, s, "engine")
	f := &fixture{
		t:     t,
		ctx:   context.Background(),
		store: s,
		ns:    ns,
		cw:    compileFixture(t, workflowFile),
	}
	f.actor = f.insertActor("worker-agent")

	// Retry delays are zeroed so a retry test does not have to wait out a
	// real backoff; the backoff arithmetic itself is covered by a unit test.
	eng, err := storepg.NewEngine(s, ns.ID, append([]engine.Option{engine.WithRetryDelays(0, 0)}, opts...)...)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	f.engine = eng
	return f
}

// rebind returns a fixture that talks to the same namespace and definition
// through a *different* store and a brand-new engine — the shape a process
// restart leaves behind.
func (f *fixture) rebind(t *testing.T, s *storepg.Store, opts ...engine.Option) *fixture {
	t.Helper()
	eng, err := storepg.NewEngine(s, f.ns.ID, append([]engine.Option{engine.WithRetryDelays(0, 0)}, opts...)...)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return &fixture{t: t, ctx: f.ctx, store: s, ns: f.ns, actor: f.actor, engine: eng, cw: f.cw}
}

func compileFixture(t *testing.T, name string) *compiler.CompiledWorkflow {
	t.Helper()

	path := filepath.Join("testdata", name)
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	cw, diags, err := compiler.Compile(source, compiler.FormatForPath(path))
	if err != nil {
		t.Fatalf("compile %s: %v", path, err)
	}
	for _, d := range diags {
		if d.Level == compiler.LevelError {
			t.Fatalf("compile %s: %s at %s: %s", path, d.Code, d.Path, d.Message)
		}
	}
	return cw
}

func (f *fixture) insertActor(key string) string {
	f.t.Helper()
	id := store.NewULID()
	_, err := f.store.Pool().Exec(f.ctx,
		`INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		 VALUES ($1, $2, $3, 1, 'agent', 'http')`,
		id, f.ns.ID, key+"-"+id)
	if err != nil {
		f.t.Fatalf("insert actor: %v", err)
	}
	return id
}

func (f *fixture) createRun(input string) engine.Run {
	f.t.Helper()
	run, err := f.engine.CreateRun(f.ctx, f.cw, json.RawMessage(input))
	if err != nil {
		f.t.Fatalf("CreateRun: %v", err)
	}
	return run
}

// claim claims the ready work item belonging to nodeRunID through the real
// claiming path.
//
// ClaimWork deliberately scans every ready row rather than filtering by
// namespace — it is the shared worker path — so anything it wins that this
// test did not ask for is handed straight back to 'ready'. Leaving it leased
// would make an earlier test's leftovers invisible to a later one.
func (f *fixture) claim(workerID, nodeRunID string) storepg.ClaimedWork {
	f.t.Helper()

	for attempt := 0; attempt < 20; attempt++ {
		claimed, err := f.store.ClaimWork(f.ctx, workerID, testLease, 20)
		if err != nil {
			f.t.Fatalf("ClaimWork: %v", err)
		}
		var found *storepg.ClaimedWork
		for i := range claimed {
			if claimed[i].NodeRunID == nodeRunID && found == nil {
				found = &claimed[i]
				continue
			}
			f.release(claimed[i].ID)
		}
		if found != nil {
			return *found
		}
		time.Sleep(25 * time.Millisecond)
	}
	f.t.Fatalf("no work item became claimable for node run %s", nodeRunID)
	return storepg.ClaimedWork{}
}

func (f *fixture) release(workID string) {
	f.t.Helper()
	_, err := f.store.Pool().Exec(f.ctx,
		`UPDATE work_items SET state = 'ready', lease_owner = NULL, lease_expires_at = NULL WHERE id = $1`, workID)
	if err != nil {
		f.t.Fatalf("release work item: %v", err)
	}
}

// expire pushes a lease into the past and reclaims it, which is exactly what
// the scheduler does when a worker dies (PRD §20.4).
func (f *fixture) expire(workID string) {
	f.t.Helper()
	if _, err := f.store.Pool().Exec(f.ctx,
		`UPDATE work_items SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, workID,
	); err != nil {
		f.t.Fatalf("expire lease: %v", err)
	}
	if _, err := f.store.ReclaimExpired(f.ctx); err != nil {
		f.t.Fatalf("ReclaimExpired: %v", err)
	}
}

// completion fills the fencing tuple from a claim, so a test states only what
// the actor produced.
func completion(claimed storepg.ClaimedWork, workerID string, req engine.CompletionRequest) engine.CompletionRequest {
	req.WorkID = claimed.ID
	req.WorkerID = workerID
	req.FencingToken = claimed.FencingToken
	req.Attempt = int(claimed.Attempt)
	return req
}

// step claims the work for a node run, reports the given completion, and
// returns the committed result.
func (f *fixture) step(workerID, nodeRunID string, req engine.CompletionRequest) engine.CompletionResult {
	f.t.Helper()
	claimed := f.claim(workerID, nodeRunID)
	result, err := f.engine.CompleteAttempt(f.ctx, completion(claimed, workerID, req))
	if err != nil {
		f.t.Fatalf("CompleteAttempt for node run %s: %v", nodeRunID, err)
	}
	return result
}

func succeeded(outcome, output string) engine.CompletionRequest {
	return engine.CompletionRequest{
		TechStatus: engine.StatusSucceeded,
		Outcome:    outcome,
		Output:     json.RawMessage(output),
	}
}

// eventTypes returns a run's audit event types in sequence order, and fails
// the test if the sequence is not 1..N with no gaps or repeats.
func (f *fixture) eventTypes(runID string) []string {
	f.t.Helper()

	rows, err := f.store.Pool().Query(f.ctx,
		`SELECT sequence, event_type, aggregate_type FROM events WHERE aggregate_id = $1 ORDER BY sequence`, runID)
	if err != nil {
		f.t.Fatalf("read events: %v", err)
	}
	defer rows.Close()

	var types []string
	expected := int64(1)
	for rows.Next() {
		var (
			sequence      int64
			eventType     string
			aggregateType string
		)
		if err := rows.Scan(&sequence, &eventType, &aggregateType); err != nil {
			f.t.Fatalf("scan event: %v", err)
		}
		if sequence != expected {
			f.t.Fatalf("event sequence for run %s jumped: got %d, want %d", runID, sequence, expected)
		}
		if aggregateType != "run" {
			f.t.Errorf("event %d has aggregate type %q, want run", sequence, aggregateType)
		}
		expected++
		types = append(types, eventType)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("read events: %v", err)
	}
	return types
}

func (f *fixture) countScalar(query string, args ...any) int {
	f.t.Helper()
	var count int
	if err := f.store.Pool().QueryRow(f.ctx, query, args...).Scan(&count); err != nil {
		f.t.Fatalf("query %q: %v", query, err)
	}
	return count
}

func (f *fixture) nodeRunCount(runID, nodeKey string) int {
	f.t.Helper()
	return f.countScalar(`SELECT COUNT(*)::int FROM node_runs WHERE run_id = $1 AND node_key = $2`, runID, nodeKey)
}

// readyNodeRun is the node run currently waiting for a worker — the question
// a worker asks, answered from the authoritative table.
func (f *fixture) readyNodeRun(runID string) engine.NodeRun {
	f.t.Helper()
	var id string
	err := f.store.Pool().QueryRow(f.ctx,
		`SELECT id FROM node_runs WHERE run_id = $1 AND status = 'ready' ORDER BY created_at DESC, id DESC LIMIT 1`,
		runID,
	).Scan(&id)
	if err != nil {
		f.t.Fatalf("no ready node run for run %s: %v", runID, err)
	}
	return f.nodeRun(id)
}

func (f *fixture) run(runID string) engine.Run {
	f.t.Helper()
	run, err := f.engine.Store().Run(f.ctx, runID)
	if err != nil {
		f.t.Fatalf("read run %s: %v", runID, err)
	}
	return run
}

func (f *fixture) nodeRun(nodeRunID string) engine.NodeRun {
	f.t.Helper()
	nodeRun, err := f.engine.Store().NodeRun(f.ctx, nodeRunID)
	if err != nil {
		f.t.Fatalf("read node run %s: %v", nodeRunID, err)
	}
	return nodeRun
}

func equalJSON(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("decode %s: %v", want, err)
	}
	gotCanonical, _ := json.Marshal(a)
	wantCanonical, _ := json.Marshal(b)
	if string(gotCanonical) != string(wantCanonical) {
		t.Errorf("payload = %s, want %s", gotCanonical, wantCanonical)
	}
}

func equalStrings(t *testing.T, got, want []string, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s =\n  %v\nwant\n  %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s =\n  %v\nwant\n  %v", what, got, want)
		}
	}
}
