package declengine

import (
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

func TestWorkerEnvelopeUsesExistingPaths(t *testing.T) {
	for _, kind := range []string{"agent.work", "github.comment", "code.run", "human.ask"} {
		t.Run(kind, func(t *testing.T) {
			with := json.RawMessage(`{"uses":"actor://test/worker@sha256:aaaaaa","input":{"text":"hello"}}`)
			if kind == "code.run" {
				with = json.RawMessage(`{"uses":"runner://headspace/docker@sha256:5555555555555555555555555555555555555555555555555555555555555555","operation":{"image":"python:3.12-slim@sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de","argv":["python3","-V"],"network":"none","allowedOutputPaths":[]}}`)
			}
			if kind == "human.ask" {
				with = json.RawMessage(`{"approver_ref":"group/reviewers","input":{"question":"continue?"}}`)
			}
			req := DispatchRequest{Firing: postgres.DeclarationFiring{ID: "f1", DeclarationID: "01KABCDEF01234567890123456", DeclarationVersion: "v1"}, Action: decl.Action{Kind: kind, With: with}, Declaration: active("A").Declaration, Marker: "signed-marker"}
			cw, input, err := workerEnvelope(req)
			if err != nil {
				t.Fatal(err)
			}
			if cw == nil {
				t.Fatal("missing compiled envelope")
			}
			var values map[string]any
			if err := json.Unmarshal(input, &values); err != nil {
				t.Fatal(err)
			}
			if values[MarkerInputKey] != "signed-marker" {
				t.Fatal("marker not passed to bridge")
			}
		})
	}
}

// The engine owns the marker key: an unmarked firing sends none (a bridge
// answers 400 to a present-but-malformed marker, and "" is malformed), and an
// author-written value never reaches the bridge in place of the minted one.
func TestWorkerEnvelopeOwnsTheMarkerKey(t *testing.T) {
	with := json.RawMessage(`{"uses":"actor://test/worker@sha256:aaaaaa","input":{"text":"hello","marker":"cn1:forged"}}`)
	for _, minted := range []string{"", "signed-marker"} {
		req := DispatchRequest{Firing: postgres.DeclarationFiring{ID: "f1", DeclarationID: "01KABCDEF01234567890123456"}, Action: decl.Action{Kind: "agent.work", With: with}, Marker: minted}
		_, input, err := workerEnvelope(req)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := json.Unmarshal(input, &values); err != nil {
			t.Fatal(err)
		}
		got, present := values[MarkerInputKey]
		if minted == "" && present {
			t.Fatalf("unmarked firing sent %q=%v", MarkerInputKey, got)
		}
		if minted != "" && got != minted {
			t.Fatalf("%q=%v, want the minted marker", MarkerInputKey, got)
		}
	}
}
