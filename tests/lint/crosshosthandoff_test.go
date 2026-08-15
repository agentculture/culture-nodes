// Package testslint holds lint-as-Go-test checks that are cheaper to run as
// part of `go test ./...` than to stand up as a separate tool -- the same
// rationale internal/actors/neutrality_test.go documents for the provider-
// neutrality guard: a fast tripwire enforced by `go test`, not a
// sophisticated static-analysis pass.
package testslint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
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
//  1. `fix.completed` may only be reported with a PORTABLE HANDLE, and
//     `review` may only be dispatched with one. What makes a handle portable
//     is declared in exactly one place -- schemas/workflow/handoff.schema.json
//     -- and a fix that produced no handle cannot honestly report `completed`:
//     the engine's own outcome-schema validation refuses it
//     (internal/engine/complete.go's checkOutput), so this is enforced, not
//     merely documented.
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
// WIDENED BY DECISION, NOT BY DRIFT (spec decision q9, task t6). This guard
// used to require one specific handle shape: `handoff.ref` matching
// `^artifact://`. Boundary c3 declared that contract settled and off-limits,
// and its own honesty condition h17 pinned that boundary to "the guard passes
// UNCHANGED after the artifact path lands; if implementing the mechanism
// requires editing the guard, the contract was not settled and this boundary
// is false." q9 requires exactly that edit -- #74's own recommendation was
// "option 1 [a git ref], with 2 [an artifact] for anything not naturally a
// git object", and the single-carrier contract had collapsed it -- so the
// boundary was retired and this file was widened deliberately. h17 firing is
// the method working; a future reader should treat this paragraph as the
// record that the widening was decided, and should not read it as licence to
// widen further without one.
//
// WHAT DID NOT MOVE, and is the load-bearing half: a BARE FILESYSTEM PATH is
// still refused between nodes whose actors may differ. That refusal is what
// #74 was actually about, and honesty condition h48 requires it be proven by
// a case that hands one over and expects rejection --
// TestABareFilesystemPathIsStillRefused below is that case, and it hands one
// to every surface that accepts a handle, under every carrier kind. Also
// unmoved: the closed `missing_capability` enum, and the rule that
// `handoff_unavailable` never routes into review.
//
// These are asserted against the committed document rather than against the
// compiled IR because what an author reads and copies is the document. The
// companion guard in examplescompile_test.go already proves it compiles.
const prUpkeepWorkflowPath = "examples/pr-upkeep/workflow.yaml"

// canonicalHandoffSchemaPath is the ONE declaration of what a portable handle
// is: which carriers exist, what each one's ref may look like, and what a
// git-ref handle must pin. A node contract cannot `$ref` it (the engine
// validates a node's contract as a self-contained document), so the workflow
// embeds a copy -- and TestHandoffRuleIsDeclaredOnce compares them keyword by
// keyword, which is what keeps "declared once" true of a copied rule.
const canonicalHandoffSchemaPath = "schemas/workflow/handoff.schema.json"

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

// enumValues returns a schema's `enum` as strings.
func enumValues(schema map[string]any) []string {
	raw, _ := schema["enum"].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// handoffSite is one place in the graph where a handle is constrained: the
// producing outcome and the consuming input. Both are checked, because they
// are two documents -- a review dispatch that trusted the fix node's contract
// would be trusting a schema no validator applies to it.
type handoffSite struct {
	name   string
	schema map[string]any
}

// handoffSites returns every constrained handle surface: the canonical
// declaration plus each embedded copy. A vector table run over this slice
// asserts the same behaviour everywhere rather than once.
func handoffSites(t *testing.T) []handoffSite {
	t.Helper()

	doc := loadPRUpkeep(t)
	fix, review := doc.node(t, "fix"), doc.node(t, "review")

	sites := []handoffSite{{name: canonicalHandoffSchemaPath, schema: canonicalHandoffSchema(t)}}

	if fix.Contract == nil {
		t.Fatalf("node fix declares no contract")
	}
	completed, ok := fix.Contract.Outcomes["completed"]
	if !ok {
		t.Fatalf("node fix declares no `completed` outcome; outcomes: %v", sortedOutcomeNames(fix))
	}
	produced := property(completed.Schema, "handoff")
	if produced == nil {
		t.Fatalf("fix.completed declares no schema for `handoff`; there is nothing "+
			"constraining what the fix lane hands over (%s)", prUpkeepWorkflowPath)
	}
	sites = append(sites, handoffSite{name: "fix.completed.handoff", schema: produced})

	if review.Contract == nil || review.Contract.Input == nil {
		t.Fatalf("node review declares no input contract")
	}
	consumed := property(review.Contract.Input.Schema, "handoff")
	if consumed == nil {
		t.Fatalf("review's input contract declares no schema for `handoff`, so the " +
			"RECEIVING half of the boundary constrains nothing: a path smuggled into " +
			"that binding by any route reaches the review host unchecked (issue #74)")
	}
	sites = append(sites, handoffSite{name: "review.input.handoff", schema: consumed})

	return sites
}

// canonicalHandoffSchema reads the single declaration of the handle rule.
func canonicalHandoffSchema(t *testing.T) map[string]any {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), canonicalHandoffSchemaPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v", canonicalHandoffSchemaPath, err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("cannot parse %s: %v", canonicalHandoffSchemaPath, err)
	}
	return schema
}

