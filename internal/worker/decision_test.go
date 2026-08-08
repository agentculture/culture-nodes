package worker

import (
	"encoding/json"
	"strings"
	"testing"
)

func decisionNode(ports ...selectPort) *nodeSpec {
	return &nodeSpec{ID: "route", Kind: kindDecision, Select: ports}
}

func TestDecisionFirstMatchWins(t *testing.T) {
	cache, err := newDecisionCache()
	if err != nil {
		t.Fatalf("newDecisionCache: %v", err)
	}
	node := decisionNode(
		selectPort{Outcome: "ship", When: "output.score >= 0.8"},
		selectPort{Outcome: "review", When: "output.score >= 0.5"},
		selectPort{Outcome: "hold", When: "true"},
	)

	for _, tc := range []struct {
		score float64
		want  string
	}{
		{0.95, "ship"},
		{0.80, "ship"},
		{0.60, "review"},
		{0.10, "hold"},
	} {
		nodeInput := json.RawMessage(`{"score":` + jsonNumber(tc.score) + `}`)
		outcome, matched, err := cache.evaluateDecision("sha256:test", node, json.RawMessage(`{}`), nodeInput)
		if err != nil {
			t.Fatalf("score %v: %v", tc.score, err)
		}
		if !matched {
			t.Fatalf("score %v matched no port", tc.score)
		}
		if outcome != tc.want {
			t.Errorf("score %v selected %q, want %q", tc.score, outcome, tc.want)
		}
	}
}

// The run input is available as `input`, exactly as in an edge guard, so an
// author does not have to learn a second variable convention.
func TestDecisionSeesRunInputAsInput(t *testing.T) {
	cache, err := newDecisionCache()
	if err != nil {
		t.Fatalf("newDecisionCache: %v", err)
	}
	node := decisionNode(
		selectPort{Outcome: "fast", When: `input.mode == "fast"`},
		selectPort{Outcome: "normal", When: "true"},
	)

	outcome, _, err := cache.evaluateDecision("sha256:test", node,
		json.RawMessage(`{"mode":"fast"}`), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if outcome != "fast" {
		t.Errorf("selected %q, want fast", outcome)
	}
}

// A node whose ports all evaluate false has no answer the workflow declared.
// That is a diagnosed gap, not a default: producing an outcome nobody
// declared would be the engine deciding a domain question.
func TestDecisionWithNoMatchIsNotADefault(t *testing.T) {
	cache, err := newDecisionCache()
	if err != nil {
		t.Fatalf("newDecisionCache: %v", err)
	}
	node := decisionNode(selectPort{Outcome: "ship", When: "output.score >= 0.8"})

	outcome, matched, err := cache.evaluateDecision("sha256:test", node,
		json.RawMessage(`{}`), json.RawMessage(`{"score":0.1}`))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if matched {
		t.Fatalf("an unmatched decision selected %q", outcome)
	}
}

// A predicate that errors has not said "false", it has said nothing.
// Swallowing it would silently route past a broken port.
func TestDecisionPredicateErrorIsNotFalse(t *testing.T) {
	cache, err := newDecisionCache()
	if err != nil {
		t.Fatalf("newDecisionCache: %v", err)
	}
	node := decisionNode(
		selectPort{Outcome: "ship", When: "output.absent.deeper == 1"},
		selectPort{Outcome: "hold", When: "true"},
	)

	_, _, err = cache.evaluateDecision("sha256:test", node, json.RawMessage(`{}`), json.RawMessage(`{"score":1}`))
	if err == nil {
		t.Fatal("a predicate that could not evaluate was treated as false and routing continued")
	}
	if !strings.Contains(err.Error(), "did not evaluate") {
		t.Errorf("error %q does not say the predicate failed to evaluate", err)
	}
}

func TestDecisionWithNoPortsIsAnError(t *testing.T) {
	cache, err := newDecisionCache()
	if err != nil {
		t.Fatalf("newDecisionCache: %v", err)
	}
	_, _, err = cache.evaluateDecision("sha256:test", decisionNode(), json.RawMessage(`{}`), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("a decision node with no select ports evaluated without complaint")
	}
}

// A null or absent payload becomes an empty map, so a predicate reading a
// member of it gets a clean "no such key" rather than a null dereference.
func TestDecisionHandlesNullPayloads(t *testing.T) {
	cache, err := newDecisionCache()
	if err != nil {
		t.Fatalf("newDecisionCache: %v", err)
	}
	node := decisionNode(selectPort{Outcome: "hold", When: `has(output.score) == false`})

	outcome, matched, err := cache.evaluateDecision("sha256:test", node, nil, json.RawMessage(`null`))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !matched || outcome != "hold" {
		t.Errorf("selected (%q, %v), want (hold, true)", outcome, matched)
	}
}

func jsonNumber(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
