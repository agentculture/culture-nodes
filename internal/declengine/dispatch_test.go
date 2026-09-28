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

func TestDiscordPostDefaultsToRealBridgeOutcomes(t *testing.T) {
	for _, with := range []string{
		`{"uses":"actor://company/notify-discord@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","input":{"content":"notice","require_delivery":false}}`,
		`{"uses":"actor://company/notify-discord@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","input":{"content":"notice"},"graph_config":{"contract":{"outcomes":{"sent":{"schema":{"type":"object"}}}}}}`,
	} {
		req := DispatchRequest{Firing: postgres.DeclarationFiring{ID: "f1", DeclarationID: "01KABCDEF01234567890123456"}, Action: decl.Action{Kind: "discord.post", With: json.RawMessage(with)}}
		cw, _, err := workerEnvelope(req)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(cw.IR.Spec.Nodes["action"].Outcomes, ","); got != "delivery_failed,sent" {
			t.Fatalf("discord.post outcomes = %s, want delivery_failed,sent", got)
		}
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
	if strings.Join(got, ",") != "blocked,no_fix,packaged" {
		t.Fatalf("agent node outcomes = %v, want [blocked no_fix packaged]", got)
	}
	got, err = envelope(`{"uses":"actor://test/worker@sha256:aaaaaa"}`)
	if err != nil || strings.Join(got, ",") != "blocked,completed" {
		t.Fatalf("agent node without a contract: outcomes %v (err %v), want [blocked completed]", got, err)
	}
	if _, err := envelope(`{"uses":"actor://test/worker@sha256:aaaaaa","graph_config":{"contract":{"outcomes":{"Bad Name":{}}}}}`); err == nil {
		t.Fatal("an outcome named \"Bad Name\" compiled")
	}
	if _, err := envelope(`{"uses":"actor://test/worker@sha256:aaaaaa","graph_config":{"contract":{"outcomes":{"blocked":{"schema":{"type":"string"}}}}}}`); err == nil {
		t.Fatal("a redefined blocked outcome compiled")
	}
}

func TestConventionalOutcomesAndBlockedInstruction(t *testing.T) {
	for _, tc := range []struct{ kind, with, outcomes string }{
		{"agent.work", `{"uses":"actor://test/worker@sha256:aaaaaa","input":{"instruction":"Do the task"}}`, "blocked,completed"},
		{"code.run", `{"uses":"runner://headspace/docker@sha256:5555555555555555555555555555555555555555555555555555555555555555","operation":{"image":"python:3.12-slim@sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de","argv":["python3","-V"],"network":"none","allowedOutputPaths":[]}}`, "failed,passed"},
		{"github.comment", `{"uses":"actor://test/worker@sha256:aaaaaa"}`, "completed"},
	} {
		req := DispatchRequest{Firing: postgres.DeclarationFiring{ID: "f1", DeclarationID: "01KABCDEF01234567890123456"}, Action: decl.Action{Kind: tc.kind, With: json.RawMessage(tc.with)}}
		cw, input, err := workerEnvelope(req)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(cw.IR.Spec.Nodes["action"].Outcomes, ","); got != tc.outcomes {
			t.Errorf("%s: %s, want %s", tc.kind, got, tc.outcomes)
		}
		if tc.kind == "agent.work" {
			schema, err := json.Marshal(cw.IR.Spec.Nodes["action"].Contract.Outcomes["blocked"].Schema)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(schema), `"required":["reason"]`) || !strings.Contains(string(schema), `"minLength":1`) || strings.Contains(string(schema), `"additionalProperties":false`) {
				t.Fatalf("blocked schema = %s, want a nonempty reason and extensible output", schema)
			}
			var value map[string]string
			if err := json.Unmarshal(input, &value); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(value["instruction"], `"outcome":"blocked"`) || !strings.Contains(value["instruction"], "missing credentials") {
				t.Fatal("blocked answer was not taught to the agent")
			}
		}
	}
}

func TestBlockedHumanAskWeekDeadlineCompiles(t *testing.T) {
	req := DispatchRequest{Firing: postgres.DeclarationFiring{ID: "f1", DeclarationID: "01KABCDEF01234567890123456"}, Action: decl.Action{Kind: "human.ask", With: json.RawMessage(`{"approver_ref":"group/platform-maintainers","timeout":"168h","input":{"agent_report":"blocked"}}`)}}
	cw, _, err := workerEnvelope(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := cw.IR.Spec.Nodes["action"].Policy.Timeout; got != "168h" {
		t.Fatalf("human deadline = %q, want 168h", got)
	}
}

func TestHumanAskDeclaredAndDefaultOutcomes(t *testing.T) {
	for _, tc := range []struct{ with, want string }{
		{`{"approver_ref":"group/reviewers","outcomes":["retry","abandon","acknowledged"]}`, "abandon,acknowledged,expired,retry"},
		{`{"approver_ref":"group/reviewers"}`, "approved,expired,rejected"},
	} {
		req := DispatchRequest{Firing: postgres.DeclarationFiring{ID: "f1", DeclarationID: "01KABCDEF01234567890123456"}, Action: decl.Action{Kind: "human.ask", With: json.RawMessage(tc.with)}}
		cw, _, err := workerEnvelope(req)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(cw.IR.Spec.Nodes["action"].Outcomes, ","); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.with, got, tc.want)
		}
	}
}
