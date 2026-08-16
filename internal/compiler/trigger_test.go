package compiler

import (
	"encoding/json"
	"testing"
)

func TestWorkflowTriggerCompilesConditionIntoPinnedIR(t *testing.T) {
	source := []byte(`apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow
metadata: {name: triggered, version: 1.0.0, ownerRef: team/platform-ai}
spec:
  entry: start
  triggers:
    - onEvent: pull-request
      when: event.payload.action == "opened"
  contract:
    input: {schema: {type: object}}
    output: {schema: {type: object}}
  nodes:
    start:
      kind: agent
      ownerRef: team/platform-ai
      uses: actor://company/start@sha256:aaaaaa
      contract: {outcomes: {completed: {schema: {type: object}}}}
    finish: {kind: end, ownerRef: team/platform-ai, output: {from: /nodes/start/output}}
  edges: [{from: start.completed, to: finish}]
`)
	cw, diags, err := Compile(source, FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diags {
		if d.Level == LevelError {
			t.Fatalf("%s %s: %s", d.Path, d.Code, d.Message)
		}
	}
	var ir IR
	if err := json.Unmarshal(cw.Normalized, &ir); err != nil {
		t.Fatal(err)
	}
	if len(ir.Spec.Triggers) != 1 || ir.Spec.Triggers[0].OnEvent != "pull-request" {
		t.Fatalf("normalized triggers = %+v", ir.Spec.Triggers)
	}
	if cw.Programs["/spec/triggers/0/when"] == nil {
		t.Fatal("trigger CEL program was not compiled")
	}
}

func TestWorkflowWithoutTriggerKeepsTriggerKeyOutOfIR(t *testing.T) {
	cw, diags := compileFixture(t, "minimal.workflow.yaml", FormatYAML)
	for _, d := range diags {
		if d.Level == LevelError {
			t.Fatal(d.Message)
		}
	}
	if containsKey(t, cw.Normalized, "triggers") {
		t.Fatal("triggerless workflow IR moved")
	}
}