// compileSchema turns one decoded schema object into a validator. The schema
// is round-tripped through JSON deliberately: the workflow copy arrives from
// YAML and the canonical copy from JSON, and what must behave identically is
// the compiled rule, not the decoder that produced the map.
func compileSchema(t *testing.T, name string, schema map[string]any) *jsonschema.Schema {
	t.Helper()

	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("%s: cannot re-encode schema: %v", name, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: cannot decode schema for compilation: %v", name, err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	const uri = "https://nodes.culture.dev/tests/handoff.schema.json"
	if err := compiler.AddResource(uri, doc); err != nil {
		t.Fatalf("%s: cannot register schema: %v", name, err)
	}
	compiled, err := compiler.Compile(uri)
	if err != nil {
		t.Fatalf("%s: schema does not compile as Draft 2020-12: %v", name, err)
	}
	return compiled
}

// validateHandle reports whether one candidate handle satisfies a compiled
// handoff schema, and why not when it does not.
func validateHandle(t *testing.T, compiled *jsonschema.Schema, handle string) error {
	t.Helper()

	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(handle))
	if err != nil {
		t.Fatalf("test vector is not valid JSON (%s): %v", handle, err)
	}
	return compiled.Validate(instance)
}

// stripAnnotations removes the keywords that describe a schema without
// constraining anything, so two copies of one rule can be compared on what
// they actually enforce. Prose is allowed to differ per surface -- the
// workflow copy explains itself to a workflow author, the canonical file to a
// schema reader -- but not one keyword of behaviour may.
func stripAnnotations(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, sub := range typed {
			switch key {
			case "$schema", "$id", "title", "description", "$comment", "examples", "deprecated", "readOnly", "writeOnly":
				continue
			}
			out[key] = stripAnnotations(sub)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, stripAnnotations(item))
		}
		return out
	// YAML and JSON decoding disagree about numeric types for identical
	// documents (`40` may arrive as float64 or as an int), so numbers are
	// compared by their canonical rendering rather than by Go type.
	case float64:
		return fmt.Sprintf("%v", typed)
	case int:
		return fmt.Sprintf("%v", typed)
	case int64:
		return fmt.Sprintf("%v", typed)
	default:
		return value
	}
}

// canonicalRendering renders a stripped schema deterministically (Go's JSON
// encoder sorts map keys), so a mismatch failure shows a reader exactly which
// keyword drifted.
func canonicalRendering(t *testing.T, value any) string {
	t.Helper()

	raw, err := json.MarshalIndent(stripAnnotations(value), "", "  ")
	if err != nil {
		t.Fatalf("cannot render schema for comparison: %v", err)
	}
	return string(raw)
}

// TestHandoffRuleIsDeclaredOnce locks the q9 acceptance criterion that the
// routing rule is declared ONCE. The engine validates a node contract as a
// self-contained document, so the workflow cannot $ref the canonical schema
// and must embed it -- which is precisely how a second, weaker rule gets
// created by accident. Comparing the embedded copies against the canonical
// declaration keyword-by-keyword is what makes the copy a copy.
func TestHandoffRuleIsDeclaredOnce(t *testing.T) {
	want := canonicalRendering(t, canonicalHandoffSchema(t))
	for _, site := range handoffSites(t) {
		if site.name == canonicalHandoffSchemaPath {
			continue
		}
		if got := canonicalRendering(t, site.schema); got != want {
			t.Errorf("%s does not embed %s's rule.\n--- embedded copy ---\n%s\n"+
				"--- canonical declaration ---\n%s\n"+
				"Edit the canonical schema and re-copy it; do not maintain two rules.",
				site.name, canonicalHandoffSchemaPath, got, want)
		}
	}
}

