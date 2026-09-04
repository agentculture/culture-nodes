package e2etest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// harnessCompareWorkflowPath is plan task t5's example (spec
// harness-interop-pi-qwen, claim c31 / honesty condition h26): one request
// fanned to several harnesses and joined into one result.
const harnessCompareWorkflowPath = "../../examples/harness-compare/workflow.yaml"

// harnessCompareSlots are the four fixed actor slots the workflow declares.
// The workflow language cannot fan out over a run-time list (`uses:` is a
// static registry id per node), so the example fans over a fixed set of
// slots and the run input says which of them run — see the example's README.
var harnessCompareSlots = []string{"claude", "codex", "pi", "qwen"}

// harnessCompareSlotActorKeys are the registry ids the workflow places each
// slot on, digest suffix stripped — what a deployment's actors table has to
// carry for the slot to dispatch.
var harnessCompareSlotActorKeys = map[string]string{
	"claude": "company/developer",
	"codex":  "company/codex-thor",
	"pi":     "company/pi-thor",
	"qwen":   "company/qwen-thor",
}

// compiledHarnessCompare is the slice of the normalized IR the structural
// assertions below read. Only the asserted fields are declared, for the
// reason exampleportability_test.go's portableDoc gives.
type compiledHarnessCompare struct {
	Spec struct {
		Entry string `json:"entry"`
		Nodes map[string]struct {
			Kind string `json:"kind"`
			Uses string `json:"uses"`
			Join *struct {
				Policy string `json:"policy"`
			} `json:"join"`
			Output *struct {
				From string `json:"from"`
			} `json:"output"`
		} `json:"nodes"`
		Edges []struct {
			From string `json:"from"`
			To   string `json:"to"`
			When string `json:"when"`
		} `json:"edges"`
	} `json:"spec"`
}

// compileHarnessCompareExample compiles the example twice through the same
// path `nodes validate` runs, asserts it compiles clean, identically and
// under the name the README documents, and returns the slice of the
// normalized IR the structural assertions read.
func compileHarnessCompareExample(t *testing.T) compiledHarnessCompare {
	t.Helper()
	source, err := os.ReadFile(filepath.Clean(harnessCompareWorkflowPath))
	if err != nil {
		t.Fatalf("read %s: %v", harnessCompareWorkflowPath, err)
	}

	compiled, diags, err := compiler.Compile(source, compiler.FormatYAML)
	if err != nil {
		t.Fatalf("Compile returned an internal error: %v", err)
	}
	if compiled == nil {
		t.Fatalf("the harness-compare workflow did not compile: %+v", diags)
	}
	for _, d := range diags {
		t.Errorf("unexpected %s diagnostic at %s: %s — %s", d.Level, d.Path, d.Code, d.Message)
	}
	if compiled.Name != "harness-compare" || compiled.Version != "1.0.0" {
		t.Errorf("name/version = %q/%q, want harness-compare/1.0.0", compiled.Name, compiled.Version)
	}

	second, _, err := compiler.Compile(source, compiler.FormatYAML)
	if err != nil || second == nil {
		t.Fatalf("second Compile: %v", err)
	}
	if second.Digest != compiled.Digest {
		t.Errorf("digest differs between compilations: %s vs %s", compiled.Digest, second.Digest)
	}
	if !bytes.Equal(second.Normalized, compiled.Normalized) {
		t.Error("normalized IR differs between compilations of identical source")
	}

	var ir compiledHarnessCompare
	if err := json.Unmarshal(compiled.Normalized, &ir); err != nil {
		t.Fatalf("decode normalized IR: %v", err)
	}
	return ir
}

