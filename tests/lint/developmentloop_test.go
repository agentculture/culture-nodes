// Package testslint holds lint-as-Go-test checks that are cheaper to run as
// part of `go test ./...` than to stand up as a separate tool -- the same
// rationale internal/actors/neutrality_test.go documents for the provider-
// neutrality guard: a fast tripwire enforced by `go test`, not a
// sophisticated static-analysis pass.
package testslint

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Task t21: examples/development-loop/workflow.yaml expresses this project's
// own development loop as a graph, so that the ORDER of the loop is a compiled
// artifact rather than prose a reader has to obey by hand (spec claim c21).
//
// examplescompile_test.go already proves that file compiles. Compiling is not
// the claim, though: a document that compiles can still have quietly lost the
// four structural properties the loop exists for, and each of them is one edit
// away from vanishing without the compiler noticing --
//
//  1. THE THREE NEW NODES exist, with the kinds their design decided
//     (provision-workspace, handover-gate, cleanup-workspace).
//  2. BOTH HANDOFF CARRIERS are required of the writer, with CONSTRAINED refs:
//     a `git_ref` for the changes, an `artifact://` reference for the context.
//     An unconstrained ref is a filesystem path again, which is issue #74.
//  3. THE GATE-FAILURE LOOP IS BOUNDED: a failed threshold routes back to the
//     node that declares `continue.while`, that declaration carries all three
//     bounds, and its `onExhausted` reaches a HUMAN node. The compiler checks
//     only that the exhaustion outcome has *an* edge -- an edge to another
//     agent node would satisfy it and would be an unbounded billable loop
//     wearing a bound's clothing.
//  4. THE FENCED TIMEOUT IS ROUTED: the workspace fence refuses a retry after
//     a deadline cancel, and a refusal "is not itself an ending"
//     (internal/engine/retry.go) -- so if no edge leaves `work.timed_out` the
//     run dies at exactly the moment the fence was built to improve.
//
// Asserted against the committed DOCUMENT rather than the compiled IR, for the
// reason crosshosthandoff_test.go gives: what an author reads and copies is
// the document.
const developmentLoopPath = "examples/development-loop/workflow.yaml"

// The three node ids task t21 names, and the kind each was authored as. The
// kinds are asserted, not just the ids: `handover-gate` is a DETERMINISTIC
// validator (the t18 decision), and an edit turning it into an agent node
// would replace a measurement with an opinion while leaving every name intact.
var developmentLoopNodeKinds = map[string]string{
	"provision-workspace": "agent",
	"handover-gate":       "code",
	"cleanup-workspace":   "agent",
}

// The writer node, and the node whose verdict routes back into it. Named as
// constants because four separate assertions below all turn on the same pair.
const (
	writerNode  = "work"
	verdictNode = "gate-verdict"
	// gateFailureOutcome is the verdict port that means "a declared numeric
	// threshold was missed" -- the one that loops, as opposed to
	// `measurement_incomplete`, which must NOT loop.
	gateFailureOutcome = "changes_required"
)

// humanNodeKinds are the kinds that put a PERSON in the path. `approval` is
// the engine's own human-task surface; `agent` counts only when the actor it
// names is a human inbox bridge, which no assertion here can tell from the
// document alone -- so this guard accepts `approval` and requires the looser
// case to be argued in a future edit rather than assumed now.
var humanNodeKinds = map[string]bool{"approval": true}

// devLoopDoc is the slice of the workflow document these guards read. It is a
// separate type from crosshosthandoff_test.go's wfDocument because it needs
// the `continue` block, which that guard has no use for; extending the shared
// type would make an unrelated guard's decoding surface grow for this one.
type devLoopDoc struct {
	Spec struct {
		Nodes map[string]devLoopNode `json:"nodes"`
		Edges []devLoopEdge          `json:"edges"`
	} `json:"spec"`
}

type devLoopNode struct {
	Kind     string          `json:"kind"`
	Uses     string          `json:"uses"`
	Contract *devLoopContra  `json:"contract"`
	Continue *devLoopContinu `json:"continue"`
}

