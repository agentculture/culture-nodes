package compiler

import (
	"encoding/json"
	"strings"
	"testing"
)

// Declared actor affinity (issue #107, task t33, acceptance criterion 3).
//
// "Declared means declared in the workflow, not inferred. Keep it data." So
// affinity is a block in the spec, it compiles like every other declaration,
// and the compiler refuses the ones it can prove wrong at publication time
// rather than at 3am inside a dispatch.

func affinityDoc(affinity string) []byte {
	return []byte(`apiVersion: nodes.culture.dev/v1alpha1
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
    finish: {kind: end, ownerRef: team/platform-ai, output: {from: /nodes/fix/output}}
  edges: [{from: fix.completed, to: finish}]
`)
}

func compileAffinity(t *testing.T, affinity string) (*CompiledWorkflow, []Diagnostic) {
	t.Helper()
	cw, diags, err := Compile(affinityDoc(affinity), FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	return cw, diags
}

func affinityErrors(diags []Diagnostic) []Diagnostic {
	var out []Diagnostic
	for _, d := range diags {
		if d.Level == LevelError {
			out = append(out, d)
		}
	}
	return out
}

func TestAffinityCompilesIntoThePinnedIR(t *testing.T) {
	cw, diags := compileAffinity(t, `  affinity:
    - name: security-findings
      node: fix
      actor: actor://company/security-developer
      when: event.payload.kind == "security"
    - name: default
      node: fix
      actor: actor://company/developer`)
	if errs := affinityErrors(diags); len(errs) > 0 {
		t.Fatalf("valid affinity produced errors: %+v", errs)
	}

	var ir struct {
		Spec struct {
			Affinity []struct {
				Name, Node, Actor, When string
			} `json:"affinity"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(cw.Normalized, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Spec.Affinity) != 2 {
		t.Fatalf("IR carries %d affinity rules, want 2", len(ir.Spec.Affinity))
	}
	got := ir.Spec.Affinity[0]
	if got.Name != "security-findings" || got.Node != "fix" ||
		got.Actor != "actor://company/security-developer" || got.When == "" {
		t.Fatalf("first affinity rule round-tripped as %+v", got)
	}
	// Declaration order is load-bearing: first match wins, so the IR must
	// preserve it rather than sort or map it.
	if ir.Spec.Affinity[1].Name != "default" {
		t.Fatalf("affinity order was not preserved: %+v", ir.Spec.Affinity)
	}
}

func TestAWorkflowWithoutAffinityKeepsItsExactBytes(t *testing.T) {
	// The IR's bytes are what the content digest addresses, so an optional
	// block that is absent must serialise as absent -- otherwise every
	// already-published workflow's digest changes the day this ships.
	cw, diags := compileAffinity(t, "")
	if errs := affinityErrors(diags); len(errs) > 0 {
		t.Fatalf("errors: %+v", errs)
	}
	if strings.Contains(string(cw.Normalized), "affinity") {
		t.Fatalf("a workflow that declares no affinity emitted the key anyway:\n%s", cw.Normalized)
	}
}

func TestAffinityIsRefusedWhenItCannotPossiblyRoute(t *testing.T) {
	for _, tc := range []struct {
		name     string
		affinity string
		wantCode string
	}{
		{
			name: "names a node that does not exist",
			affinity: `  affinity:
    - node: nonesuch
      actor: actor://company/developer`,
			wantCode: CodeAffinityNodeUnknown,
		},
		{
			name: "names a node whose kind dispatches to no actor",
			affinity: `  affinity:
    - node: finish
      actor: actor://company/developer`,
			wantCode: CodeAffinityNodeNotDispatchable,
		},
		{
			name: "names something that is not an actor reference",
			affinity: `  affinity:
    - node: fix
      actor: runner://company/build`,
			wantCode: CodeAffinityActorInvalid,
		},
		{
			name: "declares a condition that does not compile",
			affinity: `  affinity:
    - node: fix
      actor: actor://company/developer
      when: event.payload.kind ==`,
			wantCode: CodeContractCELInvalid,
		},
		{
			name: "declares two unconditional defaults for one node",
			affinity: `  affinity:
    - node: fix
      actor: actor://company/a
    - node: fix
      actor: actor://company/b`,
			wantCode: CodeAffinityDuplicateDefault,
		},
		{
			name: "buries a default where nothing after it can ever match",
			affinity: `  affinity:
    - node: fix
      actor: actor://company/a
    - node: fix
      actor: actor://company/b
      when: event.payload.kind == "security"`,
			wantCode: CodeAffinityUnreachableRule,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := compileAffinity(t, tc.affinity)
			for _, d := range affinityErrors(diags) {
				if d.Code == tc.wantCode {
					return
				}
			}
			t.Fatalf("affinity that %s was accepted; wanted diagnostic %s, got %+v",
				tc.name, tc.wantCode, affinityErrors(diags))
		})
	}
}
