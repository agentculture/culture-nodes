package declengine

import (
	"encoding/json"
	"strings"
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

// Task t38e: an agent.work node offers the outcomes its declaration's
// migrated contract declares, so the agent's own outcome is carried rather
// than collapsed to `completed`; with no contract it keeps `completed`, and
// a name that is not an outcome name is refused before anything compiles.
// #328 t32 (found live in 'after'): a bridge action reports its own domain
// outcome too -- the jira bridge answers create_issue with issue_created --
// so its node must offer the declaration's migrated contract outcomes, not a
// bare `completed` that turns a created ticket into contract_rejected.
func TestWorkerEnvelopeCarriesABridgeActionsContractOutcomes(t *testing.T) {
	req := DispatchRequest{Firing: postgres.DeclarationFiring{ID: "f1", DeclarationID: "01KABCDEF01234567890123456"}, Action: decl.Action{Kind: "jira.create",
		With: json.RawMessage(`{"uses":"actor://company/jira-comment@sha256:aaaaaa","input":{"verb":"create_issue"},"graph_config":{"contract":{"outcomes":{"issue_created":{"schema":{"type":"object","required":["issue","id"]}}}}}}`)}}
	cw, _, err := workerEnvelope(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := cw.IR.Spec.Nodes["action"].Outcomes; strings.Join(got, ",") != "issue_created" {
		t.Fatalf("jira.create node outcomes = %v, want the contract's [issue_created]", got)
	}
}

func TestWorkerEnvelopeCarriesTheAgentContractOutcomes(t *testing.T) {
	envelope := func(with string) ([]string, error) {
		req := DispatchRequest{Firing: postgres.DeclarationFiring{ID: "f1", DeclarationID: "01KABCDEF01234567890123456"}, Action: decl.Action{Kind: "agent.work", With: json.RawMessage(with)}}
		cw, _, err := workerEnvelope(req)
		if err != nil {
			return nil, err
		}
		return cw.IR.Spec.Nodes["action"].Outcomes, nil
	}
	got, err := envelope(`{"uses":"actor://test/worker@sha256:aaaaaa","graph_config":{"contract":{"outcomes":{"packaged":{"schema":{"type":"object","required":["packages"]}},"no_fix":{"schema":{"type":"object"}}}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "no_fix,packaged" {
		t.Fatalf("agent node outcomes = %v, want exactly the contract's [no_fix packaged]", got)
	}
	got, err = envelope(`{"uses":"actor://test/worker@sha256:aaaaaa"}`)
	if err != nil || strings.Join(got, ",") != "completed" {
		t.Fatalf("agent node without a contract: outcomes %v (err %v), want [completed]", got, err)
	}
	if _, err := envelope(`{"uses":"actor://test/worker@sha256:aaaaaa","graph_config":{"contract":{"outcomes":{"Bad Name":{}}}}}`); err == nil {
		t.Fatal("an outcome named \"Bad Name\" compiled")
	}
}
