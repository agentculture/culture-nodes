package api

// Task t35 (#328; spec c86, honesty h59): the declaration-output
// counterparts of workflow_generations_test.go's unit tests, plus the pure
// plan -> declaration mapping plan import uses. Every workflow-output test
// in workflow_generations_test.go stays as it is: the graph engine keeps
// running until t37, so the declaration form is added beside the workflow
// form, never in place of it.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/devague"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// Counterpart of TestWorkflowGenerationTemplateCompilesAndRoutesExhaustion:
// the orchestration that asks an actor for declarations is still a graph
// run (the engine switch is 'before'), and it must compile, dispatch only
// through the actor ref, route exhaustion as a domain outcome, and demand a
// declaration set -- not workflow source -- from the actor.
func TestDeclarationGenerationTemplateCompilesAndRoutesExhaustion(t *testing.T) {
	source := renderGeneration("actor://company/planner@sha256:aaaaaaaa", generationOutputDeclarations)
	compiled, diagnostics, err := compiler.Compile([]byte(source), compiler.FormatYAML)
	if err != nil || compiled == nil {
		t.Fatalf("declaration generation workflow compile: compiled=%v err=%v diagnostics=%+v", compiled != nil, err, diagnostics)
	}
	if strings.Contains(source, "openai") || strings.Contains(source, "anthropic") {
		t.Fatal("generation orchestration names a model provider; it must dispatch through the actor ref")
	}
	if !strings.Contains(source, "from: generate.generation_exhausted\n      to: exhausted") {
		t.Fatal("generation exhaustion is not routed as a domain outcome")
	}
	if !strings.Contains(source, "required: [format, declarations]") {
		t.Fatal("the generated outcome does not require a declaration set")
	}
	if strings.Contains(source, "required: [format, source]") {
		t.Fatal("declaration output must not ask the actor for workflow source")
	}
	// The default rendering is byte-identical to today's workflow lane.
	if renderGeneration("actor://company/planner@sha256:aaaaaaaa", generationOutputWorkflow) != renderWorkflowGeneration("actor://company/planner@sha256:aaaaaaaa") {
		t.Fatal("workflow output rendering drifted from the pre-t35 template")
	}
}

// Counterpart of TestGenerationInstructionRequiresExactServerValidationAndNeverPublish.
func TestDeclarationGenerationInstructionRequiresDeclarationValidationAndNeverPublishOrActivate(t *testing.T) {
	instruction := declarationGenerationInstruction("make a review flow", "sha256:base", "old source")
	for _, required := range []string{
		"POST /v1alpha1/declarations/validate", "valid=true", "Do not publish", "Do not activate",
		"must", "can", "sha256:base", "old source",
	} {
		if !strings.Contains(instruction, required) {
			t.Errorf("instruction does not contain %q", required)
		}
	}
	if strings.Contains(instruction, "/v1alpha1/workflows/validate") {
		t.Error("declaration instruction points the actor at the workflow validator")
	}
}

// Counterpart of TestSourceDiffNamesPinnedBaseAndChangedLines: a generated
// declaration whose name is already published is an EDIT, and its diff
// names the published version it replaces -- never a silent replacement.
func TestDeclarationDiffNamesPublishedBaseAndChangedLines(t *testing.T) {
	base := []byte(`{"action":{"kind":"agent.work"},"name":"x"}`)
	proposed := []byte(`{"action":{"kind":"code.run"},"name":"x"}`)
	diff := declarationDiff("sha256:base", base, proposed)
	if !strings.HasPrefix(diff, "--- sha256:base\n+++ proposed\n") {
		t.Fatalf("diff does not name the published base: %q", diff)
	}
	if !strings.Contains(diff, `-    "kind": "agent.work"`) || !strings.Contains(diff, `+    "kind": "code.run"`) {
		t.Fatalf("diff does not mark the changed lines: %q", diff)
	}
}

