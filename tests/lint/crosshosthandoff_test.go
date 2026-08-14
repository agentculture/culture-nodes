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

// Issue #74 / task t13: pr-upkeep handed a FILESYSTEM PATH from `fix` to
// `review`, and those two nodes deliberately sit on different actors -- which
// increasingly means different machines. Observed live in run
// 01KZZSGSWH11J7R7P4V2HPTZZQ: fix committed b01608c on spark, review was
// handed spark's path and thor's codex bridge refused it with HTTP 403
// (auth_or_policy), because that directory is outside thor's allowlist and
// does not exist on thor at all. The bridge was right; the graph asked for
// something incoherent.
//
// A 403 is the worst possible failure shape for this defect: it names
// AUTHORIZATION when the cause is TOPOLOGY. So the graph now says three
// things, and this file is what keeps saying them:
//
//  1. `fix.completed` may only be reported with a PORTABLE HANDLE -- an
//     `artifact://` reference, resolved through the artifact store, which
//     carries no host and no path (internal/artifacts/doc.go). A fix that
//     produced no handle cannot honestly report `completed`: the engine's
//     own outcome-schema validation refuses it (internal/engine/complete.go's
//     checkOutput), so this is enforced, not merely documented.
//  2. A fix host that CANNOT produce that handle has a named way to say so --
//     the `handoff_unavailable` domain outcome, whose `missing_capability` is
//     a CLOSED ENUM. The point of the enum is that the answer names a
//     capability ("this host has no way to publish an artifact") rather than
//     free text a reader has to interpret.
//  3. `handoff_unavailable` never reaches `review`. Routing a handle-less fix
//     into the review node is exactly how the 403 happened; the outcome ends
//     the run at a terminal node instead, carrying the named capability as
//     the run's output.
//
// These are asserted against the committed document rather than against the
// compiled IR because what an author reads and copies is the document. The
// companion guard in examplescompile_test.go already proves it compiles.
const prUpkeepWorkflowPath = "examples/pr-upkeep/workflow.yaml"

// handoffRefPrefix is the ref scheme a portable handle must use. It is the
// only ref shape internal/artifacts issues, and its whole point is that it
// "never carries or implies a filesystem path" (internal/artifacts/doc.go).
const handoffRefPrefix = "^artifact://"

// wfDocument is the slice of the workflow document these tests read. Only
// the fields asserted on are declared: a guard that decoded the whole schema
// would have to be updated by every unrelated authoring change.
type wfDocument struct {
	Spec struct {
		Nodes map[string]wfNode `json:"nodes"`
		Edges []wfEdge          `json:"edges"`
	} `json:"spec"`
}

type wfNode struct {
	Kind     string      `json:"kind"`
	Uses     string      `json:"uses"`
	Input    *wfInput    `json:"input"`
	Contract *wfContract `json:"contract"`
}

type wfInput struct {
	// Bindings values are either a JSON-pointer string or a `{literal: ...}`
	// object (issue #73's typed literal binding), so they decode as `any`
	// and the pointer form is recovered by type assertion.
	Bindings map[string]any `json:"bindings"`
	From     string         `json:"from"`
}

type wfContract struct {
	Input    *wfSchemaSource           `json:"input"`
	Outcomes map[string]wfSchemaSource `json:"outcomes"`
}

type wfSchemaSource struct {
	Schema map[string]any `json:"schema"`
}

type wfEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// loadPRUpkeep parses the pr-upkeep workflow document.
func loadPRUpkeep(t *testing.T) wfDocument {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), prUpkeepWorkflowPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v", prUpkeepWorkflowPath, err)
	}
	var doc wfDocument
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("cannot parse %s: %v", prUpkeepWorkflowPath, err)
	}
	if len(doc.Spec.Nodes) == 0 {
		t.Fatalf("%s declares no nodes -- decoding is broken, and a guard over "+
			"zero nodes passes vacuously", prUpkeepWorkflowPath)
	}
	return doc
}

