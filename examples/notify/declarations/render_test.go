package declarations_test

// Every notification renders against the REAL shape of its trigger's event
// (task t48, #328, owner decision d21): a notification leads with what
// happened and links to where. The shapes come from the emitting code, never
// from a tidier hand-written fake -- this cycle a fake that was tidier than
// the real emitter hid four defects, one of them that action.* and
// node.expired events reached their declarations with no variables at all:
//
//   - pr-upkeep.pr and jira.issue.created: the Python sweep's own functions,
//     run over the recorded fixtures (the same oracle the Go/Python seam test
//     in internal/api uses);
//   - action.*, node.expired, human.requested: the exported declengine
//     helpers that build both the stored payload and the handled variables;
//   - the reactions (agent.result, human.decision, code.result): the payload
//     keys internal/declengine/reactions.go EmitReaction writes, spelled out
//     below because they are assembled from database rows.
//
// A lineage reference ({pr-upkeep-route:number}) resolves only where the
// engine would resolve it: a reaction whose verified origin continues the
// lineage that began at the route (or intake) firing. Events the engine emits
// without an origin (action.*, node.expired, human.requested) start a fresh
// lineage, so a template for them can use only its own event's variables.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/decl/template"
	"github.com/agentculture/culture-nodes/internal/declengine"
)

const (
	uiBase      = "https://nodes.culture.dev"
	jiraBrowse  = "https://agentculture.atlassian.net/browse/"
	sampleRepo  = "agentculture/culture-nodes"
	sampleFired = "01K6TCA48F0000000000000001"
)

// realShapesScript prints the pr-upkeep.pr fact and the jira.issue.created
// payload the sweep emits for the recorded fixtures: a raw GitHub pull
// listing through fetch_open_pulls, SonarCloud findings through the sweep's
// own prioritise/finding_package, the Jira search through jira_emissions.
const realShapesScript = `import json, sys
sys.path.insert(0, 'examples/pr-upkeep')
import sweep, pr_upkeep_emit as e, pr_upkeep_github as g, pr_upkeep_jira as j
raw = [{'number': 7, 'head': {'sha': '0123abc', 'ref': 'fix/parser'}, 'body': '',
        'title': 'Tidy the parser', 'html_url': 'https://github.com/` + sampleRepo + `/pull/7',
        'user': {'login': 'someone'}}]
pull = g.fetch_open_pulls(lambda url, token: raw, 'https://api.github.com', None, '` + sampleRepo + `')[0]
items = sweep.sonar_work_items(json.load(open('examples/pr-upkeep/fixtures/sonarcloud-issues.json')), pr=7)
fact = e.upkeep_pr_fact(pull, '` + sampleRepo + `', e.finding_package(sweep.prioritise(items)))
facts = j.jira_emissions(json.load(open('examples/pr-upkeep/fixtures/jira-search.json')),
                         site='agentculture.atlassian.net', project='SCRUM')
created = [f['payload'] for f in facts if f['name'] == 'jira.issue.created'][0]
print(json.dumps({'pr': fact, 'jira_created': created}))
`

func realShapes(t *testing.T) (pr, jiraCreated map[string]any) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is absent")
	}
	cmd := exec.Command(python, "-c", realShapesScript)
	cmd.Dir = filepath.Join("..", "..", "..")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sweep shape oracle: %v", err)
	}
	var shapes struct {
		PR          map[string]any `json:"pr"`
		JiraCreated map[string]any `json:"jira_created"`
	}
	if err := json.Unmarshal(out, &shapes); err != nil {
		t.Fatal(err)
	}
	return shapes.PR, shapes.JiraCreated
}

