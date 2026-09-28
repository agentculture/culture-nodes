package declarations_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
)

func TestNotificationManifestPlacementAndCoKind(t *testing.T) {
	var manifest struct {
		Declarations []string `json:"declarations"`
	}
	data, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Declarations) == 0 {
		t.Fatal("empty notification set")
	}
	reactors := map[string]map[string]bool{}
	for _, area := range []string{"pr-upkeep", "jira-intake"} {
		paths, err := filepath.Glob(filepath.Join("..", "..", area, "declarations", "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			if strings.HasSuffix(path, "manifest.json") {
				continue
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			d, err := decl.Parse(body, decl.FormatJSON)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if reactors[d.StartNode.Name] == nil {
				reactors[d.StartNode.Name] = map[string]bool{}
			}
			reactors[d.StartNode.Name][d.Trigger.Kind] = true
		}
	}
	markerVerified := map[string]bool{"agent.result": true, "code.result": true, "human.decision": true, "jira.comment": true, "jira.issue.transitioned": true, "jira.issue.created": true, "github.pr.created": true, "github.pr.approved": true}
	seen := map[string]bool{}
	// One notification per (trigger kind, start node): every active
	// declaration that matches an event fires on it (engine.go Handle), and
	// the engine emits action.* with no parent firing, so two notify
	// declarations on one landing node post the same failure twice, one of
	// them titled with the wrong step (task t48 gate: finish-done had four).
	landed := map[string]string{}
	for _, file := range manifest.Declarations {
		if seen[file] {
			t.Fatalf("duplicate %s", file)
		}
		seen[file] = true
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		d, err := decl.Parse(body, decl.FormatJSON)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		key := d.Trigger.Kind + " @ " + d.StartNode.Name
		if other, dup := landed[key]; dup {
			t.Fatalf("%s and %s both notify on %s: one event would post twice", other, file, key)
		}
		landed[key] = file
		if !strings.HasPrefix(d.Name, "notify-") || d.Action.Kind != "discord.post" || d.Trigger.ReentryLimit != 0 || d.LandingNode.Deadline != "5m" {
			t.Fatalf("%s: invalid notification envelope", file)
		}
		if d.Trigger.Kind == "node.expired" && strings.HasPrefix(d.StartNode.Name, "notify-") {
			t.Fatalf("%s: notification expiry loop", file)
		}
		if d.Trigger.Kind == "timer" {
			t.Fatalf("%s: routine sweep must stay silent", file)
		}
		if markerVerified[d.Trigger.Kind] && len(reactors[d.StartNode.Name]) > 0 && !reactors[d.StartNode.Name][d.Trigger.Kind] {
			t.Fatalf("%s: %s at %s would consume another reactor's node", file, d.Trigger.Kind, d.StartNode.Name)
		}
		var with struct {
			Input       map[string]any `json:"input"`
			GraphConfig struct {
				Contract struct {
					Outcomes map[string]any `json:"outcomes"`
				} `json:"contract"`
			} `json:"graph_config"`
		}
		if err := json.Unmarshal(d.Action.With, &with); err != nil {
			t.Fatal(err)
		}
		if _, ok := with.GraphConfig.Contract.Outcomes["sent"]; !ok {
			t.Fatalf("%s: missing sent", file)
		}
		if _, ok := with.GraphConfig.Contract.Outcomes["delivery_failed"]; !ok {
			t.Fatalf("%s: missing delivery_failed", file)
		}
		if with.Input["require_delivery"] != false {
			t.Fatalf("%s: requires delivery", file)
		}
		if limit := exposureCap(d.Name); len(d.Exposes) > limit {
			t.Fatalf("%s: %d exposed values, cap %d", file, len(d.Exposes), limit)
		}
	}
}

// exposureCap is how many variables one notification may expose.
//
// Task t48 (owner decision d21) raised it from 2 to 3: a message that leads
// with what happened AND links to where needs the thing's identity (a PR
// number, a Jira key), the place to build the link from (the repository) and
// one line of what it is (a title, a summary, a decision outcome). The owner
// accepted that on the basis that a recorded-org Discord destination (task
// t49, in progress) makes most of these exposures non-widening, so the
// per-variable approvals they cost are expected to fall away rather than pile
// up.
//
// notify-pr-work-item is the one named exception, at 5: d21's own example
// message for it is `PR #{number}: {title}` with `{finding_title}` and
// `{finding_count}` in the description, and the PR link needs {repository}.
// A cap raised for every declaration to fit it would loosen all the others.
func exposureCap(name string) int {
	if name == "notify-pr-work-item" {
		return 5
	}
	return 3
}
