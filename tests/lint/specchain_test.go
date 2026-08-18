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

// Task t18 / issue #89: examples/spec-chain/workflow.yaml expresses the
// devague spec chain -- scope -> think -> challenge -- as a committed graph,
// so that the METHOD is a compiled artifact rather than a sequence an operator
// re-performs from memory in one long session.
//
// examplescompile_test.go already proves the file compiles, and
// exampleportability_test.go already proves it names no deployment. Neither is
// the claim. A document that compiles and is portable can still have quietly
// lost the five properties this example exists to hold, and each of them is
// one edit away from vanishing with the compiler none the wiser:
//
//  1. THE KIND IS THE DECISION. A deterministic devague move is a `code` node
//     (a program run through the runner boundary), judgement is an `agent`
//     node, and a gate is an `approval` node. Turning a move into an agent
//     node would replace a measurement with an opinion while leaving every
//     name in place -- which is exactly what examples/merge-gate refuses for
//     the gate, for the same PRD 10.4 reason.
//  2. AUTHORITY HONESTY (spec claim c11 / honesty h11). Every agent node
//     proposes and only proposes. The compiler refuses `ledger.observe` on a
//     non-code node, so that half is covered; what it cannot see is an agent
//     node that declares no ledger delta at all, which reads as "this node
//     writes nothing" while the actor still reports claims.
//  3. THE GATES ARE UNBYPASSABLE. `converge-export` is reachable only from
//     the first approval node and `reconverge` only from the second. That is
//     a property of the EDGES, and it is the entire mechanism by which a
//     proposed claim set cannot converge without a person.
//  4. THE FRAME CARRIER IS REQUIRED (decision c25). Every move node's success
//     port requires a non-empty `artifacts` map: a move that exported no
//     frame cannot report success, and the engine's own outcome validation
//     enforces it. An optional carrier is one a run can simply omit, leaving
//     the next node nothing portable to read (issue #74).
//  5. A CLEAN LENS PASS IS AN ANSWER, NOT AN ABSENCE. Every lens node's
//     `clean_pass` requires the examined surfaces AND the residual
//     uncertainty -- the /challenge skill's own rule that a clean pass never
//     claims there are no unknown unknowns. All four branches reach the
//     barrier, and the barrier is `all`.
//
// Asserted against the committed DOCUMENT rather than the compiled IR, for the
// reason crosshosthandoff_test.go gives: what an author reads and copies is
// the document.
const specChainPath = "examples/spec-chain/workflow.yaml"

// specChainNodeKinds is every node whose KIND is a decision this example
// records, and the kind it was authored as. Nodes not listed here (the ends,
// the verdict decisions) are free to move; these are not.
var specChainNodeKinds = map[string]string{
	// The judgement legs.
	"scope-sweep":           "agent",
	"think-frame":           "agent",
	"interrogate":           "agent",
	"lens-adjacent-systems": "agent",
	"lens-feedback-loops":   "agent",
	"lens-lifecycle":        "agent",
	"lens-security":         "agent",
	// The deterministic devague moves.
	"record-scope":    "code",
	"record-frame":    "code",
	"converge-export": "code",
	"record-findings": "code",
	"reconverge":      "code",
	// The human gates.
	"confirm-proposed-set": "approval",
	"adjudicate-findings":  "approval",
	// The fan-out and its barrier.
	"challenge-fan":  "parallel",
	"challenge-join": "join",
}

// specChainMoveNodes are the deterministic move nodes, whose success port must
// carry the frame.
var specChainMoveNodes = []string{
	"record-scope",
	"record-frame",
	"converge-export",
	"record-findings",
	"reconverge",
}

// specChainLensNodes are the four challenge branches.
var specChainLensNodes = []string{
	"lens-adjacent-systems",
	"lens-feedback-loops",
	"lens-lifecycle",
	"lens-security",
}

// specChainGatedBy maps a node that must be unreachable except through a human
// decision to the single edge source that may reach it.
var specChainGatedBy = map[string]string{
	"converge-export": "confirm-proposed-set.approved",
	"reconverge":      "adjudicate-findings.approved",
}

const specChainJoinNode = "challenge-join"