// harnessCompareSlotEdges scans the IR's edges once for the two a slot needs:
// the split edge into it, and any edge out of it that lands on the join. The
// split edge's guard is asserted here because the edge is where it lives — an
// unset slot must be skipped, not dispatched.
func harnessCompareSlotEdges(t *testing.T, ir compiledHarnessCompare, slot string) (splitEdge, joinEdge bool) {
	t.Helper()
	wantGuard := fmt.Sprintf("has(input.actors.%s)", slot)
	for _, e := range ir.Spec.Edges {
		if e.From == ir.Spec.Entry+".split" && e.To == slot {
			splitEdge = true
			if strings.TrimSpace(e.When) != wantGuard {
				t.Errorf("split edge into %q is guarded by %q, want %q: an unset slot must be skipped, not dispatched", slot, e.When, wantGuard)
			}
		}
		if strings.HasPrefix(e.From, slot+".") && ir.Spec.Nodes[e.To].Kind == "join" {
			joinEdge = true
		}
	}
	return splitEdge, joinEdge
}

// assertHarnessCompareSlot checks one harness slot: an agent node on its
// documented registry id, reached by a guarded split edge, and routing its
// outcomes into the join.
func assertHarnessCompareSlot(t *testing.T, ir compiledHarnessCompare, slot string) {
	t.Helper()
	node, ok := ir.Spec.Nodes[slot]
	if !ok {
		t.Errorf("no node %q: every harness slot must exist so a loader can register an actor behind it", slot)
		return
	}
	if node.Kind != "agent" {
		t.Errorf("slot %q is kind %q, want agent", slot, node.Kind)
	}
	if id, _, _ := strings.Cut(node.Uses, "@"); id != "actor://"+harnessCompareSlotActorKeys[slot] {
		t.Errorf("slot %q uses %q, want actor://%s (the registry id the README documents)", slot, node.Uses, harnessCompareSlotActorKeys[slot])
	}
	splitEdge, joinEdge := harnessCompareSlotEdges(t, ir, slot)
	if !splitEdge {
		t.Errorf("no edge from %s.split into slot %q", ir.Spec.Entry, slot)
	}
	if !joinEdge {
		t.Errorf("slot %q routes no outcome into the join, so its result could never be compared", slot)
	}
}

// harnessCompareJoinNode returns the id of the graph's single join node,
// having checked its policy waits for every actor the fan-out asked.
func harnessCompareJoinNode(t *testing.T, ir compiledHarnessCompare) string {
	t.Helper()
	var joins []string
	for id, node := range ir.Spec.Nodes {
		if node.Kind != "join" {
			continue
		}
		joins = append(joins, id)
		if node.Join == nil || node.Join.Policy != "all" {
			t.Errorf("join %q policy = %+v, want all: a comparison waits for every actor it asked", id, node.Join)
		}
	}
	if len(joins) != 1 {
		t.Fatalf("join nodes = %v, want exactly one", joins)
	}
	return joins[0]
}

// assertHarnessCompareEnd checks the graph has exactly one end node and that
// it emits the join's arrival array as the run's result.
func assertHarnessCompareEnd(t *testing.T, ir compiledHarnessCompare, joinID string) {
	t.Helper()
	var ends int
	for id, node := range ir.Spec.Nodes {
		if node.Kind != "end" {
			continue
		}
		ends++
		if node.Output == nil || node.Output.From != "/nodes/"+joinID+"/output" {
			t.Errorf("end node %q emits %+v, want /nodes/%s/output: the run's result must be the joined arrival array", id, node.Output, joinID)
		}
	}
	if ends != 1 {
		t.Errorf("end nodes = %d, want exactly one", ends)
	}
}

// TestHarnessCompareWorkflowCompilesCleanlyAndDeterministically needs no
// database — it is the deterministic compile-level half of t5's acceptance:
// the example compiles through the same path `nodes validate` runs, twice to
// the same digest, and its graph has the shape the README promises: a
// parallel entry, one agent slot per harness each behind a guard on the
// run input's `actors` map, a join with policy all, and an end node that
// emits the join's arrival array. Each of those properties is asserted by a
// helper above, one property per helper — the single-function form of this
// test was over SonarCloud's cognitive-complexity ceiling (go:S3776, 49 > 15).
func TestHarnessCompareWorkflowCompilesCleanlyAndDeterministically(t *testing.T) {
	ir := compileHarnessCompareExample(t)

	if fan := ir.Spec.Nodes[ir.Spec.Entry]; fan.Kind != "parallel" {
		t.Errorf("entry node %q is kind %q, want parallel: the fan-out must be the first thing a run does", ir.Spec.Entry, fan.Kind)
	}
	for _, slot := range harnessCompareSlots {
		assertHarnessCompareSlot(t, ir, slot)
	}
	assertHarnessCompareEnd(t, ir, harnessCompareJoinNode(t, ir))
}