func TestGenerationOutputSelectorDefaultsToWorkflow(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want generationOutput
		ok   bool
	}{
		{"", generationOutputWorkflow, true},
		{"workflow", generationOutputWorkflow, true},
		{"declarations", generationOutputDeclarations, true},
		{"graph", "", false},
	} {
		got, ok := parseGenerationOutput(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseGenerationOutput(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPlanImportOutputSelectorDefaultsToSnapshot(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"", planImportOutputSnapshot, true},
		{"snapshot", planImportOutputSnapshot, true},
		{"declarations", planImportOutputDeclarations, true},
		{"workflow", "", false},
	} {
		got, ok := parsePlanImportOutput(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parsePlanImportOutput(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// The plan -> declaration mapping over the SAME real devague fixture the
// snapshot tests use (internal/devague/testdata/plan-show.json): one
// declaration per active task, a 'must' link for every real dependency
// edge, the rejected task dropped, and every emitted declaration passing
// the real declaration parser.
func TestPlanDeclarationSetMapsTasksAndDependencyEdges(t *testing.T) {
	raw := mustReadDevagueFixture(t, "plan-show.json")
	plan, err := devague.ParsePlanShow(raw)
	if err != nil {
		t.Fatal(err)
	}
	items, links := planDeclarationSet(plan, "actor://company/developer")
	if len(items) != 4 {
		t.Fatalf("items = %d, want 4 (t5 is rejected and must not be emitted)", len(items))
	}
	byName := map[string]*decl.Declaration{}
	for _, item := range items {
		d, err := decl.Parse([]byte(item.Source), decl.Format(item.Format))
		if err != nil {
			t.Fatalf("emitted declaration does not parse: %v\n%s", err, item.Source)
		}
		byName[d.Name] = d
	}
	for _, want := range []string{"plan-t22fixture-t1", "plan-t22fixture-t2", "plan-t22fixture-t3", "plan-t22fixture-t4"} {
		if byName[want] == nil {
			t.Fatalf("missing declaration %q among %v", want, keys(byName))
		}
	}
	if byName["plan-t22fixture-t5"] != nil {
		t.Fatal("a rejected task was emitted as a declaration")
	}
	// Roots start from the plan's kickoff node on a human decision; a
	// dependent starts from a predecessor's landing node on its PR approval.
	root := byName["plan-t22fixture-t1"]
	if root.Trigger.Kind != "human.decision" || root.StartNode.Name != "plan-t22fixture-start" {
		t.Fatalf("root trigger/start = %s/%s", root.Trigger.Kind, root.StartNode.Name)
	}
	if root.Action.Kind != "agent.work" || !strings.Contains(string(root.Action.With), `"uses":"actor://company/developer"`) {
		t.Fatalf("root action = %s %s", root.Action.Kind, root.Action.With)
	}
	dep := byName["plan-t22fixture-t3"]
	if dep.Trigger.Kind != "github.pr.approved" || dep.StartNode.Name != root.LandingNode.Name {
		t.Fatalf("dependent t3 trigger/start = %s/%s, want github.pr.approved/%s", dep.Trigger.Kind, dep.StartNode.Name, root.LandingNode.Name)
	}
	for _, d := range byName {
		if d.StartNode.Deadline == "" || d.LandingNode.Deadline == "" {
			t.Fatalf("%s: every node declares a deadline or 'none'", d.Name)
		}
	}
	want := map[string]bool{
		"plan-t22fixture-t3->plan-t22fixture-t1:must": true,
		"plan-t22fixture-t4->plan-t22fixture-t1:must": true,
		"plan-t22fixture-t4->plan-t22fixture-t2:must": true,
	}
	if len(links) != len(want) {
		t.Fatalf("links = %+v, want %d", links, len(want))
	}
	for _, l := range links {
		if !want[l.From+"->"+l.To+":"+l.Kind] {
			t.Fatalf("unexpected link %+v", l)
		}
	}
}

// A task's own prose is data, not a template: braces in a summary or
// instruction are escaped so they render literally and never become a
// {step:name} reference the engine would try to resolve.
func TestPlanDeclarationEscapesTemplateBraces(t *testing.T) {
	plan := devague.PlanShow{Slug: "Brace_Plan", Tasks: []devague.PlanTask{{
		ID: "T1", Summary: "emit {format, source}", Instruction: "return {a:b} verbatim",
		Origin: ledger.OriginHuman, SourceStatus: "confirmed", AcceptanceCriteria: []string{"{x}"},
	}}}
	items, links := planDeclarationSet(plan, "actor://company/developer")
	if len(items) != 1 || len(links) != 0 {
		t.Fatalf("items=%d links=%d", len(items), len(links))
	}
	d, err := decl.Parse([]byte(items[0].Source), decl.Format(items[0].Format))
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "plan-brace-plan-t1" {
		t.Fatalf("name = %q, want the sanitised plan-brace-plan-t1", d.Name)
	}
	refs, err := actionTemplateReferences(d.Action)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("task prose produced template references %+v; braces must be escaped", refs)
	}
	var with struct {
		Input map[string]any `json:"input"`
	}
	if err := json.Unmarshal(d.Action.With, &with); err != nil {
		t.Fatal(err)
	}
	if with.Input["summary"] != "emit {{format, source}" {
		t.Fatalf("summary = %q, want the escaped form", with.Input["summary"])
	}
}

func keys(m map[string]*decl.Declaration) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustReadDevagueFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../devague/testdata/" + name)
	if err != nil {
		t.Fatalf("read internal/devague/testdata/%s: %v", name, err)
	}
	return data
}
