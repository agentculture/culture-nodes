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
			if values["origin_marker"] != "signed-marker" {
				t.Fatal("marker not passed to bridge")
			}
		})
	}
}