// specChainDoc is the slice of the workflow document these guards read. It is
// its own type rather than a widening of devLoopDoc or wfDocument for the
// reason those two give each other: a guard that decoded a shared, complete
// schema would need editing by every unrelated authoring change.
type specChainDoc struct {
	Spec struct {
		Nodes map[string]specChainNode `json:"nodes"`
		Edges []specChainEdge          `json:"edges"`
	} `json:"spec"`
}

type specChainNode struct {
	Kind     string             `json:"kind"`
	Uses     string             `json:"uses"`
	Contract *specChainContract `json:"contract"`
	Ledger   *specChainLedger   `json:"ledger"`
	Join     *specChainJoin     `json:"join"`
}

type specChainContract struct {
	Outcomes map[string]struct {
		Schema map[string]any `json:"schema"`
	} `json:"outcomes"`
}

type specChainLedger struct {
	Read    []string `json:"read"`
	Propose []string `json:"propose"`
	Observe []string `json:"observe"`
}

type specChainJoin struct {
	Policy string `json:"policy"`
}

type specChainEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// loadSpecChain parses the spec-chain workflow document.
func loadSpecChain(t *testing.T) specChainDoc {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), specChainPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v", specChainPath, err)
	}
	var doc specChainDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("cannot parse %s: %v", specChainPath, err)
	}
	if len(doc.Spec.Nodes) == 0 || len(doc.Spec.Edges) == 0 {
		t.Fatalf("%s decoded %d node(s) and %d edge(s) -- decoding is broken, and a "+
			"guard over an empty graph passes vacuously",
			specChainPath, len(doc.Spec.Nodes), len(doc.Spec.Edges))
	}
	return doc
}