// TestBothCarriersAreAccepted locks the widening itself (q9): changes travel
// as a git ref, context and data travel as an artifact, and BOTH are portable
// handles the graph accepts. A guard that only proved refusals would pass on a
// contract that accepts nothing at all.
func TestBothCarriersAreAccepted(t *testing.T) {
	accepted := []struct{ name, handle string }{
		{
			"an artifact handle -- context and data, which is not naturally a git object",
			`{"kind":"artifact","ref":"artifact://pr-upkeep/01M02SWEEPITEMS","media_type":"application/json"}`,
		},
		{
			"a git-ref handle -- a runner's changes, which are naturally a git object",
			`{"kind":"git_ref","ref":"git+https://github.com/agentculture/culture-nodes.git#refs/culture-nodes/01M02RUN/01M02NODERUN","commit":"df7d9740000000000000000000000000000000aa","publication":"pending"}`,
		},
		{
			"a git-ref handle over ssh, already published by the operator",
			`{"kind":"git_ref","ref":"git+ssh://git@github.com/agentculture/culture-nodes.git#refs/culture-nodes/01M02RUN/01M02NODERUN","commit":"df7d9740000000000000000000000000000000aa","publication":"published"}`,
		},
	}

	for _, site := range handoffSites(t) {
		compiled := compileSchema(t, site.name, site.schema)
		for _, tc := range accepted {
			if err := validateHandle(t, compiled, tc.handle); err != nil {
				t.Errorf("%s REFUSES %s.\nhandle: %s\nwhy: %v\n"+
					"Both carriers are decided (spec decision q9): a runner's changes take "+
					"git_ref, context and data take artifact. A surface that accepts only "+
					"one of them cannot carry the pr-upkeep case, which needs both.",
					site.name, tc.name, tc.handle, err)
			}
		}
	}
}

// TestABareFilesystemPathIsStillRefused is the load-bearing guard (honesty
// condition h48) and the reason this file exists at all. Widening the accepted
// handle kinds must not weaken the one refusal #74 was about, so this hands a
// bare filesystem path over -- under each carrier kind, and at every surface
// that accepts a handle -- and expects rejection. It also refuses the three
// near-misses that would re-admit a path or a branch through the new kind's
// door.
func TestABareFilesystemPathIsStillRefused(t *testing.T) {
	refused := []struct{ name, handle, because string }{
		{
			"a bare filesystem path, declared as an artifact",
			`{"kind":"artifact","ref":"/home/spark/git/culture-nodes"}`,
			"this is the literal value that produced HTTP 403 in run 01KZZSGSWH11J7R7P4V2HPTZZQ",
		},
		{
			"a bare filesystem path, declared as a git ref",
			`{"kind":"git_ref","ref":"/home/spark/git/culture-nodes","commit":"df7d9740000000000000000000000000000000aa","publication":"pending"}`,
			"the widening added a kind, not a door: a path is unportable under either label",
		},
		{
			"a bare filesystem path with no kind at all",
			`{"ref":"/home/spark/git/culture-nodes"}`,
			"`kind` is required, so an unlabelled handle cannot slip past the per-kind rules",
		},
		{
			"a path wearing a file:// URL",
			`{"kind":"git_ref","ref":"git+file:///home/spark/git/culture-nodes#refs/culture-nodes/01M02RUN/01M02NODERUN","commit":"df7d9740000000000000000000000000000000aa","publication":"pending"}`,
			"a file transport is a filesystem path in a costume; admitting it makes the whole pattern decorative",
		},
		{
			"a bare branch name with no remote",
			`{"kind":"git_ref","ref":"owe/batch","commit":"df7d9740000000000000000000000000000000aa","publication":"pending"}`,
			"#74 required a branch name PLUS a remote; the branch name alone is as host-local as a path",
		},
		{
			"a ref under refs/heads",
			`{"kind":"git_ref","ref":"git+https://github.com/agentculture/culture-nodes.git#refs/heads/owe/batch","commit":"df7d9740000000000000000000000000000000aa","publication":"pending"}`,
			"AGENTS.md forbids an agent to commit onto a branch, so a handover may only name refs/culture-nodes/*",
		},
		{
			"a git ref pinning no commit",
			`{"kind":"git_ref","ref":"git+https://github.com/agentculture/culture-nodes.git#refs/culture-nodes/01M02RUN/01M02NODERUN","publication":"pending"}`,
			"without the pinned sha the consuming host cannot verify it fetched the object the producer named",
		},
		{
			"a git ref that will not say whether it was published",
			`{"kind":"git_ref","ref":"git+https://github.com/agentculture/culture-nodes.git#refs/culture-nodes/01M02RUN/01M02NODERUN","commit":"df7d9740000000000000000000000000000000aa"}`,
			"a handle that is silent about publication asserts fetchability the producing side is not allowed to deliver",
		},
		{
			"an invented third carrier",
			`{"kind":"worktree","ref":"/home/spark/git/.worktrees.culture-nodes/owe-developer"}`,
			"the kind enum is closed so a new carrier is a decision, not a string somebody typed",
		},
		{
			"an artifact handle smuggling a path alongside it",
			`{"kind":"artifact","ref":"artifact://pr-upkeep/01M02SWEEPITEMS","path":"/home/spark/git/culture-nodes"}`,
			"additionalProperties is false so the handle cannot carry a location beside the handle",
		},
	}

	for _, site := range handoffSites(t) {
		compiled := compileSchema(t, site.name, site.schema)
		for _, tc := range refused {
			if err := validateHandle(t, compiled, tc.handle); err == nil {
				t.Errorf("%s ACCEPTS %s.\nhandle: %s\nwhy this must be refused: %s\n"+
					"This is the refusal issue #74 was actually about and honesty condition "+
					"h48 pins: widening the accepted handle kinds must not weaken it.",
					site.name, tc.name, tc.handle, tc.because)
			}
		}
	}
}

