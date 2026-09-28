package api_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/decl/kinds"
)

// The migrated pr-upkeep and jira-intake declaration sets (tasks t31, t31b;
// examples/*/declarations/manifest.json). A declaration moves from its start
// node to its landing node; the next one starts on that landing node and
// triggers on the REACTION to the previous action. These tests hold both
// sets to that shape, not only to "it parses and publishes".

type exampleDeclarationLink struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
	Why  string `json:"why"`
}

type exampleDeclarationManifest struct {
	Declarations []string                 `json:"declarations"`
	Links        []exampleDeclarationLink `json:"links"`
	Entries      []struct {
		Declaration string `json:"declaration"`
	} `json:"entries"`
	Steps []struct {
		GraphWorkflow string   `json:"graph_workflow"`
		GraphNode     string   `json:"graph_node"`
		Declarations  []string `json:"declarations"`
	} `json:"steps"`
}

func readExampleDeclarations(t *testing.T, workflow string) (exampleDeclarationManifest, map[string]string, map[string]*decl.Declaration) {
	t.Helper()
	dir := filepath.Join("..", "..", "examples", workflow, "declarations")
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m exampleDeclarationManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	sources := make(map[string]string)
	parsed := make(map[string]*decl.Declaration)
	for _, file := range m.Declarations {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		d, err := decl.Parse(b, decl.FormatJSON)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if _, err := decl.ActionReferences(d.Action); err != nil {
			t.Fatalf("%s templates: %v", file, err)
		}
		if _, exists := sources[d.Name]; exists {
			t.Fatalf("duplicate declaration %s", d.Name)
		}
		sources[d.Name] = string(b)
		parsed[d.Name] = d
	}
	return m, sources, parsed
}

// Every graph node of the live workflows is covered by at least one
// declaration (a node reached from several predecessors becomes one
// declaration per predecessor, each with its own start node and trigger).
func TestExampleDeclarationsParseAndCoverNodes(t *testing.T) {
	for _, tc := range []struct {
		workflow string
		nodes    []string
	}{
		{"pr-upkeep", []string{"route", "intake-orphan", "stamp-pr", "stage-dispatch", "analyse", "fix", "stage-pr-open", "readiness", "human-merges-pr", "finish", "sweep", "swept", "sweep-failed"}},
		{"jira-intake", []string{"intake", "post-comment", "transition", "stage-intake", "picked-up"}},
	} {
		t.Run(tc.workflow, func(t *testing.T) {
			m, sources, _ := readExampleDeclarations(t, tc.workflow)
			covered := map[string][]string{}
			for _, step := range m.Steps {
				covered[step.GraphNode] = append(covered[step.GraphNode], step.Declarations...)
			}
			for _, node := range tc.nodes {
				if len(covered[node]) == 0 {
					t.Errorf("graph node %s has no declaration in manifest steps", node)
				}
				for _, name := range covered[node] {
					if sources[name] == "" {
						t.Errorf("graph node %s maps to %s, which is not a declaration file in the manifest", node, name)
					}
				}
			}
		})
	}
}

// universalReactions react to a node or to an action's technical result,
// not to the artifact the predecessor's action produced, so they follow any
// action (their consumes type is `node` / `action.result`).
func universalReaction(kind string) bool {
	return kind == "node.expired" || strings.HasPrefix(kind, "action.")
}

var lineageRef = regexp.MustCompile(`lineage\["([a-z0-9-]+)"\]`)