// node returns a named node, failing with the ids it did see.
func (d specChainDoc) node(t *testing.T, id string) specChainNode {
	t.Helper()

	n, ok := d.Spec.Nodes[id]
	if !ok {
		ids := make([]string, 0, len(d.Spec.Nodes))
		for k := range d.Spec.Nodes {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		t.Fatalf("%s declares no node %q\nnodes: %v", specChainPath, id, ids)
	}
	return n
}

// targetsOf returns every node an edge carries the given "<node>.<outcome>"
// source to.
func (d specChainDoc) targetsOf(from string) []string {
	var targets []string
	for _, edge := range d.Spec.Edges {
		if edge.From == from {
			targets = append(targets, edge.To)
		}
	}
	return targets
}

// sourcesInto returns every edge source that reaches a node.
func (d specChainDoc) sourcesInto(to string) []string {
	var sources []string
	for _, edge := range d.Spec.Edges {
		if edge.To == to {
			sources = append(sources, edge.From)
		}
	}
	return sources
}

// TestSpecChainUsesOnlyTheNineKindsAndTheRightOnes is property 1. The kind is
// the decision, so the kind is what is pinned -- not merely the presence of a
// node with the right name.
func TestSpecChainUsesOnlyTheNineKindsAndTheRightOnes(t *testing.T) {
	doc := loadSpecChain(t)

	for _, id := range sortedSetKeys(specChainNodeKinds) {
		want := specChainNodeKinds[id]
		got := doc.node(t, id).Kind
		if got != want {
			t.Errorf("node %q is kind %q, want %q.\n"+
				"A deterministic devague move is a `code` node because a program run "+
				"through the runner boundary can be MEASURED, while an agent reporting "+
				"that it ran one is a `proposed` completion claim (PRD 10.4) -- the same "+
				"argument examples/merge-gate makes for the gate. Judgement is an agent "+
				"node, and a gate is a person.", id, got, want)
		}
	}

	// No tenth kind snuck in anywhere in the file, named or not (spec
	// non-goal: internal/compiler/vocabulary.go closes the enum at nine and
	// TestNodeKindEnumStaysClosedAtNine holds it there).
	nine := map[string]bool{
		"agent": true, "code": true, "action.http": true, "decision": true,
		"approval": true, "wait": true, "parallel": true, "join": true, "end": true,
	}
	for _, id := range sortedSetKeys(doc.Spec.Nodes) {
		if kind := doc.Spec.Nodes[id].Kind; !nine[kind] {
			t.Errorf("node %q declares kind %q, which is not one of the nine", id, kind)
		}
	}
}

// TestSpecChainAgentsOnlyPropose is property 2, and honesty condition h11's
// half that the compiler cannot state. `ledger.observe` on an agent is already
// an error (internal/compiler/ledger.go); an agent that declares NO ledger
// delta is not, and it reads as a node that writes nothing while its actor
// still reports claims into the run.
func TestSpecChainAgentsOnlyPropose(t *testing.T) {
	doc := loadSpecChain(t)

	agents := 0
	for _, id := range sortedSetKeys(doc.Spec.Nodes) {
		n := doc.Spec.Nodes[id]
		if n.Kind != "agent" {
			continue
		}
		agents++
		if n.Ledger == nil || len(n.Ledger.Propose) == 0 {
			t.Errorf("agent node %q declares no `ledger.propose`. Every agent-origin "+
				"record in this chain lands proposed (spec claim c11 / honesty h11); a "+
				"node that declares nothing is not a node that writes nothing.", id)
			continue
		}
		if len(n.Ledger.Observe) > 0 {
			t.Errorf("agent node %q declares `ledger.observe` %v. Only a trusted runner "+
				"issues observed records, and only for facts it measured directly.",
				id, n.Ledger.Observe)
		}
	}
	if agents < len(specChainLensNodes)+3 {
		t.Fatalf("found %d agent node(s), want at least %d -- decoding is broken, and a "+
			"guard over zero agents passes vacuously", agents, len(specChainLensNodes)+3)
	}
}

// TestSpecChainGatesCannotBeBypassed is property 3. The compiler is satisfied
// by any edge; nothing but this test notices when a second one appears into a
// gated node and quietly makes the human optional.
func TestSpecChainGatesCannotBeBypassed(t *testing.T) {
	doc := loadSpecChain(t)

	for _, gated := range sortedSetKeys(specChainGatedBy) {
		want := specChainGatedBy[gated]
		sources := doc.sourcesInto(gated)
		if len(sources) != 1 || sources[0] != want {
			t.Errorf("node %q is reachable from %v, want exactly [%s].\n"+
				"A second way in is a way past the person. `proposed` becomes something "+
				"else at a human decision and nowhere else (PRD 10.4): no actor promotes "+
				"its own proposal, and a convergence that ran without the gate would have "+
				"exported a spec nobody confirmed.", gated, sources, want)
		}
	}
}

// TestSpecChainEveryMoveCarriesTheFrame is property 4, and decision c25 made
// mechanical: the frame travels as an artifact between nodes, so a move that
// exported none did not carry it.
func TestSpecChainEveryMoveCarriesTheFrame(t *testing.T) {
	doc := loadSpecChain(t)

	for _, id := range specChainMoveNodes {
		n := doc.node(t, id)
		if n.Contract == nil {
			t.Errorf("move node %q declares no contract", id)
			continue
		}
		passed, ok := n.Contract.Outcomes["passed"]
		if !ok {
			t.Errorf("move node %q declares no `passed` outcome; the worker maps exactly "+
				"one success name onto a code node's exit status and refuses vocabulary it "+
				"does not recognise (internal/worker/code.go)", id)
			continue
		}
		if !containsString(requiredProperties(passed.Schema), "artifacts") {
			t.Errorf("%s.passed does not REQUIRE `artifacts` (required: %v).\n"+
				"The frame travels as an artifact between nodes (decision c25). An "+
				"optional carrier is one a run can omit, leaving the next node nothing "+
				"portable to read -- which is issue #74 with extra steps.",
				id, requiredProperties(passed.Schema))
			continue
		}
		artifacts := property(passed.Schema, "artifacts")
		if artifacts == nil {
			t.Errorf("%s.passed requires `artifacts` but declares no schema for it", id)
			continue
		}
		// minProperties, not a ref pattern: the shipped headspace bridge
		// reports exported paths, not artifact:// handles (the workflow's gap
		// 3), so constraining the VALUE would reject every completion the
		// runner can produce. What is enforceable today is that the move
		// exported something.
		if minProps, ok := artifacts["minProperties"]; !ok || toInt(minProps) < 1 {
			t.Errorf("%s.passed's artifacts declares no minProperties >= 1 (got %v). "+
				"An empty map satisfies `type: object`, so without it a move that "+
				"exported nothing still reports success.", id, artifacts["minProperties"])
		}
	}
}

// TestSpecChainCleanPassIsAnAnswer is property 5's first half. The /challenge
// skill's rule is that a clean pass records the examined surfaces and the
// residual uncertainty and NEVER claims there are no unknown unknowns -- so a
// `clean_pass` outcome that requires neither is a lens reporting "nothing
// found" with nothing behind it.
func TestSpecChainCleanPassIsAnAnswer(t *testing.T) {
	doc := loadSpecChain(t)

	for _, id := range specChainLensNodes {
		n := doc.node(t, id)
		if n.Contract == nil {
			t.Errorf("lens node %q declares no contract", id)
			continue
		}
		clean, ok := n.Contract.Outcomes["clean_pass"]
		if !ok {
			t.Errorf("lens node %q declares no `clean_pass` outcome, so a lens that found "+
				"nothing has to report findings it does not have", id)
			continue
		}
		required := requiredProperties(clean.Schema)
		for _, want := range []string{"examined", "residual_uncertainty"} {
			if !containsString(required, want) {
				t.Errorf("%s.clean_pass does not require %q (required: %v).\n"+
					"A clean pass is an ANSWER -- these are the surfaces I read, this is "+
					"what I still do not know -- and never a claim that there is nothing "+
					"left to find.", id, want, required)
			}
		}
	}
}

// TestSpecChainEveryLensReconvenes is property 5's second half: all four
// branches reach the barrier, both of each lens's ports do, and the barrier is
// `all`. The compiler refuses a join nothing reaches and an end inside a split;
// it does not notice a lens whose second port was quietly routed past the
// barrier, or a policy silently relaxed to `any`.
func TestSpecChainEveryLensReconvenes(t *testing.T) {
	doc := loadSpecChain(t)

	join := doc.node(t, specChainJoinNode)
	if join.Join == nil || join.Join.Policy != "all" {
		policy := "<none>"
		if join.Join != nil {
			policy = join.Join.Policy
		}
		t.Errorf("%s declares join policy %q, want \"all\".\n"+
			"A challenge pass with three of four lenses reported is a partial sweep, and "+
			"the adjudicating human would be deciding on an unstated subset.",
			specChainJoinNode, policy)
	}

	fanned := doc.targetsOf("challenge-fan.split")
	for _, lens := range specChainLensNodes {
		if !containsString(fanned, lens) {
			t.Errorf("no `challenge-fan.split` edge reaches %q (fanned: %v); a lens that "+
				"is not fanned out is a lens that runs in somebody else's context, which "+
				"is the shape this leg exists to leave behind", lens, fanned)
		}
		for _, outcome := range []string{"findings", "clean_pass"} {
			source := lens + "." + outcome
			targets := doc.targetsOf(source)
			if len(targets) == 0 {
				t.Errorf("no edge carries %s anywhere", source)
				continue
			}
			for _, target := range targets {
				if target != specChainJoinNode {
					t.Errorf("%s routes to %q, want %q. A branch that leaves the split "+
						"without passing the barrier strands its siblings.",
						source, target, specChainJoinNode)
				}
			}
		}
	}
}

// toInt reads a JSON-decoded number, which sigs.k8s.io/yaml yields as float64.
func toInt(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

// TestSpecChainHeaderNamesItsGaps keeps the honesty half from rotting. This
// example DECLARES more than the runtime can do -- no route lands a devague
// frame in the ledger, the artifact carrier has no read side, and nothing
// carries a binding into a code operation -- and the file is only honest while
// it says so. A "WHAT DOES NOT WORK YET" block that disappears in an edit is
// how a graph that looks complete comes to be believed.
func TestSpecChainHeaderNamesItsGaps(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), specChainPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v", specChainPath, err)
	}
	prose := yamlCommentProse(string(raw))

	for _, want := range []string{
		"WHAT DOES NOT WORK YET",
		"Deployment configuration",
	} {
		if !strings.Contains(prose, want) {
			t.Errorf("%s's header no longer carries a %q block. The graph declares an "+
				"interface the runtime does not yet honour; an explicit gap list is the "+
				"only thing standing between that and a false claim.", specChainPath, want)
		}
	}
}