type devLoopContra struct {
	Outcomes map[string]struct {
		Schema map[string]any `json:"schema"`
	} `json:"outcomes"`
}

type devLoopContinu struct {
	While  []string `json:"while"`
	Bounds struct {
		MaxContinuations *int   `json:"maxContinuations"`
		MaxWallClock     string `json:"maxWallClock"`
		MaxSessions      *int   `json:"maxSessions"`
	} `json:"bounds"`
	OnExhausted string `json:"onExhausted"`
}

type devLoopEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// loadDevelopmentLoop parses the development-loop workflow document.
func loadDevelopmentLoop(t *testing.T) devLoopDoc {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), developmentLoopPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v", developmentLoopPath, err)
	}
	var doc devLoopDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("cannot parse %s: %v", developmentLoopPath, err)
	}
	if len(doc.Spec.Nodes) == 0 || len(doc.Spec.Edges) == 0 {
		t.Fatalf("%s decoded %d node(s) and %d edge(s) -- decoding is broken, and a "+
			"guard over an empty graph passes vacuously",
			developmentLoopPath, len(doc.Spec.Nodes), len(doc.Spec.Edges))
	}
	return doc
}

// node returns a named node, failing with the ids it did see.
func (d devLoopDoc) node(t *testing.T, id string) devLoopNode {
	t.Helper()

	n, ok := d.Spec.Nodes[id]
	if !ok {
		ids := make([]string, 0, len(d.Spec.Nodes))
		for k := range d.Spec.Nodes {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		t.Fatalf("%s declares no node %q\nnodes: %v", developmentLoopPath, id, ids)
	}
	return n
}

// targetsOf returns every node an edge carries the given "<node>.<outcome>"
// source to.
func (d devLoopDoc) targetsOf(from string) []string {
	var targets []string
	for _, edge := range d.Spec.Edges {
		if edge.From == from {
			targets = append(targets, edge.To)
		}
	}
	return targets
}

// TestDevelopmentLoopDeclaresTheThreeNewNodes is acceptance criterion 2 of
// task t21: the new nodes are declared, and they are declared as the kinds
// their design chose.
func TestDevelopmentLoopDeclaresTheThreeNewNodes(t *testing.T) {
	doc := loadDevelopmentLoop(t)

	for _, id := range sortedSetKeys(developmentLoopNodeKinds) {
		wantKind := developmentLoopNodeKinds[id]
		got := doc.node(t, id)
		if got.Kind != wantKind {
			t.Errorf("node %q is kind %q, want %q.\n"+
				"The kind is the decision: `handover-gate` is a deterministic validator "+
				"(the t18 design), and provisioning/reaping a worktree can only be done "+
				"by the host that owns the checkout, which is what an agent node reaches.",
				id, got.Kind, wantKind)
		}
	}
}

// TestDevelopmentLoopRequiresBothHandoffCarriers is acceptance criterion 4's
// first half: the writer must hand over BOTH carriers, and each must be
// constrained to a portable shape.
//
// The routing rule being pinned here -- a runner's changes take `git_ref`,
// context and data take an artifact -- is the whole reason there are two: they
// are produced by different machinery and fail independently, so a single
// polymorphic handle would let one substitute for the other silently.
func TestDevelopmentLoopRequiresBothHandoffCarriers(t *testing.T) {
	work := loadDevelopmentLoop(t).node(t, writerNode)
	if work.Contract == nil {
		t.Fatalf("node %q declares no contract", writerNode)
	}
	completed, ok := work.Contract.Outcomes["completed"]
	if !ok {
		t.Fatalf("node %q declares no `completed` outcome", writerNode)
	}
	if !containsString(requiredProperties(completed.Schema), "handoff") {
		t.Fatalf("%s.completed does not REQUIRE `handoff` (required: %v); an optional "+
			"handle is one a session can simply omit, leaving the review lane nothing "+
			"portable to read (issue #74)", writerNode, requiredProperties(completed.Schema))
	}
	handoff := property(completed.Schema, "handoff")
	if handoff == nil {
		t.Fatalf("%s.completed requires `handoff` but declares no schema for it", writerNode)
	}

	for _, carrier := range []struct {
		member      string
		wantKind    string
		wantPrefix  string
		description string
	}{
		{"changes", "git_ref", "^refs/culture-nodes/",
			"a runner's CHANGES travel as a git ref, under the namespace AGENTS.md's " +
				"policy fixed (a handover ref, never a branch, never pushed)"},
		{"context", "artifact", "^artifact://",
			"CONTEXT and data travel as an artifact reference, which by construction " +
				"never carries or implies a filesystem path (internal/artifacts/doc.go)"},
	} {
		if !containsString(requiredProperties(handoff), carrier.member) {
			t.Errorf("%s.completed's handoff does not require %q (required: %v).\n%s",
				writerNode, carrier.member, requiredProperties(handoff), carrier.description)
			continue
		}
		schema := property(handoff, carrier.member)
		if schema == nil {
			t.Errorf("%s.completed's handoff requires %q but declares no schema for it",
				writerNode, carrier.member)
			continue
		}
		if kind, _ := property(schema, "kind")["const"].(string); kind != carrier.wantKind {
			t.Errorf("handoff.%s declares kind const %q, want %q -- the carrier has to NAME "+
				"which machinery produced it, or the two become interchangeable",
				carrier.member, kind, carrier.wantKind)
		}
		pattern, _ := property(schema, "ref")["pattern"].(string)
		if !strings.HasPrefix(pattern, carrier.wantPrefix) {
			t.Errorf("handoff.%s's ref pattern is %q, want one anchored on %q.\n"+
				"A handle that is not constrained can be a filesystem path again, and a "+
				"path is meaningful on exactly one machine (issue #74).",
				carrier.member, pattern, carrier.wantPrefix)
		}
	}
}

// TestDevelopmentLoopBoundsTheGateFailureLoop is acceptance criterion 4's
// second half, and the one the compiler cannot make: the gate-failure edge
// reaches the node that declares the continuation, that declaration carries
// all three bounds, and exhaustion reaches a person.
func TestDevelopmentLoopBoundsTheGateFailureLoop(t *testing.T) {
	doc := loadDevelopmentLoop(t)

	targets := doc.targetsOf(verdictNode + "." + gateFailureOutcome)
	if !containsString(targets, writerNode) {
		t.Fatalf("no edge carries %s.%s to %q (targets: %v).\n"+
			"A failed gate has to route to the node that declares the continuation; "+
			"routing it anywhere else is the unbounded fix/re-measure loop the bounds "+
			"exist to stop.", verdictNode, gateFailureOutcome, writerNode, targets)
	}

	work := doc.node(t, writerNode)
	if work.Continue == nil {
		t.Fatalf("node %q declares no `continue` block, so the gate-failure edge above "+
			"loops on the engine's good nature rather than on a declared bound", writerNode)
	}
	if len(work.Continue.While) == 0 {
		t.Errorf("%s.continue declares no `while` condition; the engine evaluates this in "+
			"CEL precisely so that no model decides whether to keep going", writerNode)
	}
	// All three bounds, not any one of them. The schema requires only that the
	// block declare at least one, and the three stop different things: turns,
	// wall clock, and cold provider sessions. maxWallClock carries extra
	// weight -- the deadline handler refuses to PAUSE a continuation with no
	// time-based bound (internal/scheduler/scheduler.go), so leaving it off
	// silently converts every pause into a cancel.
	if work.Continue.Bounds.MaxContinuations == nil {
		t.Errorf("%s.continue.bounds declares no maxContinuations", writerNode)
	}
	if work.Continue.Bounds.MaxSessions == nil {
		t.Errorf("%s.continue.bounds declares no maxSessions -- the bound that costs money", writerNode)
	}
	if work.Continue.Bounds.MaxWallClock == "" {
		t.Errorf("%s.continue.bounds declares no maxWallClock. A fired deadline refuses to "+
			"pause a continuation with no time-based bound and cancels instead, so its "+
			"absence quietly removes the pause behaviour rather than removing a limit",
			writerNode)
	}

	if work.Continue.OnExhausted == "" {
		t.Fatalf("%s.continue declares no onExhausted outcome", writerNode)
	}
	exhaustedTargets := doc.targetsOf(writerNode + "." + work.Continue.OnExhausted)
	if len(exhaustedTargets) == 0 {
		// The compiler reports this too (graph.continuation_exhausted_unrouted);
		// saying it here as well is cheap and keeps this test's story complete.
		t.Fatalf("no edge carries %s.%s away", writerNode, work.Continue.OnExhausted)
	}
	for _, target := range exhaustedTargets {
		if kind := doc.node(t, target).Kind; !humanNodeKinds[kind] {
			t.Errorf("%s.%s routes to %q (kind %q), want a human node.\n"+
				"The compiler is satisfied by ANY edge, so routing exhaustion at another "+
				"agent node would compile -- and would be the same unbounded billable loop "+
				"with one extra hop. A spent budget is not something this graph is "+
				"entitled to resolve by itself.",
				writerNode, work.Continue.OnExhausted, target, kind)
		}
	}
}

// TestDevelopmentLoopRoutesTheFencedTimeout locks the workspace fence's
// aftermath. `timed_out` is an engine status, so nothing in the document
// declares it and nothing but this test notices when its edge disappears --
// at which point a deadline cancel followed by a refused retry ends the run
// with no domain answer at all.
func TestDevelopmentLoopRoutesTheFencedTimeout(t *testing.T) {
	doc := loadDevelopmentLoop(t)

	targets := doc.targetsOf(writerNode + ".timed_out")
	if len(targets) == 0 {
		t.Fatalf("no edge routes %s.timed_out.\n"+
			"A deadline that cancels a live session commits `timed_out` with origin "+
			"`deadline`, and the workspace fence then REFUSES the remaining retry "+
			"(internal/engine/retry.go). That refusal is not an ending -- but with no "+
			"edge from this status the run fails anyway, which is the outcome the fence "+
			"was built to improve on.", writerNode)
	}
	for _, target := range targets {
		if kind := doc.node(t, target).Kind; !humanNodeKinds[kind] {
			t.Errorf("%s.timed_out routes to %q (kind %q), want a human node: whether a "+
				"cancelled session's work is recoverable is not a question this graph "+
				"can answer for itself", writerNode, target, kind)
		}
	}
}

// TestDevelopmentLoopKeepsAnIncompleteMeasurementOffTheHappyPath is the t18
// honesty rule made mechanical: an instrument that did not reach the tree must
// never be reinterpreted as a pass. `measurement_incomplete` is a separate
// verdict port from `changes_required` for exactly that reason, and it must
// reach a person rather than the review lane or the writer.
func TestDevelopmentLoopKeepsAnIncompleteMeasurementOffTheHappyPath(t *testing.T) {
	doc := loadDevelopmentLoop(t)

	targets := doc.targetsOf(verdictNode + ".measurement_incomplete")
	if len(targets) == 0 {
		t.Fatalf("%s declares no routed `measurement_incomplete` port.\n"+
			"Coverage and cognitive complexity do not reach `internal`, `adapters` or "+
			"`web` today, so a change there is measured by two of four gates; collapsing "+
			"that into the threshold-miss port is how an unmeasured tree comes to be "+
			"reported as green (the t18 design's applicability matrix).", verdictNode)
	}
	for _, target := range targets {
		if kind := doc.node(t, target).Kind; !humanNodeKinds[kind] {
			t.Errorf("%s.measurement_incomplete routes to %q (kind %q), want a human node: "+
				"accepting a partial measurement is a person's decision, and routing it "+
				"onward would make it the graph's", verdictNode, target, kind)
		}
	}
}

// sortedSetKeys returns a string-keyed map's keys in a stable order, so the
// assertions above report in the same order every run.
func sortedSetKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