// firingVariables is what the engine records for a fired firing and hands
// its descendants as lineage variables (engine.go Lineage): the event's
// variables overlaid with the action's rendered input (every template value
// renders to a string) -- here the route's and intake's own input keys.
func firingVariables(event map[string]any, inputKeys ...string) map[string]any {
	vars := map[string]any{}
	for k, v := range event {
		vars[k] = v
	}
	for _, k := range inputKeys {
		if v, ok := event[k]; ok {
			if s, isString := v.(string); isString {
				vars[k] = s
			} else {
				raw, _ := json.Marshal(v)
				vars[k] = string(raw)
			}
		}
	}
	return vars
}

// reaction is the payload EmitReaction writes (reactions.go): node,
// firing_id, outcome and the origin, plus the kind's own keys.
func reaction(node, outcome string, extra map[string]any) map[string]any {
	vars := map[string]any{
		"node": node, "firing_id": sampleFired, "outcome": outcome,
		"origin": map[string]any{"marker": "cn1:" + sampleFired + ":agent_work:" + strings.Repeat("a", 48) + ":" + strings.Repeat("b", 64),
			"artifact_kind": "agent_work", "artifact_id": sampleFired},
	}
	for k, v := range extra {
		vars[k] = v
	}
	return vars
}

// eventFor is the trigger's variables and the lineage the engine would
// resolve for it, by trigger kind and start node.
func eventFor(t *testing.T, d decl.Declaration, pr, jiraCreated map[string]any) (map[string]any, map[string]map[string]any) {
	t.Helper()
	lineage := map[string]map[string]any{}
	start := d.StartNode.Name
	// Only a verified reaction continues a lineage, and only a start node
	// downstream of the route (or intake) firing has it as an ancestor.
	withOrigin := func() {
		switch {
		case strings.HasPrefix(start, "jira-intake-"):
			lineage["jira-intake-intake"] = firingVariables(jiraCreated, "id", "title", "description", "status", "details_url")
		case strings.HasPrefix(start, "pr-upkeep-") && start != "pr-upkeep-sweep-done":
			lineage["pr-upkeep-route"] = firingVariables(pr, "source", "repository", "number", "head_sha", "work_item", "findings")
		}
	}
	switch d.Trigger.Kind {
	case "pr-upkeep.pr":
		return pr, lineage
	case "jira.issue.created":
		return jiraCreated, lineage
	case "action.failed":
		return declengine.ActionResultVariables(declengine.ActionResultEvent{FiringID: sampleFired, NodeID: "node-1", NodeName: start,
			Trigger: "action.failed", Class: "execution", Status: "failed"}), lineage
	case "node.expired":
		return declengine.NodeExpiredVariables(declengine.ExpiredNode{NodeID: "node-1", NodeName: start, FiringID: sampleFired}), lineage
	case "human.requested":
		return declengine.HumanRequestedVariables(declengine.HumanRequestedEvent{RunID: sampleFired, TaskID: "task-1", NodeName: start}), lineage
	case "agent.result":
		withOrigin()
		return reaction(start, "blocked", map[string]any{"result": map[string]any{"summary": "stuck"}, "outcome_authority": "proposed"}), lineage
	case "human.decision":
		withOrigin()
		return reaction(start, "retry", map[string]any{"human_task_id": "task-1", "decision": map[string]any{"notes": "go"}}), lineage
	case "code.result":
		withOrigin()
		return reaction(start, "failed", map[string]any{"result": map[string]any{"exit_code": 1}}), lineage
	}
	t.Fatalf("%s: no real shape recorded for trigger %q; add its emitter here", d.Name, d.Trigger.Kind)
	return nil, nil
}

var unresolved = regexp.MustCompile(`\{[^{}]*\}`)

