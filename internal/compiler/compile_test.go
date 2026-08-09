package compiler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/schemas"
)

// readFixture reads a testdata file or fails the test.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// compileFixture compiles a testdata file, failing on an internal error (as
// opposed to a diagnostic, which is a statement about the document).
func compileFixture(t *testing.T, name string, format Format) (*CompiledWorkflow, []Diagnostic) {
	t.Helper()
	compiled, diags, err := Compile(readFixture(t, name), format)
	if err != nil {
		t.Fatalf("Compile(%s) returned an internal error: %v", name, err)
	}
	return compiled, diags
}

// TestDeliveryLoopExampleCompilesWithoutErrors is the acceptance test named in
// the build plan: the PRD §11.1 delivery-loop example compiles clean.
func TestDeliveryLoopExampleCompilesWithoutErrors(t *testing.T) {
	compiled, diags := compileFixture(t, "deliver-change.workflow.yaml", FormatYAML)
	for _, d := range diags {
		if d.Level == LevelError {
			t.Errorf("unexpected error diagnostic: %s %s %s: %s", d.Level, d.Path, d.Code, d.Message)
		}
	}
	if compiled == nil {
		t.Fatal("Compile returned no CompiledWorkflow for the PRD §11.1 example")
	}
	if compiled.Name != "deliver-change" || compiled.Version != "1.0.0" {
		t.Errorf("Name/Version = %q/%q, want deliver-change/1.0.0", compiled.Name, compiled.Version)
	}
	if !strings.HasPrefix(compiled.Digest, contracts.DigestPrefix) {
		t.Errorf("Digest = %q, want a %q-prefixed digest", compiled.Digest, contracts.DigestPrefix)
	}
	if len(compiled.Source) == 0 {
		t.Error("CompiledWorkflow.Source is empty; the exact submitted source must be retained (PRD §11.3)")
	}
	if compiled.Format != FormatYAML {
		t.Errorf("Format = %q, want %q", compiled.Format, FormatYAML)
	}
}

// TestDeliveryLoopExampleHasNoWarningsEither is not required by the plan
// (warnings are allowed) but pins the example as a genuinely clean reference:
// every component is digest-pinned and every loop is bounded.
func TestDeliveryLoopExampleHasNoWarningsEither(t *testing.T) {
	_, diags := compileFixture(t, "deliver-change.workflow.yaml", FormatYAML)
	if len(diags) != 0 {
		for _, d := range diags {
			t.Errorf("unexpected diagnostic: %s %s %s: %s", d.Level, d.Path, d.Code, d.Message)
		}
	}
}

