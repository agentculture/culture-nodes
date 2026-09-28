package declengine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
)

func blockedFile(t *testing.T, lane, file string) decl.Declaration {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", lane, "declarations", file+".json"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := decl.Parse(data, decl.FormatJSON)
	if err != nil {
		t.Fatalf("%s/%s: %v", lane, file, err)
	}
	return *d
}

func TestRealBlockedDecisionRoutes(t *testing.T) {
	for _, tc := range []struct{ lane, step, original string }{
		{"pr-upkeep", "stamp-pr", "stamp-pr"},
		{"pr-upkeep", "analyse", "analyse"},
		{"pr-upkeep", "fix", "fix"},
		{"jira-intake", "intake", "intake"},
	} {
		t.Run(tc.lane+"/"+tc.step, func(t *testing.T) {
			blocked := blockedFile(t, tc.lane, "blocked-"+tc.step)
			if blocked.Trigger.ReentryLimit != 1 {
				t.Fatalf("blocked route reentry limit = %d", blocked.Trigger.ReentryLimit)
			}
			var with struct {
				Outcomes []string `json:"outcomes"`
			}
			if err := json.Unmarshal(blocked.Action.With, &with); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(with.Outcomes, []string{"retry", "abandon", "acknowledged"}) {
				t.Fatalf("choices %v", with.Outcomes)
			}
			cw, _, err := workerEnvelope(DispatchRequest{Action: blocked.Action})
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(cw.IR.Spec.Nodes["action"].Outcomes, ","); got != "abandon,acknowledged,expired,retry" {
				t.Fatalf("task allowed outcomes %s", got)
			}
			prefix := tc.lane
			original := blockedFile(t, tc.lane, tc.original)
			base := []Ancestor{{FiringID: "blocked", CanonicalID: "blocked", DeclarationID: blocked.Name, Name: blocked.Name, Variables: map[string]any{"outcome": "blocked"}},
				{FiringID: "original", CanonicalID: "original", DeclarationID: original.Name, Name: original.Name, Variables: map[string]any{"id": "JIRA-7", "title": "Title", "description": "Description", "description_truncated": false, "status": "To Do", "severity": "major", "kind": "task", "details_url": "url"}}}
			if tc.lane == "pr-upkeep" {
				base = append(base, Ancestor{FiringID: "route", CanonicalID: "route", DeclarationID: "pr-upkeep-route", Name: "pr-upkeep-route", Variables: map[string]any{"source": "github_pr", "repository": "a/b", "number": "12", "head_sha": "abc", "work_item": "JIRA-7", "findings": "[]"}})
			}
			if tc.step == "fix" {
				base = append(base, Ancestor{FiringID: "analyse", CanonicalID: "analyse", DeclarationID: "pr-upkeep-analyse", Name: "pr-upkeep-analyse", Variables: map[string]any{"packages": "[1]"}})
			}
			for _, choice := range []string{"retry", "abandon", "acknowledged"} {
				file := choice + "-" + tc.step
				if choice == "acknowledged" {
					file = "acknowledge-" + tc.step
				}
				d := blockedFile(t, tc.lane, file)
				a := ActiveDeclaration{ID: d.Name, VersionID: d.Name + "-v1", Declaration: d}
				calls := []DispatchRequest{}
				m := &memoryBackend{ancestors: base}
				e := newTestEngine(t, m, dispatchFunc(func(_ context.Context, r DispatchRequest) (DispatchResult, error) {
					calls = append(calls, r)
					return DispatchResult{}, nil
				}))
				ev := Event{NamespaceID: "ns", ID: "decision-" + choice, Kind: "human.decision", Node: blocked.LandingNode.Name, Variables: map[string]any{"outcome": choice}}
				if err := e.evaluate(context.Background(), ev, a, "blocked", base); err != nil {
					t.Fatal(err)
				}
				if len(calls) != 1 {
					t.Fatalf("%s dispatched %d actions, evaluations %+v", choice, len(calls), m.steps)
				}
				if choice == "retry" {
					if d.Trigger.ReentryLimit != 1 {
						t.Fatalf("retry reentry limit = %d", d.Trigger.ReentryLimit)
					}
					if calls[0].Action.Kind != "agent.work" || d.LandingNode.Name != original.LandingNode.Name {
						t.Fatalf("retry action/landing: %+v", d)
					}
					var origWith, retryWith struct {
						Uses  string         `json:"uses"`
						Input map[string]any `json:"input"`
					}
					originalRendered, err := renderAction(original.Action, base[1].Variables, base[2:])
					if err != nil {
						t.Fatal(err)
					}
					_ = json.Unmarshal(originalRendered.With, &origWith)
					_ = json.Unmarshal(calls[0].Action.With, &retryWith)
					delete(retryWith.Input, "id") // retained for a second Jira retry's lineage
					if retryWith.Uses != origWith.Uses || !reflect.DeepEqual(retryWith.Input, origWith.Input) {
						t.Fatalf("retry actor/input differs: %q/%q, %v/%v", retryWith.Uses, origWith.Uses, retryWith.Input, origWith.Input)
					}
					if retryWith.Input["repository"] == "{pr-upkeep-route:repository}" || retryWith.Input["issue"] == "{jira-intake-intake:id}" {
						t.Fatal("retry kept unresolved lineage template")
					}
				} else if calls[0].Action.Kind != "code.run" || d.LandingNode.Name != prefix+"-"+file+"-done" {
					t.Fatalf("%s did not record and end", choice)
				}
			}
			for _, choice := range []string{"retry", "abandon", "acknowledged"} {
				data, err := os.ReadFile(filepath.Join("..", "..", "examples", "notify", "declarations", "human-decided-"+prefix+"-blocked-"+tc.step+".json"))
				if err != nil {
					t.Fatal(err)
				}
				n, err := decl.Parse(data, decl.FormatJSON)
				if err != nil {
					t.Fatal(err)
				}
				ok, err := matches(*n, Event{Kind: "human.decision", Node: blocked.LandingNode.Name, Variables: map[string]any{"outcome": choice}})
				if err != nil || !ok {
					t.Fatalf("notify did not match %s: %v", choice, err)
				}
			}
		})
	}
}