func TestEveryNotificationRendersFromItsTriggersRealShape(t *testing.T) {
	pr, jiraCreated := realShapes(t)
	prNumber := int(pr["number"].(float64))
	prLink := "(https://github.com/" + sampleRepo + "/pull/" + jsonText(prNumber) + ")"
	jiraLink := "(" + jiraBrowse + jiraCreated["id"].(string) + ")"
	runLink := "(" + uiBase + "/runs/" + sampleFired + ")"
	inboxLink := "(" + uiBase + "/inbox)"

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
	for _, file := range manifest.Declarations {
		t.Run(file, func(t *testing.T) {
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			d, err := decl.Parse(body, decl.FormatJSON)
			if err != nil {
				t.Fatal(err)
			}
			var with struct {
				Input struct {
					Title       string `json:"title"`
					Description string `json:"description"`
				} `json:"input"`
			}
			if err := json.Unmarshal(d.Action.With, &with); err != nil {
				t.Fatal(err)
			}
			current, lineage := eventFor(t, *d, pr, jiraCreated)
			referenced := map[string]bool{}
			render := func(source string) string {
				tpl, err := template.Parse(source)
				if err != nil {
					t.Fatal(err)
				}
				for _, ref := range tpl.References() {
					referenced[decl.ExposureEntry(ref)] = true
				}
				return tpl.Render(func(ref template.Reference) (string, bool) {
					vars := current
					if ref.Step != "0" {
						vars = lineage[ref.Step]
					}
					v, ok := vars[ref.Name]
					if !ok || v == nil {
						return "", false
					}
					switch v.(type) {
					case map[string]any, []any:
						t.Errorf("{%s:%s} renders a structured value; templates take flat values", ref.Step, ref.Name)
					}
					return jsonText(v), true
				})
			}
			title, description := render(with.Input.Title), render(with.Input.Description)
			for _, text := range []string{title, description} {
				if m := unresolved.FindString(text); m != "" {
					t.Fatalf("unresolved %s in %q: its trigger's real event does not carry it", m, text)
				}
			}
			if strings.TrimSpace(title) == "" || strings.HasPrefix(title, "cn1:") {
				t.Fatalf("title %q must say what happened", title)
			}
			first := strings.SplitN(description, "\n", 2)[0]
			var want []string
			switch d.Trigger.Kind {
			case "pr-upkeep.pr":
				want = []string{prLink}
			case "jira.issue.created":
				want = []string{jiraLink}
			case "action.failed", "code.result", "node.expired":
				want = []string{runLink}
			case "human.requested":
				want = []string{inboxLink, runLink}
			case "agent.result", "human.decision":
				want = []string{prLink}
				if strings.HasPrefix(d.StartNode.Name, "jira-intake-") {
					want = []string{jiraLink}
				}
				if d.Trigger.Kind == "agent.result" {
					want = append(want, inboxLink)
				}
			}
			for _, link := range want {
				if !strings.Contains(first, "]"+link) {
					t.Errorf("first description line %q lacks the markdown link ...]%s", first, link)
				}
			}
			// exposes is exactly the set of references rendered: complete,
			// so no value is blocked unlisted, and no stale entry asks an
			// owner to approve something the message never shows.
			var refs []string
			for r := range referenced {
				refs = append(refs, r)
			}
			sort.Strings(refs)
			if got := decl.NormalizeExposes(d.Exposes); strings.Join(got, ",") != strings.Join(refs, ",") {
				t.Errorf("exposes = %v, want exactly the rendered references %v", got, refs)
			}
		})
	}
}

// The pr-work-item message d21 names: the PR's number and title first, the
// first finding and the count in the description.
func TestPRWorkItemLeadsWithThePullRequest(t *testing.T) {
	body, err := os.ReadFile("pr-work-item.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Action struct {
			With struct {
				Input map[string]any `json:"input"`
			} `json:"with"`
		} `json:"action"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	title, _ := d.Action.With.Input["title"].(string)
	description, _ := d.Action.With.Input["description"].(string)
	if !strings.HasPrefix(title, "PR #{number}: ") || !strings.Contains(title, "title") {
		t.Fatalf("title = %q, want PR #{number}: {title}", title)
	}
	for _, v := range []string{"finding_title", "finding_count"} {
		if !strings.Contains(description, v) {
			t.Fatalf("description %q does not render %s", description, v)
		}
	}
}

func jsonText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}