// harnessCompareActors is one HTTP server standing in for every actor slot
// the fixture run names. It speaks the real §13 actor protocol; the only
// scripted part is each harness's "work". Every slot reports a different
// model, a different bridge-measured change set and a different handover
// ref, so the joined result can be checked for keeping them apart.
type harnessCompareActors struct {
	server *httptest.Server

	mu       sync.Mutex
	actorIDs map[string]string // slot (node id) -> registered actors.id
	requests []actors.InvocationRequest
	failures []string
}

// harnessCompareScript is what each fake harness answers with. The model
// string, the measured change set and the handover ref are per slot on
// purpose: a joined result that carried the right count but the same
// values for both would not show the comparison keeps each harness in its
// own shape (spec decision q4).
var harnessCompareScript = map[string]struct {
	model        string
	changedFiles []string
	handoverRef  string
}{
	"codex": {
		model:        "fake-codex-model",
		changedFiles: []string{"README.md"},
		handoverRef:  "refs/culture-nodes/handover/codex-fixture",
	},
	"pi": {
		model:        "fake-pi-model",
		changedFiles: []string{"docs/guide.md", "README.md"},
		handoverRef:  "refs/culture-nodes/handover/pi-fixture",
	},
}

func newHarnessCompareActors(t *testing.T, actorIDs map[string]string) *harnessCompareActors {
	t.Helper()
	a := &harnessCompareActors{actorIDs: actorIDs}
	a.server = httptest.NewServer(http.HandlerFunc(a.handle))
	t.Cleanup(a.server.Close)
	return a
}

func (a *harnessCompareActors) URL() string { return a.server.URL }

func (a *harnessCompareActors) handle(w http.ResponseWriter, r *http.Request) {
	var req actors.InvocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad invocation", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	a.requests = append(a.requests, req)
	actorID := a.actorIDs[req.Node.ID]
	a.mu.Unlock()

	result, err := harnessCompareAnswer(req, actorID)
	if err != nil {
		a.mu.Lock()
		a.failures = append(a.failures, err.Error())
		a.mu.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (a *harnessCompareActors) invocations() []actors.InvocationRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]actors.InvocationRequest(nil), a.requests...)
}

func (a *harnessCompareActors) scriptFailures() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.failures...)
}

// harnessCompareAnswer builds one slot's §13.2 result in the shape every
// shipped bridge produces: the model's own output (summary + self-claimed
// changed_files), the bridge-measured `workspace_measured` block beside it,
// a usage block naming the model, a handover block naming the ref, and one
// proposed claim.
func harnessCompareAnswer(req actors.InvocationRequest, actorID string) (actors.InvocationResult, error) {
	script, ok := harnessCompareScript[req.Node.ID]
	if !ok {
		return actors.InvocationResult{}, fmt.Errorf("no script for slot %q", req.Node.ID)
	}
	output, _ := json.Marshal(map[string]any{
		"summary":       "did the work as " + req.Node.ID,
		"changed_files": script.changedFiles,
	})
	measured, _ := json.Marshal(map[string]any{
		"measured":      true,
		"changed_files": script.changedFiles,
		"head_before":   "aaaaaaa",
		"head_after":    "bbbbbbb",
	})
	claim, _ := json.Marshal(map[string]any{
		"statement": "completed the shared instruction as " + req.Node.ID,
	})
	model := script.model
	ref := script.handoverRef
	return actors.InvocationResult{
		Outcome: "completed",
		Output:  output,
		LedgerDelta: &actors.LedgerDelta{Records: []ledger.Record{{
			RecordType: ledger.RecordClaim,
			Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: actorID},
			Authority:  ledger.AuthorityProposed,
			Data:       claim,
		}}},
		Usage: &actors.Usage{
			InputTokens:  120,
			OutputTokens: 40,
			Model:        &model,
		},
		WorkspaceMeasured: measured,
		Handover: &actors.Handover{
			Attempted: true,
			Created:   true,
			Ref:       &ref,
		},
	}, nil
}