// node returns a named node, failing with the node ids it did see.
func (d wfDocument) node(t *testing.T, id string) wfNode {
	t.Helper()

	n, ok := d.Spec.Nodes[id]
	if !ok {
		ids := make([]string, 0, len(d.Spec.Nodes))
		for k := range d.Spec.Nodes {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		t.Fatalf("%s declares no node %q\nnodes: %v", prUpkeepWorkflowPath, id, ids)
	}
	return n
}

// pointerBinding returns the JSON pointer bound to name, or "" when the name
// is unbound or bound to a literal rather than a pointer.
func (n wfNode) pointerBinding(name string) string {
	if n.Input == nil {
		return ""
	}
	pointer, _ := n.Input.Bindings[name].(string)
	return pointer
}

// requiredProperties returns a schema's `required` list as strings.
func requiredProperties(schema map[string]any) []string {
	raw, _ := schema["required"].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// property walks `properties/<name>` of a schema object.
func property(schema map[string]any, name string) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	sub, _ := props[name].(map[string]any)
	return sub
}

// TestFixCompletionCarriesAPortableHandle locks guard 1: `fix.completed`
// requires a handle, and the handle is an artifact reference rather than a
// path. The pattern is asserted, not just the property name -- a `handoff`
// whose ref could be any string would re-admit the exact value (a spark
// filesystem path) that produced the 403.
func TestFixCompletionCarriesAPortableHandle(t *testing.T) {
	fix := loadPRUpkeep(t).node(t, "fix")
	if fix.Contract == nil {
		t.Fatalf("node fix declares no contract")
	}

	completed, ok := fix.Contract.Outcomes["completed"]
	if !ok {
		t.Fatalf("node fix declares no `completed` outcome; outcomes: %v",
			sortedOutcomeNames(fix))
	}
	if !containsString(requiredProperties(completed.Schema), "handoff") {
		t.Fatalf("fix.completed does not REQUIRE `handoff` (required: %v).\n"+
			"An optional handle is one a session can simply omit, which leaves "+
			"review with nothing portable to read -- issue #74.",
			requiredProperties(completed.Schema))
	}

	handoff := property(completed.Schema, "handoff")
	if handoff == nil {
		t.Fatalf("fix.completed requires `handoff` but declares no schema for it")
	}
	for _, want := range []string{"kind", "ref"} {
		if !containsString(requiredProperties(handoff), want) {
			t.Errorf("fix.completed's handoff does not require %q (required: %v)",
				want, requiredProperties(handoff))
		}
	}

	ref := property(handoff, "ref")
	pattern, _ := ref["pattern"].(string)
	if !strings.HasPrefix(pattern, handoffRefPrefix) {
		t.Errorf("fix.completed's handoff.ref pattern is %q, want one anchored on %q.\n"+
			"A handle that is not constrained to an artifact reference can be a "+
			"filesystem path again, and a path is not portable across hosts (issue #74).",
			pattern, handoffRefPrefix)
	}
}

// TestFixNamesTheMissingCapability locks guard 2: the honest failure exists,
// is a DOMAIN outcome, and names a capability from a closed set.
func TestFixNamesTheMissingCapability(t *testing.T) {
	fix := loadPRUpkeep(t).node(t, "fix")
	if fix.Contract == nil {
		t.Fatalf("node fix declares no contract")
	}

	unavailable, ok := fix.Contract.Outcomes["handoff_unavailable"]
	if !ok {
		t.Fatalf("node fix declares no `handoff_unavailable` outcome, so a fix host "+
			"that cannot produce a portable handle has no honest way to say so -- "+
			"it fails as a 403 at the review node instead (issue #74).\noutcomes: %v",
			sortedOutcomeNames(fix))
	}
	if !containsString(requiredProperties(unavailable.Schema), "missing_capability") {
		t.Fatalf("fix.handoff_unavailable does not require `missing_capability` "+
			"(required: %v); the outcome has to NAME what is missing, not merely "+
			"report that something is", requiredProperties(unavailable.Schema))
	}

	capability := property(unavailable.Schema, "missing_capability")
	enum, _ := capability["enum"].([]any)
	if len(enum) == 0 {
		t.Errorf("fix.handoff_unavailable's missing_capability declares no enum. " +
			"A free-text capability is a sentence a reader has to interpret; the " +
			"closed set is what makes it a name.")
	}
}

// TestHandoffUnavailableNeverReachesReview locks guard 3: the honest failure
// terminates rather than being routed into the node that cannot act on it.
// This is the assertion that would have caught the live 403 -- review was
// reachable from a fix that had produced nothing review could read.
func TestHandoffUnavailableNeverReachesReview(t *testing.T) {
	doc := loadPRUpkeep(t)

	var targets []string
	for _, edge := range doc.Spec.Edges {
		if edge.From == "fix.handoff_unavailable" {
			targets = append(targets, edge.To)
		}
	}
	if len(targets) == 0 {
		t.Fatalf("no edge routes fix.handoff_unavailable; a declared outcome with no " +
			"edge is a run that stops with nothing said about why")
	}
	for _, target := range targets {
		if target == "review" {
			t.Errorf("fix.handoff_unavailable routes to `review`, which is exactly the " +
				"dispatch that produced HTTP 403 in run 01KZZSGSWH11J7R7P4V2HPTZZQ: " +
				"the review host is asked to read work it was given no portable " +
				"handle for (issue #74)")
			continue
		}
		if kind := doc.node(t, target).Kind; kind != "end" {
			t.Errorf("fix.handoff_unavailable routes to %q (kind %q), want a terminal "+
				"node: a missing host capability is not something another node in "+
				"this graph can resolve", target, kind)
		}
	}
}

// TestReviewReadsTheHandleNotAPath locks the receiving half. Review must bind
// the fix node's handle and require it, and it must NOT take the work under
// review from the same run-input pointer the fix actor was given -- the two
// actors differ, so that pointer is a path that is only meaningful on one of
// their hosts.
func TestReviewReadsTheHandleNotAPath(t *testing.T) {
	doc := loadPRUpkeep(t)
	fix, review := doc.node(t, "fix"), doc.node(t, "review")

	// The premise: these two nodes are deliberately different actors
	// ("the diversity is the value"). Without that, none of this matters.
	if fix.Uses == review.Uses {
		t.Fatalf("fix and review name the same actor (%s); this guard exists because "+
			"they deliberately do not, and different actor increasingly means "+
			"different machine (issue #74)", fix.Uses)
	}

	const wantPointer = "/nodes/fix/output/handoff"
	if got := review.pointerBinding("handoff"); got != wantPointer {
		t.Errorf("review binds handoff = %q, want %q -- review must read the fix "+
			"lane's actual work through the portable handle", got, wantPointer)
	}
	if review.Contract == nil || review.Contract.Input == nil {
		t.Fatalf("node review declares no input contract")
	}
	if !containsString(requiredProperties(review.Contract.Input.Schema), "handoff") {
		t.Errorf("review's input contract does not require `handoff` (required: %v); "+
			"an optional handle lets a review dispatch proceed with nothing to read "+
			"but its own host's checkout, which is not the work under review",
			requiredProperties(review.Contract.Input.Schema))
	}

	if fixRepo := fix.pointerBinding("repo"); fixRepo != "" && fixRepo == review.pointerBinding("repo") {
		t.Errorf("fix and review both bind repo = %q while naming different actors. "+
			"That is the original defect: one host's path handed to another host, "+
			"which fails as HTTP 403 at dispatch (issue #74)", fixRepo)
	}
}

// sortedOutcomeNames renders a node's declared outcomes for failure messages.
func sortedOutcomeNames(n wfNode) []string {
	if n.Contract == nil {
		return nil
	}
	names := make([]string, 0, len(n.Contract.Outcomes))
	for name := range n.Contract.Outcomes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
