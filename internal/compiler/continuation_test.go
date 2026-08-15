package compiler

import (
	"encoding/json"
	"strings"
	"testing"
)

// withContinuation splices a `continue:` block into the minimal fixture's
// `start` node.
//
// The anchor and the block are both indented to the fixture's real depth --
// node properties sit at six spaces, not four. Matching at four spaces still
// finds the line (it is a substring of the six-space one) but consumes only
// part of the indentation, so the spliced block lands one level out and YAML
// reads `continue` as a sibling NODE rather than a property of `start`. The
// compiler then correctly rejects a node with no kind, and the test looks like
// a continuation bug instead of an indentation one.
func withContinuation(t *testing.T, block string) string {
	t.Helper()
	const anchor = "      kind: agent"
	fixture := string(readFixture(t, "minimal.workflow.yaml"))
	if !strings.Contains(fixture, anchor) {
		t.Fatalf("fixture no longer contains %q -- update the anchor", anchor)
	}
	return strings.Replace(fixture, anchor, anchor+"\n"+block, 1)
}

func TestContinuationDeclarationCompilesIntoIRAndCEL(t *testing.T) {
	source := withContinuation(t, `      continue:
        while:
          - node.state == "incomplete"
          - budget.remaining_sessions > 0
        bounds:
          maxContinuations: 3
          maxWallClock: 2h
          maxSessions: 4
        onExhausted: needs_human`)
	// Exhaustion is a domain outcome, so it travels an edge like any other.
	source += "    - from: start.needs_human\n      to: finish\n"
	compiled, diags, err := Compile([]byte(source), FormatYAML)
	if err != nil || compiled == nil {
		t.Fatalf("continuation declaration did not compile: err=%v diagnostics=%+v", err, diags)
	}
	for _, path := range []string{"/spec/nodes/start/continue/while/0", "/spec/nodes/start/continue/while/1"} {
		if _, ok := compiled.Programs[path]; !ok {
			t.Errorf("missing CEL program %s; the engine evaluates the condition, so an "+
				"unc*ompiled expression would mean a model deciding whether to keep going", path)
		}
	}
	var ir map[string]any
	if err := json.Unmarshal(compiled.Normalized, &ir); err != nil {
		t.Fatal(err)
	}
	node := ir["spec"].(map[string]any)["nodes"].(map[string]any)["start"].(map[string]any)
	if node["continue"] == nil {
		t.Fatal("normalized IR dropped continue declaration")
	}
}

func TestMalformedContinuationDeclarationIsDiagnostic(t *testing.T) {
	source := withContinuation(t, `      continue:
        while: [1]
        bounds:
          maxContinuations: 0
        onExhausted: ''`)
	compiled, diags, err := Compile([]byte(source), FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	if compiled != nil {
		t.Fatal("malformed continue declaration compiled")
	}
	if len(diags) == 0 {
		t.Fatal("malformed continue declaration produced no diagnostic")
	}
}

// TestExhaustedOutcomeMustBeRouted pins the half of the contract that is easy
// to lose: the edge loop deliberately excuses `onExhausted` from needing a
// contract.outcomes declaration -- the ENGINE produces it when a bound is
// spent, not the actor -- and that excuse is what would otherwise let a
// workflow name an exhaustion outcome nothing routes. A run that then spends
// its budget arrives at the one state the whole declaration exists to handle
// and stops, which is #78's shape (finished work with nowhere to go) reached
// by a different road.
func TestExhaustedOutcomeMustBeRouted(t *testing.T) {
	source := withContinuation(t, `      continue:
        while:
          - node.state == "incomplete"
        bounds:
          maxContinuations: 3
        onExhausted: needs_human`)
	compiled, diags, err := Compile([]byte(source), FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	if compiled != nil {
		t.Fatal("an unrouted onExhausted outcome compiled; exhaustion would have nowhere to go")
	}
	found := false
	for _, d := range diags {
		if d.Code == CodeGraphExhaustedUnrouted {
			found = true
			if d.Path != "/spec/nodes/start/continue/onExhausted" {
				t.Errorf("diagnostic path = %q, want the declaration that is wrong", d.Path)
			}
		}
	}
	if !found {
		t.Errorf("no %s diagnostic; got %+v", CodeGraphExhaustedUnrouted, diags)
	}
}