// registerHarnessCompareActors inserts the actors rows the fixture's two
// named slots resolve against — the harness-compare twin of
// registerNewsletterActors. Only the slots the run input names are
// registered: an unnamed slot must be skipped by its guard before the
// registry is ever consulted, so leaving it unregistered is part of what
// the run proves.
func registerHarnessCompareActors(t *testing.T, db *postgres.Store, namespaceID, agentsURL string, slots []string) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for _, slot := range slots {
		id := "actor_" + idstore.NewULID()
		if _, err := db.Pool().Exec(context.Background(), `
			INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref)
			VALUES ($1, $2, $3, 1, 'agent', 'http', $4)
		`, id, namespaceID, harnessCompareSlotActorKeys[slot], agentsURL); err != nil {
			t.Fatalf("register actor %s: %v", harnessCompareSlotActorKeys[slot], err)
		}
		ids[slot] = id
	}
	return ids
}

// harnessCompareJoined is the run output's documented shape: the join's
// arrival array (internal/worker/paralleljoin.go, design D5), each element
// carrying the slot's outcome and its node output — which for an agent node
// is the bridge output merged with the bridge-measured `workspace_measured`
// block (internal/worker/dispatch.go).
type harnessCompareJoined struct {
	Policy      string `json:"policy"`
	Cardinality int    `json:"cardinality"`
	Arrivals    []struct {
		FromNode string `json:"from_node"`
		Outcome  string `json:"outcome"`
		Output   struct {
			Summary           string `json:"summary"`
			WorkspaceMeasured struct {
				Measured     bool     `json:"measured"`
				ChangedFiles []string `json:"changed_files"`
			} `json:"workspace_measured"`
		} `json:"output"`
	} `json:"arrivals"`
}

