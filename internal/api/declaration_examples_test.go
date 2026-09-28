package api_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
)

type exampleDeclarationManifest struct {
	Declarations []string `json:"declarations"`
	Links        []struct {
		From string `json:"from"`
		To   string `json:"to"`
		Kind string `json:"kind"`
	} `json:"links"`
}

func readExampleDeclarations(t *testing.T, workflow string) (exampleDeclarationManifest, map[string]string) {
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
	}
	return m, sources
}

func TestExampleDeclarationsParseAndCoverNodes(t *testing.T) {
	for _, tc := range []struct {
		workflow string
		names    []string
	}{
		{"pr-upkeep", []string{"route", "intake-orphan", "stamp-pr", "stage-dispatch", "analyse", "fix", "stage-pr-open", "readiness", "human-merges-pr", "finish", "sweep", "swept", "sweep-failed"}},
		{"jira-intake", []string{"intake", "post-comment", "transition", "stage-intake", "picked-up"}},
	} {
		t.Run(tc.workflow, func(t *testing.T) {
			_, sources := readExampleDeclarations(t, tc.workflow)
			if len(sources) != len(tc.names) {
				t.Fatalf("got %d declarations, want %d", len(sources), len(tc.names))
			}
			for _, node := range tc.names {
				name := tc.workflow + "-" + node
				if sources[name] == "" {
					t.Errorf("missing node declaration %s", name)
				}
			}
		})
	}
}

func TestExampleDeclarationsValidatePublishAndLink(t *testing.T) {
	srv, _, token, _ := newDeclarationHumanFixture(t)
	for _, workflow := range []string{"pr-upkeep", "jira-intake"} {
		m, sources := readExampleDeclarations(t, workflow)
		for name, source := range sources {
			var validation declarationValidationResp
			rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/validate", token, declarationSourceReq{Format: "json", Source: source}, &validation)
			if rr.Code != http.StatusOK || !validation.Valid {
				t.Fatalf("validate %s: %d %s", name, rr.Code, rr.Body.String())
			}
			rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations", token, declarationSourceReq{Format: "json", Source: source}, nil)
			if rr.Code != http.StatusCreated {
				t.Fatalf("publish %s: %d %s", name, rr.Code, rr.Body.String())
			}
		}
		for _, link := range m.Links {
			rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/"+link.From+"/links", token, map[string]string{"to": link.To, "kind": link.Kind}, nil)
			if rr.Code != http.StatusCreated {
				t.Fatalf("link %s -> %s: %d %s", link.From, link.To, rr.Code, rr.Body.String())
			}
		}
	}
}
