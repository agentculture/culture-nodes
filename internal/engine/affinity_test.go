package engine

import (
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/compiler"
)

// Affinity resolution (issue #107, task t33, acceptance criterion 3): the
// engine picks the declared actor from the triggering event, and produces the
// value that gets recorded on the run.

func loadAffinityWorkflow(t *testing.T, affinity string) *Workflow {
	t.Helper()
	source := []byte(`apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow
metadata: {name: affine, version: 1.0.0, ownerRef: team/platform-ai}
spec:
  entry: fix
` + affinity + `
  contract:
    input: {schema: {type: object}}
    output: {schema: {type: object}}
  nodes:
    fix:
      kind: agent
      ownerRef: team/platform-ai
      uses: actor://company/developer@sha256:aaaaaa
      contract: {outcomes: {completed: {schema: {type: object}}}}
    review:
      kind: agent
      ownerRef: team/platform-ai
      uses: actor://company/reviewer@sha256:bbbbbb
      contract: {outcomes: {completed: {schema: {type: object}}}}
    finish: {kind: end, ownerRef: team/platform-ai, output: {from: /nodes/fix/output}}
  edges:
    - {from: fix.completed, to: review}
    - {from: review.completed, to: finish}
`)
	cw, diags, err := compiler.Compile(source, compiler.FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diags {
		if d.Level == compiler.LevelError {
			t.Fatalf("%s %s: %s", d.Path, d.Code, d.Message)
		}
	}
	wf, err := LoadWorkflow(cw.Digest, cw.Normalized)
	if err != nil {
		t.Fatalf("LoadWorkflow: %v", err)
	}
	return wf
}

const twoRules = `  affinity:
    - name: security-findings
      node: fix
      actor: actor://company/security-developer
      when: event.payload.kind == "security"
    - name: general-findings
      node: fix
      actor: actor://company/developer`

func TestAffinityPicksTheDeclaredActorForTheMatchingEvent(t *testing.T) {
	wf := loadAffinityWorkflow(t, twoRules)

	resolved, err := wf.ResolveAffinity(PickupEvent{
		Name: "finding", Emitter: "sweep", Payload: json.RawMessage(`{"kind":"security"}`)})
	if err != nil {
		t.Fatalf("ResolveAffinity: %v", err)
	}
	got, ok := resolved["fix"]
	if !ok {
		t.Fatalf("no affinity resolved for node fix; got %+v", resolved)
	}
	if got.Actor != "actor://company/security-developer" {
		t.Fatalf("Actor = %q, want the security rule's actor", got.Actor)
	}
	if got.Rule != "security-findings" {
		t.Fatalf("Rule = %q, want the matching rule's declared name — the comparative record slices by it", got.Rule)
	}
}

func TestAffinityFallsThroughToTheUnconditionalRule(t *testing.T) {
	wf := loadAffinityWorkflow(t, twoRules)

	resolved, err := wf.ResolveAffinity(PickupEvent{
		Name: "finding", Emitter: "sweep", Payload: json.RawMessage(`{"kind":"dependency"}`)})
	if err != nil {
		t.Fatalf("ResolveAffinity: %v", err)
	}
	if resolved["fix"].Actor != "actor://company/developer" {
		t.Fatalf("Actor = %q, want the unconditional rule's actor", resolved["fix"].Actor)
	}
	if resolved["fix"].Rule != "general-findings" {
		t.Fatalf("Rule = %q, want general-findings", resolved["fix"].Rule)
	}
}

func TestAffinityResolvesNothingWhenNoRuleMatches(t *testing.T) {
	wf := loadAffinityWorkflow(t, `  affinity:
    - node: fix
      actor: actor://company/security-developer
      when: event.payload.kind == "security"`)

	resolved, err := wf.ResolveAffinity(PickupEvent{
		Name: "finding", Emitter: "sweep", Payload: json.RawMessage(`{"kind":"typo"}`)})
	if err != nil {
		t.Fatalf("ResolveAffinity: %v", err)
	}
	if len(resolved) != 0 {
		t.Fatalf("resolved %+v when no rule matched; the node must keep its declared uses", resolved)
	}
}

// TestAffinityRoutesEachNodeIndependently: one event, two nodes, two rules.
// A workflow whose findings go to a developer AND whose review goes to a
// different actor has to be expressible, or "route by affinity" only ever
// means "route the entry node".
func TestAffinityRoutesEachNodeIndependently(t *testing.T) {
	wf := loadAffinityWorkflow(t, `  affinity:
    - name: fixer
      node: fix
      actor: actor://company/security-developer
      when: event.payload.kind == "security"
    - name: reviewer
      node: review
      actor: actor://company/senior-reviewer
      when: event.payload.kind == "security"`)

	resolved, err := wf.ResolveAffinity(PickupEvent{
		Name: "finding", Emitter: "sweep", Payload: json.RawMessage(`{"kind":"security"}`)})
	if err != nil {
		t.Fatalf("ResolveAffinity: %v", err)
	}
	if resolved["fix"].Actor != "actor://company/security-developer" {
		t.Fatalf("fix routed to %q", resolved["fix"].Actor)
	}
	if resolved["review"].Actor != "actor://company/senior-reviewer" {
		t.Fatalf("review routed to %q", resolved["review"].Actor)
	}
}

// TestAnUnevaluableAffinityConditionIsARefusalNotASilentDefault: a condition
// that errors (a missing key on an unexpected payload shape) must not quietly
// fall through to the next rule, because the next rule is usually the
// catch-all default and the run would then look like it routed correctly.
func TestAnUnevaluableAffinityConditionIsARefusalNotASilentDefault(t *testing.T) {
	wf := loadAffinityWorkflow(t, twoRules)

	// No `kind` key at all: the condition cannot be decided.
	_, err := wf.ResolveAffinity(PickupEvent{
		Name: "finding", Emitter: "sweep", Payload: json.RawMessage(`{"other":"shape"}`)})
	if err == nil {
		t.Fatal("an affinity condition that could not be evaluated resolved silently instead of refusing")
	}
}

func TestAWorkflowWithNoAffinityResolvesNothing(t *testing.T) {
	wf := loadAffinityWorkflow(t, "")
	resolved, err := wf.ResolveAffinity(PickupEvent{Name: "finding", Emitter: "sweep"})
	if err != nil {
		t.Fatalf("ResolveAffinity: %v", err)
	}
	if resolved != nil {
		t.Fatalf("a workflow declaring no affinity resolved %+v, want nil", resolved)
	}
}

func TestResolvedAffinityMarshalsToTheShapeTheRunColumnStores(t *testing.T) {
	wf := loadAffinityWorkflow(t, twoRules)
	resolved, err := wf.ResolveAffinity(PickupEvent{
		Name: "finding", Emitter: "sweep", Payload: json.RawMessage(`{"kind":"security"}`)})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"fix":{"actor":"actor://company/security-developer","rule":"security-findings"}}`
	if string(encoded) != want {
		t.Fatalf("resolved affinity encodes as\n  %s\nwant\n  %s", encoded, want)
	}
}
