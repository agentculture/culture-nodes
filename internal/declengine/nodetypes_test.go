package declengine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Task t38d (d6): a delivered event's payload `node` is a claim, never its
// arrival node -- EventFromSignal always starts it at root.
func TestEventFromSignalAlwaysArrivesAtRoot(t *testing.T) {
	for _, payload := range []string{`{"node":"waiting"}`, `{"node":"waiting","origin":{"marker":"cn1:x"}}`, `{}`, `[1]`} {
		ev := EventFromSignal(postgres.SignalEvent{ID: "e", NamespaceID: "ns", Name: "pr-upkeep.pr", Payload: json.RawMessage(payload)})
		if ev.Node != RootNode {
			t.Errorf("payload %s: Node = %q, want %q", payload, ev.Node, RootNode)
		}
		if ev.arrival.opened {
			t.Errorf("payload %s: arrival set by EventFromSignal", payload)
		}
	}
	// t38g: the payload's node is only a variable; the emitter is the
	// signal row's, for Handle's reserved-name check.
	ev := EventFromSignal(postgres.SignalEvent{ID: "e", NamespaceID: "ns", Name: "pr-upkeep.pr", Emitter: "jira-webhook", Payload: json.RawMessage(`{"node":"waiting"}`)})
	if ev.Node != RootNode || ev.Variables["node"] != "waiting" || ev.Emitter != "jira-webhook" {
		t.Fatalf("EventFromSignal = node %q, variables %v, emitter %q; want root, the payload kept as variables, the row's emitter", ev.Node, ev.Variables, ev.Emitter)
	}
}

// start_from matching reads only the arrival Handle derived; the event's
// Node name and variables are irrelevant to it, and root never matches.
func TestClassifyStartFrom(t *testing.T) {
	codex := decl.Declaration{Trigger: decl.Trigger{Kind: "agent.result"}, StartNode: decl.Node{Name: "declared"},
		StartFrom: &decl.StartFrom{ActorKind: "codex"}}
	anyNode := codex
	anyNode.StartFrom = &decl.StartFrom{Any: true}
	arrivedCodex := Event{Kind: "agent.result", Node: "n", arrival: nodeArrival{opened: true, host: "thor", actorKind: "codex"},
		Variables: map[string]any{"actor_kind": "claude", "host": "orin"}}
	arrivedClaude := Event{Kind: "agent.result", Node: "n", arrival: nodeArrival{opened: true, actorKind: "claude"}}
	atRoot := Event{Kind: "agent.result", Node: RootNode, Variables: map[string]any{"actor_kind": "codex"}}
	atDeclared := Event{Kind: "agent.result", Node: "declared"}

	for _, tc := range []struct {
		name     string
		d        decl.Declaration
		ev       Event
		matched  bool
		missHint string
	}{
		{"codex node", codex, arrivedCodex, true, ""},
		{"claude node", codex, arrivedClaude, false, "actor_kind=codex"},
		{"root", codex, atRoot, false, "no firing opened"},
		{"declared name alone is not enough", codex, atDeclared, false, "no firing opened"},
		{"any at an opened node", anyNode, arrivedClaude, true, ""},
		{"any at root", anyNode, atRoot, false, "no firing opened"},
		{"other trigger kind is silent", codex, Event{Kind: "pr-upkeep.pr", Node: RootNode}, false, ""},
	} {
		matched, miss, err := classify(tc.d, tc.ev)
		if err != nil {
			t.Fatal(err)
		}
		if matched != tc.matched || (tc.missHint == "") != (miss == "") || !strings.Contains(miss, tc.missHint) {
			t.Errorf("%s: matched=%v miss=%q, want matched=%v miss containing %q", tc.name, matched, miss, tc.matched, tc.missHint)
		}
	}
	// Without start_from, a start-node mismatch stays silent, as before.
	plain := codex
	plain.StartFrom = nil
	if matched, miss, _ := classify(plain, arrivedCodex); matched || miss != "" {
		t.Fatalf("plain declaration at another node: matched=%v miss=%q, want a silent non-match", matched, miss)
	}
	if matched, _, _ := classify(plain, atDeclared); !matched {
		t.Fatal("plain declaration at its declared start node did not match")
	}
}