// TestFixCompletionCarriesAPortableHandle locks guard 1: `fix.completed`
// requires a handle, the handle names its carrier and its ref, and both
// decided carriers are declared. What each carrier's ref may contain is
// asserted by behaviour above rather than by reading the pattern here -- a
// pattern read is satisfied by a pattern that matches everything.
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

	kinds := enumValues(property(handoff, "kind"))
	for _, want := range []string{"artifact", "git_ref"} {
		if !containsString(kinds, want) {
			t.Errorf("fix.completed's handoff.kind does not offer %q (enum: %v).\n"+
				"Spec decision q9 settled TWO carriers with a declared rule: a runner's "+
				"changes take git_ref, context and data take artifact. One carrier "+
				"collapses that rule, which is the state #74 recommended against.",
				want, kinds)
		}
	}
}

// TestFixNamesTheMissingCapability locks guard 2: the honest failure exists,
// is a DOMAIN outcome, and names a capability from a closed set. Two carriers
// means two publish capabilities, and a host can be missing either alone.
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
	capabilities := enumValues(capability)
	if len(capabilities) == 0 {
		t.Fatalf("fix.handoff_unavailable's missing_capability declares no enum. " +
			"A free-text capability is a sentence a reader has to interpret; the " +
			"closed set is what makes it a name.")
	}
	for _, want := range []string{"artifact_publish", "git_ref_publish"} {
		if !containsString(capabilities, want) {
			t.Errorf("fix.handoff_unavailable's missing_capability cannot say %q "+
				"(enum: %v). Each carrier has its own publish capability and a host "+
				"can be missing either one alone; a single name would make the two "+
				"failures indistinguishable on a dashboard.", want, capabilities)
		}
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
		// Deliberately NOT "the target must be an end node" any more.
		//
		// That was the original wording, and it conflated two things: the
		// invariant (a fix that produced no portable handle must never be
		// handed to the review host) and one particular way of honouring it
		// (stop the run). The first is load-bearing and is asserted above.
		// The second turned out to be wrong in practice — ending the flow
		// meant ONE item's capability gap stopped upkeep for every other
		// item, observed on run 01M02JBTMGSY7EZMDMTJWC6BJW, which ended with
		// twelve untouched findings behind the one that could not hand over.
		//
		// What still must hold is that the target cannot act on work it was
		// given no handle for. A `wait` is safe: it dispatches nobody. So the
		// rule is now about the KIND OF NODE that may receive this outcome,
		// not about the run ending.
		switch kind := doc.node(t, target).Kind; kind {
		case "end", "wait":
			// end: the run stops and names the gap.
			// wait: the flow backs off and re-sweeps; no actor is dispatched
			// with an unreadable handoff, which is the whole point.
		default:
			t.Errorf("fix.handoff_unavailable routes to %q (kind %q), want an `end` or "+
				"`wait` node: a missing host capability is not something a dispatching "+
				"node can resolve, and handing this outcome to one is how issue #74's "+
				"403 happened", target, kind)
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
