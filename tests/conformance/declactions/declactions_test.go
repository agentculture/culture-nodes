package declactions_test

// Action-kind conformance through the real registry (issue #328, task t29;
// spec c5/h38): every action kind a declaration can name -- agent work, a
// Discord post, a GitHub message, a Jira comment/transition/create, and a
// code run -- is dispatched by the REAL declaration engine through the REAL
// WorkerDispatcher and worker envelope, resolved by the REAL actors-table
// registry (worker.DBRegistry, capabilities and all) or the real runner
// service registry, and executed by the real worker loop. The one stand-in is
// the far side of the wire: a fake bridge (or runner service) HTTP endpoint,
// because the external service is somebody else's process.
//
// What each case pins:
//
//   - the request reaches exactly the registered actor (or runner) the
//     action's `uses` names, and no other registered bridge;
//   - the rendered input arrives intact, including the minted cn1 marker
//     under the key every bridge's stamping.py reads (declengine.
//     MarkerInputKey). The fake bridge reads that key exactly the way
//     adapters/*/src/*/stamping.py read_marker does, so the old
//     `origin_marker` spelling makes these tests fail rather than pass
//     around a stub;
//   - the result comes back into the firing: the run completes, the
//     artifact id the bridge reported is bound to the firing's marker (a
//     reaction carrying that marker and artifact resolves to this firing as
//     its parent), and the action's output is visible as lineage variables.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/decl/kinds"
	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
	"github.com/agentculture/culture-nodes/internal/worker"
)

var declStore *postgres.Store

// TestMain gives the declaration-action cases a real, migrated PostgreSQL.
// Without NODES_TEST_DATABASE_URL (or Docker) those cases skip; the flag-gated
// endpoint kit and the in-process reference check are unaffected.
func TestMain(m *testing.M) {
	os.Exit(pgtest.Run(m, func(s *postgres.Store) { declStore = s }))
}

// cn1Marker is stamping.py's _MARKER, character for character: a bridge
// refuses (400) a present marker that does not match it.
var cn1Marker = regexp.MustCompile(`^cn1:[^:]{1,256}:[^:]{1,256}:[0-9a-fA-F]{48}:[0-9a-fA-F]{64}$`)

const (
	declMarkerKeyEnv = "TCA_CONFORMANCE_MARKER_KEY"
	bridgeTokenEnv   = "TCA_CONFORMANCE_BRIDGE_TOKEN"
	bridgeToken      = "conformance-bridge-token"
	// The runner-service identity a code.run action's `uses` names, and the
	// image digest its operation pins (the pair internal/worker's own
	// runner-service tests register).
	codeRunnerRef    = "runner://headspace/docker@sha256:5555555555555555555555555555555555555555555555555555555555555555"
	codeImageDigest  = "sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de"
	runnerSecretRef  = "runner/conformance/execute-token"
	runnerSecret     = "conformance-execute-token"
	codeRunnerName   = "headspace"
	codeRunnerDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// bridgeKeys are the bridge actors every case registers, so each dispatch
// has somewhere WRONG it could have gone.
var bridgeKeys = []string{"culture/codex-thor", "culture/notify", "culture/github", "culture/jira"}

// fakeBridge stands in for a bridge's /v1/invoke. It does what every real
// bridge's stamping contract does: read `input[MarkerInputKey]`, refuse a
// malformed one, and report {artifact_id, marker} for what it "created".
type fakeBridge struct {
	mu     sync.Mutex
	key    string
	inputs []map[string]any
	server *httptest.Server
}

func newFakeBridge(t *testing.T, key string) *fakeBridge {
	t.Helper()
	b := &fakeBridge{key: key}
	b.server = httptest.NewServer(http.HandlerFunc(b.invoke))
	t.Cleanup(b.server.Close)
	return b
}

func (b *fakeBridge) invoke(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+bridgeToken {
		http.Error(w, `{"error":"a scoped workload token is required"}`, http.StatusUnauthorized)
		return
	}
	var req actors.InvocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var input map[string]any
	_ = json.Unmarshal(req.Input, &input)
	b.mu.Lock()
	b.inputs = append(b.inputs, input)
	b.mu.Unlock()
	output := map[string]any{"bridge": b.key}
	// stamping.read_marker: absent -> nothing to stamp; present and
	// malformed -> 400; present and well formed -> stamp and report.
	if raw, present := input[declengine.MarkerInputKey]; present && raw != nil {
		marker, ok := raw.(string)
		if !ok || !cn1Marker.MatchString(marker) {
			http.Error(w, `{"error":"invalid cn1 marker","class":"actor_rejected_input"}`, http.StatusBadRequest)
			return
		}
		output["artifact_id"] = "artifact-" + strings.ReplaceAll(b.key, "/", "-")
		output["marker"] = marker
	}
	body, _ := json.Marshal(output)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(actors.InvocationResult{Outcome: "completed", Output: body})
}

func (b *fakeBridge) received() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]map[string]any(nil), b.inputs...)
}

