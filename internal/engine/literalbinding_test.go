package engine

import (
	"encoding/json"
	"testing"
	"time"
)

// Issue #73, option A, at the engine end. The engine does not resolve bindings
// into dispatch payloads — it records the REFERENCE — so what it owes a literal
// is fidelity: the value the author wrote into the graph must reach the human
// task's context_refs as that value, not as a pointer standing in for it and
// not as a mangled re-encoding.

func TestLoadWorkflowDecodesLiteralBindings(t *testing.T) {
	wf := loadFixture(t, "approval-literal.workflow.yaml")

	review := wf.Nodes["review"]
	if review == nil {
		t.Fatal("node review is missing")
	}
	if got := review.InputBindings["scope"]; got.Pointer != "/nodes/intake/output/scope" || got.IsLiteral() {
		t.Errorf("scope binding = %+v, want the pointer /nodes/intake/output/scope", got)
	}
	observe := review.InputBindings["observe"]
	if !observe.IsLiteral() {
		t.Fatalf("observe binding = %+v, want a literal", observe)
	}
	if observe.Pointer != "" {
		t.Errorf("observe binding carries pointer %q as well as a literal", observe.Pointer)
	}
	var decoded map[string]any
	if err := json.Unmarshal(observe.Literal, &decoded); err != nil {
		t.Fatalf("decode the literal: %v (%s)", err, observe.Literal)
	}
	if decoded["kind"] != "github_pr_merged" {
		t.Errorf("literal kind = %v, want github_pr_merged", decoded["kind"])
	}
}

// The human task is what a person is actually shown. A literal must appear in
// its context_refs as the declared value, wrapped exactly as the author wrote
// it — a reader of the task should be able to name the observable without
// opening the workflow.
func TestHumanTaskContextCarriesLiteralBindings(t *testing.T) {
	wf := loadFixture(t, "approval-literal.workflow.yaml")

	request, err := buildHumanTaskRequest(
		wf.Nodes["review"],
		Run{ID: "run_1", WorkflowDigest: wf.Digest},
		NodeRun{TokenID: "tok_1"},
		"intake", "completed",
		time.Unix(0, 0).UTC(),
	)
	if err != nil {
		t.Fatalf("buildHumanTaskRequest: %v", err)
	}

	var payload struct {
		ContextRefs struct {
			From     string                     `json:"from"`
			Bindings map[string]json.RawMessage `json:"bindings"`
		} `json:"context_refs"`
	}
	if err := json.Unmarshal(request, &payload); err != nil {
		t.Fatalf("decode human task request: %v (%s)", err, request)
	}
	if payload.ContextRefs.From != "" {
		t.Errorf("context_refs.from = %q, want empty: this node declares named bindings", payload.ContextRefs.From)
	}
	if got := string(payload.ContextRefs.Bindings["scope"]); got != `"/nodes/intake/output/scope"` {
		t.Errorf("context_refs.bindings.scope = %s, want the pointer string", got)
	}
	var literal struct {
		Literal struct {
			Kind string `json:"kind"`
			PR   int    `json:"pr"`
		} `json:"literal"`
	}
	if err := json.Unmarshal(payload.ContextRefs.Bindings["observe"], &literal); err != nil {
		t.Fatalf("decode context_refs.bindings.observe: %v (%s)", err, payload.ContextRefs.Bindings["observe"])
	}
	if literal.Literal.Kind != "github_pr_merged" || literal.Literal.PR != 42 {
		t.Errorf("context_refs.bindings.observe = %s, want the declared observable", payload.ContextRefs.Bindings["observe"])
	}
}

// A hand-built IR (the engine accepts one; several tests here construct
// workflows directly) must reject an ambiguous binding value rather than
// silently reading it as an empty pointer.
func TestInputBindingRejectsAmbiguousShapes(t *testing.T) {
	for _, raw := range []string{
		`{"kind":"github_pr_merged"}`,
		`{"literal":1,"pointer":"/run/input"}`,
		`{}`,
		`null`,
		`7`,
	} {
		var v InputBinding
		if err := json.Unmarshal([]byte(raw), &v); err == nil {
			t.Errorf("decoded %s as %+v, want a refusal", raw, v)
		}
	}
}
