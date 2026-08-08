package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentculture/culture-nodes/internal/compiler"
)

// compileFixture compiles a testdata workflow the way a publisher would, and
// fails the test on any diagnostic error — a fixture that does not compile
// would make every assertion below a statement about nothing.
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
	if cw == nil {
		t.Fatalf("compile %s produced no workflow", path)
	}
	return cw
}

func loadFixture(t *testing.T, name string) *Workflow {
	t.Helper()
	cw := compileFixture(t, name)
	wf, err := LoadWorkflow(cw.Digest, cw.Normalized)
	if err != nil {
		t.Fatalf("LoadWorkflow(%s): %v", name, err)
	}
	return wf
}

func TestLoadWorkflowDecodesTheNormalizedIR(t *testing.T) {
	wf := loadFixture(t, "loop.workflow.yaml")

	if wf.Entry != "intake" {
		t.Errorf("entry = %q, want intake", wf.Entry)
	}
	if wf.Name != "engine-loop-slice" {
		t.Errorf("name = %q, want engine-loop-slice", wf.Name)
	}
	if got, want := wf.Limits.MaxTransitions, 32; got != want {
		t.Errorf("maxTransitions = %d, want %d", got, want)
	}
	if got, want := wf.Limits.MaxVisitsPerNode, 8; got != want {
		t.Errorf("maxVisitsPerNode = %d, want %d", got, want)
	}
	if wf.Limits.MaxDuration.String() != "1h0m0s" {
		t.Errorf("maxDuration = %s, want 1h0m0s", wf.Limits.MaxDuration)
	}
	if got, want := wf.Limits.MaxParallelTokens, 1; got != want {
		t.Errorf("maxParallelTokens = %d, want %d (the MVP is sequential)", got, want)
	}
	if wf.InputSchema == nil || wf.OutputSchema == nil {
		t.Fatal("the workflow declares inline input and output contracts; both should be compiled")
	}

	work := wf.Nodes["work"]
	if work == nil {
		t.Fatal("node work is missing")
	}
	if got, want := work.Retry.MaxAttempts, 3; got != want {
		t.Errorf("work retry maxAttempts = %d, want %d", got, want)
	}
	if got, want := work.Timeout.String(), "5m0s"; got != want {
		t.Errorf("work timeout = %s, want %s", got, want)
	}
	if got := work.Propose; len(got) != 2 || got[0] != "claim" || got[1] != "result" {
		t.Errorf("work propose = %v, want [claim result]", got)
	}

	check := wf.Nodes["check"]
	if check == nil {
		t.Fatal("node check is missing")
	}
	if !check.declaresOutcome("changes_required") || !check.declaresOutcome("done") {
		t.Errorf("check outcomes = %v, want both done and changes_required", check.Outcomes)
	}
	if check.OutcomeSchemas["done"] == nil {
		t.Error("check.done declares an inline contract; it should be compiled")
	}

	if got := wf.Nodes["finish"].OutputFrom; got != "/nodes/check/output" {
		t.Errorf("finish output binding = %q, want /nodes/check/output", got)
	}
}

// The order edges are evaluated in is the compiler's normalized order, not
// the order the author listed them. That is what makes "first match wins" a
// property of the definition rather than of its formatting.
func TestLoadWorkflowKeepsNormalizedEdgeOrder(t *testing.T) {
	wf := loadFixture(t, "loop.workflow.yaml")

	var got []string
	for _, e := range wf.Edges {
		got = append(got, e.From+"->"+e.To)
	}
	want := []string{
		"check.changes_required->finish",
		"check.changes_required->work",
		"check.done->finish",
		"intake.completed->work",
		"work.completed->check",
	}
	if len(got) != len(want) {
		t.Fatalf("edges = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("edges = %v, want %v", got, want)
		}
	}

	// The guarded edge must be the one evaluated first, or the guard could
	// never win against the unguarded fallback beside it.
	if wf.Edges[0].Guard == nil {
		t.Error("check.changes_required -> finish carries a CEL guard; it should be compiled")
	}
	if wf.Edges[1].Guard != nil {
		t.Error("check.changes_required -> work is unguarded")
	}
}

func TestLoadWorkflowRejectsAnUndecodableIR(t *testing.T) {
	if _, err := LoadWorkflow("sha256:none", []byte(`{"spec":`)); err == nil {
		t.Fatal("a truncated IR should not load")
	}
	if _, err := LoadWorkflow("sha256:none", []byte(`{"spec":{"nodes":{}}}`)); err == nil {
		t.Fatal("an IR with no entry node should not load")
	}
}

// A schemaRef the compiler could not bundle stays unresolved, and the engine
// must not pretend an unresolvable reference is a satisfied contract.
func TestLoadWorkflowLeavesSchemaRefsUnresolved(t *testing.T) {
	wf := loadFixture(t, "../../compiler/testdata/minimal.workflow.yaml")
	if wf.InputSchema != nil {
		t.Error("minimal.workflow.yaml references its input schema by path; nothing should be compiled for it")
	}
	if wf.Nodes["start"].OutcomeSchemas["completed"] != nil {
		t.Error("start.completed references its schema by path; nothing should be compiled for it")
	}
	// And an unresolved contract admits anything rather than refusing
	// everything, which is the only honest reading of "not checked here".
	if err := validatePayload(wf.InputSchema, []byte(`{"anything":true}`)); err != nil {
		t.Errorf("an unresolved contract should admit any payload, got %v", err)
	}
}