// fakeRunner speaks api/runner-protocol: 202 on execute, a terminal result
// (exit 0 unless exitCode says otherwise) on the first status sample.
type fakeRunner struct {
	mu  sync.Mutex
	ops []runners.Operation
	// exitCode is what every operation exits with (0 unless a case sets it).
	exitCode int
}

func (f *fakeRunner) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+runnerSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == runners.OperationsPath:
			var op runners.Operation
			if err := json.NewDecoder(r.Body).Decode(&op); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.ops = append(f.ops, op)
			f.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(runners.Acceptance{OperationID: op.OperationID, StatusRetentionSeconds: 86400})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, runners.OperationsPath+"/"):
			id := strings.TrimPrefix(r.URL.Path, runners.OperationsPath+"/")
			f.mu.Lock()
			var op *runners.Operation
			for i := range f.ops {
				if f.ops[i].OperationID == id {
					op = &f.ops[i]
				}
			}
			f.mu.Unlock()
			if op == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			result := exitWith(*op, f.exitCode)
			_ = json.NewEncoder(w).Encode(runners.OperationStatus{OperationID: id, State: result.State, Result: &result})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (f *fakeRunner) operations() []runners.Operation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runners.Operation(nil), f.ops...)
}

func exitWith(op runners.Operation, code int) runners.Result {
	finished := time.Now().UTC()
	unmeasured := runners.Observation{}
	return runners.Result{
		OperationID: op.OperationID, State: runners.StateCompleted, Exit: &runners.Exit{Code: &code},
		Timing: runners.Timing{StartedAt: finished.Add(-30 * time.Millisecond), FinishedAt: finished, DurationMs: 30},
		Environment: runners.Environment{RunnerRevision: op.RunnerRevision, ImageDigest: op.Execution.ImageDigest,
			PolicyDigest: "sha256:" + strings.Repeat("c", 64), PlatformRequestID: "ws_conformance"},
		Observations: runners.Observations{
			ExitStatus:   runners.Observation{Measured: true, Complete: true, Method: "container_wait_status"},
			Logs:         runners.Observation{Measured: true, Complete: true, Method: "container_log_capture"},
			ChangedPaths: unmeasured, ResourceUsage: unmeasured,
		},
	}
}

// declHarness is one namespace with the full cast registered.
type declHarness struct {
	t       *testing.T
	ctx     context.Context
	db      *postgres.Store
	ns      string
	bridges map[string]*fakeBridge
	runner  *fakeRunner
	engine  *declengine.Engine
	worker  *worker.Worker
}

