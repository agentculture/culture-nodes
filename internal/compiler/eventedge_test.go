package compiler

import (
	"encoding/json"
	"testing"
)

// Compiler-level tests for `onEvent` edges (issue #43, parallel-tokens design
// §6.1/§8): the authoring surface for any-node event pickup.

func TestEventEdgesCompileCleanly(t *testing.T) {
	cw, diags := compileFixture(t, "event-pickup.workflow.yaml", FormatYAML)
	for _, d := range diags {
		t.Errorf("unexpected diagnostic: %s %s at %s: %s", d.Level, d.Code, d.Path, d.Message)
	}

	var ir IR
	if err := json.Unmarshal(cw.Normalized, &ir); err != nil {
		t.Fatalf("decode normalized IR: %v", err)
	}

	// An event edge carries its name and no node source; a node edge is
	// untouched. Both must survive normalization, because the IR is what the
	// engine materializes routes from.
	var events, nodeEdges int
	for _, e := range ir.Spec.Edges {
		if e.OnEvent == "" {
			nodeEdges++
			if e.FromNode == "" || e.FromOutcome == "" {
				t.Errorf("node edge %+v lost its decomposition", e)
			}
			continue
		}
		events++
		if e.From != "" || e.FromNode != "" || e.FromOutcome != "" {
			t.Errorf("event edge %+v carries a node source", e)
		}
		if e.To == "" {
			t.Errorf("event edge %+v has no target", e)
		}
	}
	if events != 2 || nodeEdges != 1 {
		t.Fatalf("normalized %d event edges and %d node edges, want 2 and 1", events, nodeEdges)
	}

	// Two edges naming one event IS the pickup split (design D9): the set
	// semantics live in the edge list, so nothing else has to know about it.
	same := 0
	for _, e := range ir.Spec.Edges {
		if e.OnEvent == "review-requested" {
			same++
		}
	}
	if same != 2 {
		t.Errorf("edges naming review-requested = %d, want 2 (the pickup split)", same)
	}
}

// A node that only an event can reach is reachable. Refusing it would refuse
// exactly the graphs this feature exists to allow: nothing routes into
// `notify` or `escalate` from the entry node, and both are legitimate.
func TestEventEdgeTargetsAreReachabilityRoots(t *testing.T) {
	_, diags := compileFixture(t, "event-pickup.workflow.yaml", FormatYAML)
	for _, d := range diags {
		if d.Code == CodeGraphNodeUnreachable {
			t.Errorf("event-edge target reported unreachable: %s", d.Message)
		}
	}
}

// An event edge's guard sees `event`, which no node-outcome guard ever
// populates. The compiler must type-check it, or the guard would only fail at
// run time — after publication, which is exactly what compile-time CEL
// checking exists to prevent.
func TestEventEdgeGuardMayReadTheEvent(t *testing.T) {
	_, diags := compileFixture(t, "event-pickup.workflow.yaml", FormatYAML)
	for _, d := range diags {
		if d.Code == CodeContractCELInvalid || d.Code == CodeContractCELNotBoolean {
			t.Errorf("event-scoped guard was refused: %s at %s", d.Message, d.Path)
		}
	}
}

func TestEventEdgeToEndNodeIsRefused(t *testing.T) {
	_, diags := compileFixture(t, "err-event-edge-to-end.workflow.yaml", FormatYAML)
	found := false
	for _, d := range diags {
		if d.Code == CodeGraphEventEdgeToEnd && d.Level == LevelError {
			found = true
		}
	}
	if !found {
		t.Fatalf("an event edge into an end node was accepted: %s", renderDiagnostics(diags))
	}
}

// The normalized bytes are what a run pins, so adding event edges to the
// model must not move a workflow that has none. This is the digest-stability
// guard: minimal.workflow.yaml compiled before and after must be identical
// bytes, and the assertion below is the readable half of that — no event
// edge, no onEvent key anywhere in the IR.
func TestWorkflowsWithoutEventEdgesCarryNoOnEventKey(t *testing.T) {
	cw, diags := compileFixture(t, "minimal.workflow.yaml", FormatYAML)
	for _, d := range diags {
		if d.Level == LevelError {
			t.Fatalf("minimal fixture stopped compiling: %s", d.Message)
		}
	}
	if containsKey(t, cw.Normalized, "onEvent") {
		t.Error("a workflow with no event edges emitted an onEvent key; every already-published digest would move")
	}
}

func containsKey(t *testing.T, raw []byte, key string) bool {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode IR: %v", err)
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch value := v.(type) {
		case map[string]any:
			for k, child := range value {
				if k == key || walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range value {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(doc)
}