// TestHarnessCompareFansOneInstructionToTwoActorsAndJoins is t5's fixture
// run: the harness-compare workflow, published and started through the real
// HTTP API and driven by the real engine and worker against a real
// PostgreSQL, with two fake actors behind two of its four slots. The run
// input names exactly those two slots; the other two are left unset and
// unregistered.
//
// What it checks, in the order the acceptance criteria name it: the same
// instruction reached both actors, each in its own checkout; the run ends
// completed with ONE joined result carrying both actors' outcomes and their
// bridge-measured change sets, kept apart per slot; the unset slots never
// ran; each slot's attempt carries the model that harness reported; and
// each actor's proposed claim stays proposed. What it also records, because
// the README says it: the handover ref is NOT in the joined result. It
// reaches the run as `observed` evidence only when a control plane
// configured with a handover remote fetches it (internal/handover) — the
// workflow language has no binding surface for it, so this test asserts the
// request asked for one and stops there.
func TestHarnessCompareFansOneInstructionToTwoActorsAndJoins(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "e2e-harness-compare")

	named := []string{"codex", "pi"}
	agentIDs := map[string]string{}
	agents := newHarnessCompareActors(t, agentIDs)
	for slot, id := range registerHarnessCompareActors(t, s, ns.ID, agents.URL(), named) {
		agentIDs[slot] = id
	}

	stack := startStack(t, stackConfig{
		namespaceID: ns.ID,
		agentsURL:   agents.URL(),
		runner:      &scriptedRunner{},
	})
	defer stack.stop()

	digest := stack.publishWorkflowFile(t, harnessCompareWorkflowPath)
	const instruction = "add a /healthz endpoint and say what you changed"
	runID := stack.createRun(t, digest, json.RawMessage(`{
		"instruction": "`+instruction+`",
		"sandbox": "workspace-write",
		"handover": true,
		"actors": {
			"codex": {"repo": "/srv/checkouts/codex/culture-nodes"},
			"pi":    {"repo": "/srv/checkouts/pi/culture-nodes"}
		}
	}`))

	view := stack.waitForTerminal(t, runID, 60*time.Second)
	if failures := agents.scriptFailures(); len(failures) > 0 {
		t.Fatalf("the fake actors refused an invocation: %v", failures)
	}
	if view.Run.State != string(engine.RunCompleted) {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", view.Run.State, stack.errors())
	}

	// 1. The same instruction reached every named slot, each in its own
	//    checkout, with the write posture and handover request the run asked
	//    for — the four things a comparison has to hold constant or vary on
	//    purpose.
	invocations := agents.invocations()
	if len(invocations) != len(named) {
		t.Fatalf("actor invocations = %d, want %d (one per named slot)", len(invocations), len(named))
	}
	seenRepo := map[string]string{}
	for _, inv := range invocations {
		var in struct {
			Instruction    string `json:"instruction"`
			Repo           string `json:"repo"`
			Sandbox        string `json:"sandbox"`
			SuccessOutcome string `json:"success_outcome"`
			Handover       bool   `json:"handover"`
		}
		if err := json.Unmarshal(inv.Input, &in); err != nil {
			t.Fatalf("slot %s input is not an object (%s): %v", inv.Node.ID, inv.Input, err)
		}
		if in.Instruction != instruction {
			t.Errorf("slot %s received instruction %q, want the run's %q", inv.Node.ID, in.Instruction, instruction)
		}
		if in.Sandbox != "workspace-write" || !in.Handover || in.SuccessOutcome != "completed" {
			t.Errorf("slot %s received sandbox/handover/success_outcome = %q/%v/%q, want workspace-write/true/completed", inv.Node.ID, in.Sandbox, in.Handover, in.SuccessOutcome)
		}
		if !strings.Contains(in.Repo, "/"+inv.Node.ID+"/") {
			t.Errorf("slot %s received repo %q, want its own slot's checkout", inv.Node.ID, in.Repo)
		}
		seenRepo[inv.Node.ID] = in.Repo
	}
	if seenRepo["codex"] == seenRepo["pi"] {
		t.Errorf("both slots received the same repo %q; each actor works in its own checkout", seenRepo["codex"])
	}

	// 2. One joined result, both actors' outcomes and measured change sets
	//    kept apart per slot.
	var joined harnessCompareJoined
	if err := json.Unmarshal(view.Run.Output, &joined); err != nil {
		t.Fatalf("run output is not the join's arrival array (%s): %v", view.Run.Output, err)
	}
	if joined.Policy != "all" {
		t.Errorf("join policy = %q, want all", joined.Policy)
	}
	if joined.Cardinality != len(named) {
		t.Errorf("join cardinality = %d, want %d: only the named slots fan out", joined.Cardinality, len(named))
	}
	if len(joined.Arrivals) != len(named) {
		t.Fatalf("joined arrivals = %d, want %d: %s", len(joined.Arrivals), len(named), view.Run.Output)
	}
	var arrived []string
	for _, a := range joined.Arrivals {
		arrived = append(arrived, a.FromNode)
		script, ok := harnessCompareScript[a.FromNode]
		if !ok {
			t.Errorf("arrival from %q, which the run input never named", a.FromNode)
			continue
		}
		if a.Outcome != "completed" {
			t.Errorf("slot %s arrived with outcome %q, want completed", a.FromNode, a.Outcome)
		}
		if !a.Output.WorkspaceMeasured.Measured {
			t.Errorf("slot %s's joined output carries no bridge-measured block: %s", a.FromNode, view.Run.Output)
		}
		if got, want := a.Output.WorkspaceMeasured.ChangedFiles, script.changedFiles; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("slot %s's measured changed_files = %v, want %v (its own harness's, not another slot's)", a.FromNode, got, want)
		}
		if !strings.HasSuffix(a.Output.Summary, a.FromNode) {
			t.Errorf("slot %s's joined summary = %q, want the one that slot's actor wrote", a.FromNode, a.Output.Summary)
		}
	}
	sort.Strings(arrived)
	if strings.Join(arrived, ",") != strings.Join(named, ",") {
		t.Errorf("arrivals came from %v, want exactly the named slots %v", arrived, named)
	}

	// 3. The unset slots never ran, and every named slot ran exactly once.
	ran := map[string]int{}
	usageModel := map[string]string{}
	for _, nr := range view.NodeRuns {
		ran[nr.NodeID]++
		for _, at := range nr.Attempts {
			var out struct {
				Usage *struct {
					UsageModel *string `json:"usage_model"`
				} `json:"usage"`
			}
			// attemptView declares only what harness_test.go asserts on;
			// re-read this attempt's usage from the same wire payload.
			raw := attemptRaw(t, stack, runID, at.ID)
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("decode attempt %s: %v", at.ID, err)
			}
			if out.Usage != nil && out.Usage.UsageModel != nil {
				usageModel[nr.NodeID] = *out.Usage.UsageModel
			}
		}
	}
	for _, slot := range harnessCompareSlots {
		wasNamed := harnessCompareScript[slot].model != ""
		switch {
		case wasNamed && ran[slot] != 1:
			t.Errorf("slot %s ran %d time(s), want 1", slot, ran[slot])
		case !wasNamed && ran[slot] != 0:
			t.Errorf("slot %s ran %d time(s), want 0: it was left unset in the run input", slot, ran[slot])
		}
	}

	// 4. Each slot's attempt carries the model its harness reported — the
	//    `usage.model` half of the per-actor comparison, which lives on the
	//    attempt (`run <id>`), not in the joined output.
	for _, slot := range named {
		if got, want := usageModel[slot], harnessCompareScript[slot].model; got != want {
			t.Errorf("slot %s's attempt reports usage_model %q, want %q", slot, got, want)
		}
	}

	// 5. Each actor's claim stays proposed, attributed to its own actor.
	led := ledgerFor(t, stack.db, ns.ID)
	records, err := led.Records(context.Background(), runID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	claimsBy := map[string]int{}
	for _, rec := range records {
		if rec.RecordType != ledger.RecordClaim {
			continue
		}
		if rec.Authority != ledger.AuthorityProposed {
			t.Errorf("claim %s has authority %q, want proposed: an agent saying done is a claim, not evidence", rec.ID, rec.Authority)
		}
		claimsBy[rec.Origin.ActorID]++
	}
	for _, slot := range named {
		if claimsBy[agentIDs[slot]] != 1 {
			t.Errorf("slot %s's actor %s wrote %d claim(s), want 1", slot, agentIDs[slot], claimsBy[agentIDs[slot]])
		}
	}
}

// attemptRaw fetches one attempt's wire payload from the run view, so the
// test can read fields (usage) that harness_test.go's attemptView does not
// declare — without widening a struct other tests depend on.
func attemptRaw(t *testing.T, s *stack, runID, attemptID string) json.RawMessage {
	t.Helper()
	var view struct {
		NodeRuns []struct {
			Attempts []json.RawMessage `json:"attempts"`
		} `json:"node_runs"`
	}
	if status := s.getJSON("/v1alpha1/runs/"+runID, &view); status != http.StatusOK {
		t.Fatalf("GET run: status %d", status)
	}
	for _, nr := range view.NodeRuns {
		for _, raw := range nr.Attempts {
			var probe struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &probe) == nil && probe.ID == attemptID {
				return raw
			}
		}
	}
	t.Fatalf("attempt %s not in run view", attemptID)
	return nil
}