func newDeclHarness(t *testing.T) *declHarness {
	t.Helper()
	db := pgtest.RequireStore(t, declStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "decl-actions").ID
	t.Setenv(declMarkerKeyEnv, strings.Repeat("m", 32))
	t.Setenv(bridgeTokenEnv, bridgeToken)
	h := &declHarness{t: t, ctx: ctx, db: db, ns: ns, bridges: map[string]*fakeBridge{}, runner: &fakeRunner{}}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Pool().Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// actors.id is global across the shared database; generate every id.
	producer, runnerActor := store.NewULID(), store.NewULID()
	exec(`INSERT INTO actors(id,namespace_id,actor_key,revision,kind,protocol) VALUES($1,$2,'engine/declarations',1,'validator','internal')`, producer, ns)
	exec(`INSERT INTO actors(id,namespace_id,actor_key,revision,kind,protocol) VALUES($1,$2,$3,1,'runner','internal')`, runnerActor, ns, "runner/"+runnerActor)
	// Each bridge is registered the way deploy/prod/register-actor.sh
	// registers one: an endpoint, a credential by env-var NAME, and the
	// capability facts its /v1/capabilities advertises -- including the
	// stamping block (stamping.CAPABILITY) t38's refusal reads.
	for _, key := range bridgeKeys {
		b := newFakeBridge(t, key)
		h.bridges[key] = b
		exec(`INSERT INTO actors(id,namespace_id,actor_key,revision,kind,protocol,endpoint_ref,metadata,capabilities) VALUES($1,$2,$3,1,'agent','nodes.actor/v1alpha1',$4,$5,'{"stamping":{"marker":"cn1","version":1}}')`,
			store.NewULID(), ns, key, b.server.URL, `{"auth_token_env":"`+bridgeTokenEnv+`"}`)
	}

	runnerServer := httptest.NewServer(h.runner.handler())
	t.Cleanup(runnerServer.Close)
	runnerRegistry := runners.NewFunctionRegistry()
	if err := runnerRegistry.RegisterService(codeRunnerRef, runners.ServiceIdentity{Endpoint: runnerServer.URL, ImageDigest: codeImageDigest,
		SecretRef: runnerSecretRef, AllowInsecureTransport: true}); err != nil {
		t.Fatal(err)
	}
	client, err := runners.NewProtocolClient(runners.StaticSecrets{runnerSecretRef: runnerSecret})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := worker.NewDBRegistry(db, ns)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := postgres.NewEngine(db, ns)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := actors.NewTokenSigner([]byte(strings.Repeat("s", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h.worker, err = worker.New(db, graph, worker.Options{NamespaceID: ns, WorkerID: "conformance-" + t.Name(), Signer: signer,
		CallbackBaseURL: "http://127.0.0.1:1", Registry: registry,
		CodeRunnerName: codeRunnerName, CodeRunnerActorID: runnerActor, CodeRunnerRevision: codeRunnerDigest,
		RunnerService: worker.RunnerServiceOptions{Registry: runnerRegistry, Client: client, PollInterval: 10 * time.Millisecond, DisableCallback: true},
		OnError:       func(err error) { t.Error(err) }})
	if err != nil {
		t.Fatal(err)
	}
	h.engine, err = declengine.New(declengine.Config{MarkerKeyEnv: declMarkerKeyEnv}, declengine.PostgresBackend{Store: db},
		declengine.PostgresMarkerStore{Store: db}, declengine.WorkerDispatcher{Store: db, ProducerActorID: producer})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// approveWidenings records the owner's approval of every step-0 reference
// that renders the timer's variables into a wider audience (task t30: e.g.
// into Discord or GitHub), exactly as the owner would through the
// sensitivity inbox -- so this test keeps exercising dispatch through the
// registry rather than the sensitivity gate in front of it.
func (h *declHarness) approveWidenings(v postgres.DeclarationVersion, d decl.Declaration) {
	t := h.t
	t.Helper()
	refs, err := decl.ActionReferences(d.Action)
	if err != nil {
		t.Fatal(err)
	}
	source, target := decl.TriggerSensitivity(d), decl.TargetSensitivity(d.Action.Kind)
	if !decl.Widens(source, target) {
		return
	}
	for _, ref := range refs {
		a, err := (declengine.PostgresBackend{Store: h.db}).RequestSensitivityApproval(h.ctx, declengine.SensitivityApprovalRequest{
			NamespaceID: h.ns, DeclarationID: v.DeclarationID, DeclarationVersionID: v.ID, SourceDeclarationID: v.DeclarationID,
			SourceVersionID: v.ID, Variable: ref.Name, Owner: v.Author, EventID: "conformance", Source: source, Target: target})
		if err != nil {
			t.Fatal(err)
		}
		if a.Status == declengine.SensitivityApproved {
			continue
		}
		if _, err := declengine.DecideSensitivityApproval(h.ctx, h.db, h.ns, a.ID,
			declengine.ActivationPrincipal{Kind: declengine.PrincipalHuman, Author: v.Author}, declengine.SensitivityApproved, "conformance"); err != nil {
			t.Fatal(err)
		}
	}
}

// fire publishes and activates one declaration whose action is kind/with,
// delivers the event that triggers it, and drives the resulting run to a
// terminal state through the worker. It returns the firing id.
func (h *declHarness) fire(kind string, with string) string {
	t := h.t
	t.Helper()
	d := decl.Declaration{Name: "conformance-" + strings.NewReplacer(".", "-", "_", "-").Replace(kind),
		Trigger:   decl.Trigger{Kind: "timer", ReentryLimit: 3, HopLimit: 20, RateCeiling: "30/h"},
		Condition: "true", Action: decl.Action{Kind: kind, With: json.RawMessage(with)},
		StartNode: decl.Node{Name: "ready", Deadline: "none"}, LandingNode: decl.Node{Name: "done", Deadline: "1h"}}
	body, err := d.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	v, err := h.db.PublishDeclaration(h.ctx, postgres.PublishDeclarationInput{NamespaceID: h.ns, Name: d.Name, Body: body, Author: "human"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.RecordDeclarationActivation(h.ctx, h.ns, v.ID, "activate", "human", ""); err != nil {
		t.Fatal(err)
	}
	h.approveWidenings(v, d)
	ev, err := h.db.DeliverSignalEvent(h.ctx, postgres.DeliverSignalEventInput{NamespaceID: h.ns, Name: "timer", Emitter: "conformance"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Handle(h.ctx, declengine.Event{NamespaceID: h.ns, ID: ev.Event.ID, Kind: "timer", Node: "ready",
		Variables: map[string]any{"subject": "SCRUM-7", "note": "rendered-from-event"}}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var firing string
	if err := h.db.Pool().QueryRow(h.ctx, `SELECT id FROM declaration_firings WHERE namespace_id=$1`, h.ns).Scan(&firing); err != nil {
		t.Fatalf("no firing was claimed: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if err := h.db.Pool().QueryRow(h.ctx, `SELECT status FROM runs WHERE namespace_id=$1 AND id=$2`, h.ns, firing).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "completed" {
			return firing
		}
		if state != "running" {
			t.Fatalf("firing run %s ended %s, want completed", firing, state)
		}
		if _, err := h.worker.Tick(h.ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := h.worker.SampleRunnerOperations(h.ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("firing run %s did not complete", firing)
	return ""
}

// assertResultInFiring checks that the action's output came back into the
// firing: visible as the firing's lineage variables once reconciled, and --
// for an artifact-creating bridge action -- bound to the firing's marker so
// a reaction carrying it inherits this firing.
func (h *declHarness) assertResultInFiring(firing, marker, artifactKind, artifactID string, want map[string]any) {
	t := h.t
	t.Helper()
	if err := h.engine.Reconcile(h.ctx, h.db, h.ns, firing); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	lineage, err := (declengine.PostgresBackend{Store: h.db}).Lineage(h.ctx, h.ns, firing)
	if err != nil {
		t.Fatal(err)
	}
	if len(lineage) != 1 {
		t.Fatalf("lineage = %+v, want exactly this firing", lineage)
	}
	for k, v := range want {
		if got := lineage[0].Variables[k]; got != v {
			t.Errorf("firing variable %q = %v, want %v (variables %v)", k, got, v, lineage[0].Variables)
		}
	}
	if artifactID == "" {
		return
	}
	markers, err := declengine.NewMarkerService([]byte(os.Getenv(declMarkerKeyEnv)), declengine.PostgresMarkerStore{Store: h.db})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := markers.Resolve(h.ctx, declengine.OriginEvent{NamespaceID: h.ns, EventID: "reaction-" + firing,
		Marker: marker, ArtifactKind: artifactKind, ArtifactID: artifactID})
	if err != nil {
		t.Fatal(err)
	}
	if parent != firing {
		t.Fatalf("a reaction on %s carrying the stamped marker resolved to parent %q, want firing %s", artifactID, parent, firing)
	}
}

type bridgeCase struct {
	kind, uses, artifact string
	input                map[string]any
}

// bridgeCases is one row per bridge-backed action kind: which registered
// actor it names, what artifact kind its marker is minted for, and an input
// shaped like that bridge's verb, with a template rendered from the event.
var bridgeCases = []bridgeCase{
	{"agent.work", "culture/codex-thor", "github.pr", map[string]any{"instruction": "fix {0:subject}", "note": "{0:note}"}},
	{"discord.post", "culture/notify", "discord.message", map[string]any{"channel": "ops", "content": "{0:subject} moved", "note": "{0:note}"}},
	{"github.comment", "culture/github", "github.comment", map[string]any{"repository": "agentculture/culture-nodes", "pr": 328, "comment": "{0:subject} looks good", "note": "{0:note}"}},
	{"github.review_reply", "culture/github", "github.comment", map[string]any{"repository": "agentculture/culture-nodes", "pr": 328, "comment": "addressed", "note": "{0:note}"}},
	{"jira.comment", "culture/jira", "jira.comment", map[string]any{"issue": "{0:subject}", "body": "picked up", "note": "{0:note}"}},
	{"jira.transition", "culture/jira", "jira.issue", map[string]any{"issue": "{0:subject}", "to": "In Progress", "note": "{0:note}"}},
	{"jira.create", "culture/jira", "jira.issue", map[string]any{"project": "SCRUM", "summary": "follow up {0:subject}", "note": "{0:note}"}},
}

func TestDeclActionKindsDispatchThroughRealRegistry(t *testing.T) {
	for _, c := range bridgeCases {
		t.Run(c.kind, func(t *testing.T) {
			h := newDeclHarness(t)
			with, err := json.Marshal(map[string]any{"uses": "actor://" + c.uses + "@sha256:" + strings.Repeat("a", 64), "input": c.input})
			if err != nil {
				t.Fatal(err)
			}
			firing := h.fire(c.kind, string(with))

			// Exactly the named actor was invoked, once; no other bridge was.
			for key, b := range h.bridges {
				n := len(b.received())
				if key == c.uses && n != 1 {
					t.Fatalf("%s invoked %d times, want exactly once", key, n)
				}
				if key != c.uses && n != 0 {
					t.Fatalf("%s dispatched to %s as well as %s", c.kind, key, c.uses)
				}
			}
			got := h.bridges[c.uses].received()[0]
			if got["note"] != "rendered-from-event" {
				t.Errorf("bridge input lost the rendered template: %v", got)
			}
			marker, _ := got[declengine.MarkerInputKey].(string)
			if !cn1Marker.MatchString(marker) || !strings.HasPrefix(marker, "cn1:"+firing+":"+c.artifact+":") {
				t.Fatalf("bridge read input[%q] = %q, want the cn1 marker minted for firing %s and artifact %s (input %v)",
					declengine.MarkerInputKey, marker, firing, c.artifact, got)
			}
			artifactID := "artifact-" + strings.ReplaceAll(c.uses, "/", "-")
			h.assertResultInFiring(firing, marker, c.artifact, artifactID, map[string]any{"artifact_id": artifactID, "bridge": c.uses, "subject": "SCRUM-7"})
		})
	}
}

// A code run goes to a runner:// target through the runner-service registry;
// the marker rides into the sandbox with the rest of the rendered input as
// NODES_INPUT_JSON (runners.EnvInputJSON) and nothing is stamped.
func TestDeclCodeRunDispatchesThroughRunnerService(t *testing.T) {
	h := newDeclHarness(t)
	with := `{"uses":"` + codeRunnerRef + `","input":{"note":"{0:note}"},"operation":{"image":"python:3.12-slim@` + codeImageDigest +
		`","argv":["python3","-V"],"network":"none","allowedOutputPaths":[]}}`
	firing := h.fire("code.run", with)
	for key, b := range h.bridges {
		if n := len(b.received()); n != 0 {
			t.Fatalf("code.run reached bridge %s %d times; it must go to the runner", key, n)
		}
	}
	ops := h.runner.operations()
	if len(ops) != 1 {
		t.Fatalf("runner received %d operations, want 1", len(ops))
	}
	if ops[0].Execution.ImageDigest != codeImageDigest || ops[0].Context == nil || ops[0].Context.RunID != firing {
		t.Fatalf("operation = %+v, want the pinned image for run %s", ops[0], firing)
	}
	var input map[string]any
	if err := json.Unmarshal(ops[0].Input, &input); err != nil {
		t.Fatalf("operation input %s: %v", ops[0].Input, err)
	}
	marker, _ := input[declengine.MarkerInputKey].(string)
	if input["note"] != "rendered-from-event" || !strings.HasPrefix(marker, "cn1:"+firing+":code.result:") {
		t.Fatalf("runner input = %v, want the rendered input and the firing's code.result marker", input)
	}
	h.assertResultInFiring(firing, "", "", "", map[string]any{"state": "completed", "subject": "SCRUM-7"})
}

// Every registered action kind is either conformance-tested above or named
// here with the reason it is not a registry dispatch. A new kind fails this
// until someone decides which.
func TestDeclActionKindsAreAllCovered(t *testing.T) {
	notRegistryDispatch := map[string]string{
		"human.ask": "served by the control plane's own human-task surface, not a registered actor",
		"activate":  "a catalog move on a declaration, not an external action",
	}
	covered := map[string]bool{"code.run": true}
	for _, c := range bridgeCases {
		covered[c.kind] = true
	}
	var missing []string
	for _, k := range kinds.Actions() {
		if !covered[k.Name] && notRegistryDispatch[k.Name] == "" {
			missing = append(missing, k.Name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("action kinds with no conformance case: %v", missing)
	}
}

// The engine and the bridges must agree on the key byte for byte: the Go side
// writes declengine.MarkerInputKey, and every bridge's (byte-identical)
// stamping.py reads it. This is the cross-language half of the check the
// fake bridge above makes at runtime.
func TestBridgeStampingReadsEngineMarkerKey(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "..", "adapters", "*", "src", "*", "stamping.py"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no adapters/*/src/*/stamping.py found")
	}
	want := `raw_input.get("` + declengine.MarkerInputKey + `")`
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), want) {
			t.Errorf("%s does not read %s: the engine writes the marker under %q", p, want, declengine.MarkerInputKey)
		}
	}
}