// TestExampleDeclarationsChain is the structural half of t31b: both sets
// are chains, not islands. For each set:
//
//   - every link endpoint is a declaration of the set, the kind is must or
//     can, and the manifest says why;
//   - an entry starts on the shared root node and has no predecessor; a
//     timer trigger is allowed only on an entry (a genuine schedule);
//   - every other declaration starts on the landing node of at least one
//     declaration it links from (its immediate predecessor), and triggers on
//     a reaction to that predecessor's action: a trigger kind whose
//     `consumes` includes something the action `produces`
//     (internal/decl/kinds), or node.expired / action.*;
//   - every non-entry declaration is reachable from an entry through
//     immediate predecessors;
//   - the CEL condition compiles against the engine's variables (event,
//     lineage), and every lineage["X"] it reads is guaranteed present: X is
//     reachable from the declaration through must links alone.
func TestExampleDeclarationsChain(t *testing.T) {
	env, err := cel.NewEnv(cel.Variable("event", cel.MapType(cel.StringType, cel.DynType)), cel.Variable("lineage", cel.MapType(cel.StringType, cel.DynType)))
	if err != nil {
		t.Fatal(err)
	}
	for _, workflow := range []string{"pr-upkeep", "jira-intake"} {
		t.Run(workflow, func(t *testing.T) {
			m, _, ds := readExampleDeclarations(t, workflow)
			entries := map[string]bool{}
			for _, e := range m.Entries {
				if ds[e.Declaration] == nil {
					t.Errorf("entry %s is not a declaration of the set", e.Declaration)
				}
				entries[e.Declaration] = true
			}
			if len(entries) == 0 {
				t.Errorf("manifest names no entry declaration")
			}
			preds := map[string][]exampleDeclarationLink{}
			for _, l := range m.Links {
				if ds[l.From] == nil || ds[l.To] == nil {
					t.Errorf("link %s -> %s: endpoint is not a declaration of the set", l.From, l.To)
					continue
				}
				if l.Kind != "must" && l.Kind != "can" {
					t.Errorf("link %s -> %s: kind %q", l.From, l.To, l.Kind)
				}
				if strings.TrimSpace(l.Why) == "" {
					t.Errorf("link %s -> %s (%s) does not say why", l.From, l.To, l.Kind)
				}
				preds[l.From] = append(preds[l.From], l)
			}
			names := make([]string, 0, len(ds))
			for name := range ds {
				names = append(names, name)
			}
			sort.Strings(names)
			immediate := map[string][]string{}
			for _, name := range names {
				d := ds[name]
				if entries[name] {
					if d.StartNode.Name != "root" {
						t.Errorf("entry %s starts on %q, want the shared root node", name, d.StartNode.Name)
					}
					if len(preds[name]) != 0 {
						t.Errorf("entry %s links from %d predecessors, want none", name, len(preds[name]))
					}
					continue
				}
				if d.Trigger.Kind == "timer" {
					t.Errorf("%s triggers on timer but is not an entry: a chained declaration triggers on the reaction to its predecessor's action", name)
				}
				trig, _ := kinds.Trigger(d.Trigger.Kind)
				for _, l := range preds[name] {
					p := ds[l.To]
					if p.LandingNode.Name != d.StartNode.Name {
						continue
					}
					immediate[name] = append(immediate[name], l.To)
					act, _ := kinds.Action(p.Action.Kind)
					if !universalReaction(d.Trigger.Kind) && !sharesArtifact(trig.Consumes, act.Produces) {
						t.Errorf("%s triggers on %s, which consumes %v; its predecessor %s's action %s produces %v: not a reaction to it",
							name, d.Trigger.Kind, trig.Consumes, p.Name, p.Action.Kind, act.Produces)
					}
				}
				if len(immediate[name]) == 0 {
					t.Errorf("%s starts on %q, which is not the landing node of any declaration it links from", name, d.StartNode.Name)
				}
			}
			for _, name := range names {
				if !entries[name] && !reachesEntry(name, immediate, entries, map[string]bool{}) {
					t.Errorf("%s is not reachable from an entry through immediate predecessors", name)
				}
			}
			for _, name := range names {
				d := ds[name]
				if _, iss := env.Compile(d.Condition); iss.Err() != nil {
					t.Errorf("%s condition %q: %v", name, d.Condition, iss.Err())
				}
				guaranteed := mustClosure(name, preds)
				for _, match := range lineageRef.FindAllStringSubmatch(d.Condition, -1) {
					if !guaranteed[match[1]] {
						t.Errorf("%s condition reads lineage[%q], which no chain of must links guarantees", name, match[1])
					}
				}
			}
		})
	}
}

func sharesArtifact(consumes, produces []kinds.ArtifactType) bool {
	for _, c := range consumes {
		for _, p := range produces {
			if c == p && c != kinds.ArtifactNone {
				return true
			}
		}
	}
	return false
}

func reachesEntry(name string, immediate map[string][]string, entries, seen map[string]bool) bool {
	if entries[name] {
		return true
	}
	if seen[name] {
		return false
	}
	seen[name] = true
	for _, p := range immediate[name] {
		if reachesEntry(p, immediate, entries, seen) {
			return true
		}
	}
	return false
}

func mustClosure(name string, preds map[string][]exampleDeclarationLink) map[string]bool {
	out := map[string]bool{}
	var walk func(string)
	walk = func(n string) {
		for _, l := range preds[n] {
			if l.Kind == "must" && !out[l.To] {
				out[l.To] = true
				walk(l.To)
			}
		}
	}
	walk(name)
	return out
}

// Both sets validate, publish and link through the API. After linking, each
// declaration is validated again so its reference and sensitivity warnings
// are the ones a linked publish reports (t30); they are logged, never fatal,
// and listed in each set's README.
func TestExampleDeclarationsValidatePublishAndLink(t *testing.T) {
	srv, _, token, _ := newDeclarationHumanFixture(t)
	for _, workflow := range []string{"pr-upkeep", "jira-intake"} {
		m, sources, _ := readExampleDeclarations(t, workflow)
		names := make([]string, 0, len(sources))
		for name := range sources {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			source := sources[name]
			var validation declarationValidationResp
			rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/validate", token, declarationSourceReq{Format: "json", Source: source}, &validation)
			if rr.Code != http.StatusOK || !validation.Valid {
				t.Fatalf("validate %s: %d %s", name, rr.Code, rr.Body.String())
			}
			rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations", token, declarationSourceReq{Format: "json", Source: source}, nil)
			if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
				t.Fatalf("publish %s: %d %s", name, rr.Code, rr.Body.String())
			}
		}
		for _, link := range m.Links {
			rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/"+link.From+"/links", token, map[string]string{"to": link.To, "kind": link.Kind}, nil)
			if rr.Code != http.StatusCreated {
				t.Fatalf("link %s -> %s: %d %s", link.From, link.To, rr.Code, rr.Body.String())
			}
		}
		for _, name := range names {
			var validation declarationValidationResp
			rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/validate", token, declarationSourceReq{Format: "json", Source: sources[name]}, &validation)
			if rr.Code != http.StatusOK || !validation.Valid {
				t.Fatalf("re-validate %s: %d %s", name, rr.Code, rr.Body.String())
			}
			for _, w := range validation.Warnings {
				t.Logf("linked-publish warning %s: %s", name, w)
			}
		}
	}
}