// TestCompileIsDeterministic is the digest-stability acceptance criterion:
// compiling the same source twice yields byte-identical IR and one digest.
func TestCompileIsDeterministic(t *testing.T) {
	source := readFixture(t, "deliver-change.workflow.yaml")

	first, _, err := Compile(source, FormatYAML)
	if err != nil {
		t.Fatalf("first Compile: %v", err)
	}
	second, _, err := Compile(source, FormatYAML)
	if err != nil {
		t.Fatalf("second Compile: %v", err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digests differ across identical compilations: %s vs %s", first.Digest, second.Digest)
	}
	if string(first.Normalized) != string(second.Normalized) {
		t.Fatal("normalized IR bytes differ across identical compilations")
	}
}

// TestYAMLAndJSONSourcesAgree proves the PRD's "JSON is canonical, YAML is
// authoring sugar" claim mechanically: the YAML authoring example and the
// canonical JSON rendering of the same workflow compile to one digest.
func TestYAMLAndJSONSourcesAgree(t *testing.T) {
	yamlSource := readFixture(t, "deliver-change.workflow.yaml")
	jsonSource, err := schemas.ExamplesFS.ReadFile("examples/deliver-change.workflow.json")
	if err != nil {
		t.Fatalf("read embedded JSON example: %v", err)
	}

	fromYAML, diags, err := Compile(yamlSource, FormatYAML)
	if err != nil {
		t.Fatalf("Compile(yaml): %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("YAML example produced diagnostics: %+v", diags)
	}
	fromJSON, diags, err := Compile(jsonSource, FormatJSON)
	if err != nil {
		t.Fatalf("Compile(json): %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("JSON example produced diagnostics: %+v", diags)
	}
	if fromYAML.Digest != fromJSON.Digest {
		t.Fatalf("YAML digest %s != JSON digest %s", fromYAML.Digest, fromJSON.Digest)
	}
}

// TestNormalizedIRExpandsDefaultsAndResolvesOwners locks the §11.3 compiler
// output guarantees that a caller can check without a runtime.
func TestNormalizedIRExpandsDefaultsAndResolvesOwners(t *testing.T) {
	compiled, _ := compileFixture(t, "minimal.workflow.yaml", FormatYAML)
	if compiled == nil {
		t.Fatal("minimal fixture did not compile")
	}

	var ir map[string]any
	if err := json.Unmarshal(compiled.Normalized, &ir); err != nil {
		t.Fatalf("normalized IR is not valid JSON: %v", err)
	}

	spec, _ := ir["spec"].(map[string]any)
	limits, _ := spec["limits"].(map[string]any)
	for _, key := range []string{"maxDuration", "maxTransitions", "maxVisitsPerNode", "maxParallelTokens"} {
		if _, ok := limits[key]; !ok {
			t.Errorf("normalized spec.limits is missing expanded default %q", key)
		}
	}
	ledger, _ := spec["ledger"].(map[string]any)
	if ledger["schemaVersion"] != DefaultLedgerSchemaVersion {
		t.Errorf("normalized spec.ledger.schemaVersion = %v, want %q", ledger["schemaVersion"], DefaultLedgerSchemaVersion)
	}
	if ledger["requireProvenance"] != true {
		t.Errorf("normalized spec.ledger.requireProvenance = %v, want true", ledger["requireProvenance"])
	}

	nodes, _ := spec["nodes"].(map[string]any)
	if len(nodes) == 0 {
		t.Fatal("normalized spec.nodes is empty")
	}
	for id, raw := range nodes {
		node, _ := raw.(map[string]any)
		owner, _ := node["ownerRef"].(string)
		if owner == "" {
			t.Errorf("normalized node %q carries no concrete ownerRef (PRD §9.4)", id)
		}
	}

	start, _ := nodes["start"].(map[string]any)
	policy, _ := start["policy"].(map[string]any)
	if policy["timeout"] != DefaultNodeTimeout {
		t.Errorf("node start timeout = %v, want the expanded default %q", policy["timeout"], DefaultNodeTimeout)
	}
	outcomes, _ := start["outcomes"].([]any)
	if len(outcomes) != 1 || outcomes[0] != "completed" {
		t.Errorf("node start outcomes = %v, want [completed]", outcomes)
	}
}

// TestNormalizedIRSeparatesPresentation checks that presentation metadata is
// lifted out of the executable spec (PRD §9.1: presentation never changes
// runtime semantics) while remaining part of the content-addressed document.
func TestNormalizedIRSeparatesPresentation(t *testing.T) {
	compiled, diags := compileFixture(t, "presentation.workflow.yaml", FormatYAML)
	if compiled == nil {
		t.Fatalf("presentation fixture did not compile: %+v", diags)
	}

	var ir map[string]any
	if err := json.Unmarshal(compiled.Normalized, &ir); err != nil {
		t.Fatalf("normalized IR is not valid JSON: %v", err)
	}
	spec, _ := ir["spec"].(map[string]any)
	if _, ok := spec["presentation"]; ok {
		t.Error("spec still carries presentation metadata; it must be separated out")
	}
	nodes, _ := spec["nodes"].(map[string]any)
	start, _ := nodes["start"].(map[string]any)
	if _, ok := start["presentation"]; ok {
		t.Error("node still carries presentation metadata; it must be separated out")
	}

	presentation, ok := ir["presentation"].(map[string]any)
	if !ok {
		t.Fatal("normalized IR has no top-level presentation block")
	}
	if _, ok := presentation["workflow"]; !ok {
		t.Error("presentation.workflow missing")
	}
	presentationNodes, _ := presentation["nodes"].(map[string]any)
	if _, ok := presentationNodes["start"]; !ok {
		t.Error("presentation.nodes.start missing")
	}
}

// TestNormalizedEdgeOrderIsStable checks that authored edge order does not
// change the digest: edges are sorted into one canonical order.
func TestNormalizedEdgeOrderIsStable(t *testing.T) {
	shuffled, _ := compileFixture(t, "edge-order-shuffled.workflow.yaml", FormatYAML)
	ordered, _ := compileFixture(t, "edge-order-ordered.workflow.yaml", FormatYAML)
	if shuffled == nil || ordered == nil {
		t.Fatal("edge-order fixtures did not compile")
	}
	if shuffled.Digest != ordered.Digest {
		t.Fatalf("edge order changed the digest: %s vs %s", shuffled.Digest, ordered.Digest)
	}

	var ir map[string]any
	if err := json.Unmarshal(ordered.Normalized, &ir); err != nil {
		t.Fatalf("normalized IR is not valid JSON: %v", err)
	}
	spec, _ := ir["spec"].(map[string]any)
	edges, _ := spec["edges"].([]any)
	var got []string
	for _, raw := range edges {
		edge, _ := raw.(map[string]any)
		got = append(got, edge["from"].(string)+"->"+edge["to"].(string))
	}
	// Lexicographic by (from node, outcome, target): a canonical order that
	// depends only on the edge set, never on how the author listed it.
	want := []string{"middle.completed->finish", "start.completed->middle", "start.failed->finish"}
	if len(got) != len(want) {
		t.Fatalf("edges = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("edges = %v, want %v", got, want)
		}
	}
}

// TestCompiledCELPrograms checks that compiled CEL programs come back on the
// CompiledWorkflow keyed by the expression's JSON path.
func TestCompiledCELPrograms(t *testing.T) {
	compiled, diags := compileFixture(t, "cel-ok.workflow.yaml", FormatYAML)
	if compiled == nil {
		t.Fatalf("cel-ok fixture did not compile: %+v", diags)
	}
	for _, path := range []string{"/spec/edges/0/when", "/spec/nodes/route/select/0/when"} {
		if _, ok := compiled.Programs[path]; !ok {
			t.Errorf("no compiled CEL program at %q (have %v)", path, programPaths(compiled))
		}
	}
}

func programPaths(c *CompiledWorkflow) []string {
	out := make([]string, 0, len(c.Programs))
	for path := range c.Programs {
		out = append(out, path)
	}
	return out
}

// TestUnboundedCycleIsAWarningNotAnError pins the deliberate leniency: a loop
// with no authored bound is reported, but does not stop compilation.
func TestUnboundedCycleIsAWarningNotAnError(t *testing.T) {
	compiled, diags := compileFixture(t, "unbounded-cycle.workflow.yaml", FormatYAML)
	if compiled == nil {
		t.Fatalf("unbounded-cycle fixture must still compile; diagnostics: %+v", diags)
	}
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %+v, want exactly one", diags)
	}
	if diags[0].Level != LevelWarning || diags[0].Code != CodeGraphUnboundedCycle {
		t.Fatalf("diagnostic = %+v, want a %s warning", diags[0], CodeGraphUnboundedCycle)
	}
}

// TestSyntaxErrorsStopThePipeline checks that an unparseable document yields
// exactly one syntax diagnostic rather than a cascade of downstream noise.
func TestSyntaxErrorsStopThePipeline(t *testing.T) {
	compiled, diags, err := Compile([]byte("apiVersion: [unterminated\n"), FormatYAML)
	if err != nil {
		t.Fatalf("Compile returned an internal error for bad YAML: %v", err)
	}
	if compiled != nil {
		t.Fatal("Compile returned a CompiledWorkflow for unparseable source")
	}
	if len(diags) != 1 || diags[0].Code != CodeSyntaxParse {
		t.Fatalf("diagnostics = %+v, want exactly one %s", diags, CodeSyntaxParse)
	}
	if diags[0].Hint == "" {
		t.Error("syntax diagnostic has no hint")
	}
}

func TestNonObjectDocumentIsASyntaxError(t *testing.T) {
	_, diags, err := Compile([]byte("- a\n- b\n"), FormatYAML)
	if err != nil {
		t.Fatalf("Compile returned an internal error: %v", err)
	}
	if len(diags) != 1 || diags[0].Code != CodeSyntaxNotAnObject {
		t.Fatalf("diagnostics = %+v, want exactly one %s", diags, CodeSyntaxNotAnObject)
	}
}

func TestUnknownFormatIsAnInternalError(t *testing.T) {
	_, _, err := Compile([]byte("{}"), Format("toml"))
	if err == nil {
		t.Fatal("Compile accepted an unknown format")
	}
}

// TestEveryDiagnosticCarriesAHint enforces the agent-first rubric across every
// fixture in testdata: a diagnostic without remediation is not a diagnostic.
func TestEveryDiagnosticCarriesAHint(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		_, diags := compileFixture(t, entry.Name(), FormatYAML)
		for _, d := range diags {
			if d.Message == "" {
				t.Errorf("%s: diagnostic %s at %s has no message", entry.Name(), d.Code, d.Path)
			}
			if d.Hint == "" {
				t.Errorf("%s: diagnostic %s at %s has no hint", entry.Name(), d.Code, d.Path)
			}
			if d.Level != LevelError && d.Level != LevelWarning {
				t.Errorf("%s: diagnostic %s has level %q", entry.Name(), d.Code, d.Level)
			}
			// One diagnostic is one line: `nodes validate` prints them line
			// by line, and an embedded newline would break every reader of
			// that output.
			if strings.ContainsAny(d.Message+d.Hint, "\n\r") {
				t.Errorf("%s: diagnostic %s spans multiple lines: %q / %q", entry.Name(), d.Code, d.Message, d.Hint)
			}
		}
	}
}

func TestFormatForPath(t *testing.T) {
	cases := map[string]Format{
		"a.json":          FormatJSON,
		"a.JSON":          FormatJSON,
		"a.yaml":          FormatYAML,
		"a.yml":           FormatYAML,
		"workflow":        FormatYAML,
		"dir.json/a.yaml": FormatYAML,
	}
	for path, want := range cases {
		if got := FormatForPath(path); got != want {
			t.Errorf("FormatForPath(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestValidHooksCarryIntoTheNormalizedIR is the "valid hooks in IR"
// acceptance case for task t14: a workflow whose agent nodes declare
// pre_run/post_run compiles clean, and both hook shapes — an outcome-routed
// on_failure and the reject_assurance sentinel — survive normalization with
// their operation defaults expanded exactly like a code node's own
// operation.
func TestValidHooksCarryIntoTheNormalizedIR(t *testing.T) {
	compiled, diags := compileFixture(t, "hooks-ok.workflow.yaml", FormatYAML)
	if compiled == nil {
		t.Fatalf("hooks-ok fixture did not compile: %s", renderDiagnostics(diags))
	}
	if len(diags) != 0 {
		t.Errorf("hooks-ok fixture produced diagnostics, want none: %s", renderDiagnostics(diags))
	}

	var ir map[string]any
	if err := json.Unmarshal(compiled.Normalized, &ir); err != nil {
		t.Fatalf("normalized IR is not valid JSON: %v", err)
	}
	nodes := ir["spec"].(map[string]any)["nodes"].(map[string]any)

	work, ok := nodes["work"].(map[string]any)
	if !ok {
		t.Fatal("normalized IR has no node \"work\"")
	}
	preRun, ok := work["pre_run"].(map[string]any)
	if !ok {
		t.Fatal("node work carries no pre_run in the normalized IR")
	}
	preOp, ok := preRun["operation"].(map[string]any)
	if !ok {
		t.Fatal("pre_run carries no operation")
	}
	if preOp["image"] != "registry.example/guard@sha256:111111" {
		t.Errorf("pre_run.operation.image = %v, want the authored image", preOp["image"])
	}
	// The same PRD §13.7 safe defaults a code node's own operation gets.
	if preOp["network"] != DefaultNetwork {
		t.Errorf("pre_run.operation.network = %v, want the expanded default %q", preOp["network"], DefaultNetwork)
	}
	if preOp["workingDirectory"] != DefaultWorkingDirectory {
		t.Errorf("pre_run.operation.workingDirectory = %v, want %q", preOp["workingDirectory"], DefaultWorkingDirectory)
	}
	if preOp["requiresShell"] != false {
		t.Errorf("pre_run.operation.requiresShell = %v, want false", preOp["requiresShell"])
	}

	postRun, ok := work["post_run"].(map[string]any)
	if !ok {
		t.Fatal("node work carries no post_run in the normalized IR")
	}
	onFailure, ok := postRun["on_failure"].(map[string]any)
	if !ok {
		t.Fatal("node work post_run.on_failure did not survive normalization as an object")
	}
	if onFailure["outcome"] != "changes_required" {
		t.Errorf("post_run.on_failure.outcome = %v, want changes_required", onFailure["outcome"])
	}

	guarded, ok := nodes["guarded"].(map[string]any)
	if !ok {
		t.Fatal("normalized IR has no node \"guarded\"")
	}
	guardedPostRun, ok := guarded["post_run"].(map[string]any)
	if !ok {
		t.Fatal("node guarded carries no post_run in the normalized IR")
	}
	if guardedPostRun["on_failure"] != "reject_assurance" {
		t.Errorf("guarded post_run.on_failure = %v, want the reject_assurance sentinel", guardedPostRun["on_failure"])
	}

	// A node that declares neither hook carries neither key — pre_run/post_run
	// are additive, not a shape every node grows.
	finish, ok := nodes["finish"].(map[string]any)
	if !ok {
		t.Fatal("normalized IR has no node \"finish\"")
	}
	if _, ok := finish["pre_run"]; ok {
		t.Error("node finish carries a pre_run it never declared")
	}
	if _, ok := finish["post_run"]; ok {
		t.Error("node finish carries a post_run it never declared")
	}
}

func TestResolveOwnerFallsBackToWorkflowMetadata(t *testing.T) {
	if got := resolveOwner("", "team/platform-ai"); got != "team/platform-ai" {
		t.Errorf("resolveOwner(\"\", metadata) = %q, want the metadata default", got)
	}
	if got := resolveOwner("team/node", "team/platform-ai"); got != "team/node" {
		t.Errorf("resolveOwner(node, metadata) = %q, want the node's own owner", got)
	}
	if got := resolveOwner("", ""); got != "" {
		t.Errorf("resolveOwner(\"\", \"\") = %q, want the empty string", got)
	}
}
