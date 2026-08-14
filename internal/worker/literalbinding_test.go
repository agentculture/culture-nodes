package worker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/compiler"
)

// Issue #73, option A, at the dispatch end. A literal binding is a value the
// author wrote into the graph; resolving one is a copy, not a read, so it
// touches none of the four surfaces and cannot fail for a runtime reason.
// These tests pin that, and pin the thing that makes the shape safe: the
// worker decodes exactly what the compiler emits.

func TestResolveNodeInputMixesLiteralsAndPointers(t *testing.T) {
	got, err := resolveNodeInput(context.Background(), testSources(t), &inputBinding{
		Bindings: map[string]bindingValue{
			"subject": {Pointer: "/run/input/subject"},
			"observe": {Literal: json.RawMessage(`{"kind":"github_pr_merged","pr":42}`)},
			"retries": {Literal: json.RawMessage(`3`)},
			"note":    {Literal: json.RawMessage(`null`)},
		},
	})
	if err != nil {
		t.Fatalf("resolve mixed bindings: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decode resolved bindings: %v", err)
	}
	if len(decoded) != 4 {
		t.Fatalf("resolved %d bindings, want 4: %s", len(decoded), got)
	}
	assertEqualJSON(t, decoded["subject"], `"widget"`)
	assertEqualJSON(t, decoded["observe"], `{"kind":"github_pr_merged","pr":42}`)
	assertEqualJSON(t, decoded["retries"], `3`)
	assertEqualJSON(t, decoded["note"], `null`)
}

// A literal is a constant, so resolution must not consult any surface. The
// sources here answer nothing at all: a resolver that reached for one would
// panic on the nil closures rather than quietly returning an empty value.
func TestLiteralBindingNeedsNoSources(t *testing.T) {
	got, err := resolveNodeInput(context.Background(), bindingSources{}, &inputBinding{
		Bindings: map[string]bindingValue{
			"observe": {Literal: json.RawMessage(`{"kind":"github_pr_reply"}`)},
		},
	})
	if err != nil {
		t.Fatalf("resolve a literal with no sources: %v", err)
	}
	assertEqualJSON(t, got, `{"observe":{"kind":"github_pr_reply"}}`)
}

// A binding map holding only literals is still a declared binding: it must not
// fall through to the undeclared-binding `{}`.
func TestLiteralOnlyBindingIsDeclared(t *testing.T) {
	binding := &inputBinding{Bindings: map[string]bindingValue{
		"observe": {Literal: json.RawMessage(`{"kind":"github_pr_merged"}`)},
	}}
	if !binding.declared() {
		t.Fatal("a literal-only bindings map reports itself undeclared")
	}
}

// TestWorkerDecodesTheCompilersLiteralIR is the cross-layer guard: the worker's
// IR mirror is a separate decode from the compiler's model (the same reason
// this package carries its own parsePointer), so the two shapes are only in
// agreement if something checks. The compiler's own normalized bytes are the
// input here — not a hand-written fixture that could drift from them.
func TestWorkerDecodesTheCompilersLiteralIR(t *testing.T) {
	compiled, diags, err := compiler.Compile([]byte(literalBindingWorkflow), compiler.FormatYAML)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled == nil {
		t.Fatalf("literal-binding workflow did not compile: %+v", diags)
	}

	var ir irDocument
	if err := json.Unmarshal(compiled.Normalized, &ir); err != nil {
		t.Fatalf("decode the compiler's normalized IR: %v", err)
	}
	bindings := ir.Spec.Nodes["start"].Input.Bindings
	if got := bindings["instruction"]; got.Pointer != "/run/input/subject" || got.isLiteral() {
		t.Errorf("instruction = %+v, want the pointer /run/input/subject", got)
	}
	observe := bindings["observe"]
	if !observe.isLiteral() {
		t.Fatalf("observe = %+v, want a literal", observe)
	}
	assertEqualJSON(t, observe.Literal, `{"kind":"github_pr_merged","pr":42}`)

	resolved, err := resolveNodeInput(context.Background(), testSources(t), ir.Spec.Nodes["start"].Input)
	if err != nil {
		t.Fatalf("resolve the compiled binding: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(resolved, &decoded); err != nil {
		t.Fatalf("decode resolved payload: %v", err)
	}
	assertEqualJSON(t, decoded["observe"], `{"kind":"github_pr_merged","pr":42}`)
}

const literalBindingWorkflow = `
apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow
metadata:
  name: literal-binding
  version: 1.0.0
  ownerRef: team/platform-ai
spec:
  entry: start
  contract:
    input:
      schemaRef: ./in.schema.json
    output:
      schemaRef: ./out.schema.json
  nodes:
    start:
      kind: agent
      ownerRef: team/platform-ai
      uses: actor://company/start@sha256:aaaaaa
      contract:
        outcomes:
          completed:
            schemaRef: ./start.schema.json
      input:
        bindings:
          instruction: /run/input/subject
          observe:
            literal:
              kind: github_pr_merged
              pr: 42
    finish:
      kind: end
      ownerRef: team/platform-ai
      output:
        from: /nodes/start/output
  edges:
    - from: start.completed
      to: finish
`
