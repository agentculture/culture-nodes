// Package deploytest holds manifest-as-Go-test checks over deploy/ that are
// cheaper to run as part of `go test ./...` than to stand up as a separate
// tool.
//
// This file is task t5's (codex-bridges): parse deploy/prod/compose.thor.yml
// and deploy/prod/compose.orin.yml, and assert both production worker
// environment blocks contain both NODES_ACTOR_CODEX_THOR_TOKEN and
// NODES_ACTOR_CODEX_ORIN_TOKEN keys (environment variables for codex actor
// endpoints on thor and orin machines).
package deploytest

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"sigs.k8s.io/yaml"
)

type prodComposeService struct {
	Image       string            `json:"image"`
	Environment map[string]string `json:"environment"`
}

type prodComposeFile struct {
	Services map[string]prodComposeService `json:"services"`
}

// TestCodexWorkerEnvInProdCompose asserts both production compose files'
// worker services carry environment variables for both codex actor tokens
// (thor and orin).
func TestCodexWorkerEnvInProdCompose(t *testing.T) {
	requiredEnvVars := []string{
		"NODES_ACTOR_CODEX_THOR_TOKEN",
		"NODES_ACTOR_CODEX_ORIN_TOKEN",
	}

	// Test both prod compose files: thor and orin
	composeFiles := []string{
		filepath.Join(prodComposeDir(t), "compose.thor.yml"),
		filepath.Join(prodComposeDir(t), "compose.orin.yml"),
	}

	for _, composeFile := range composeFiles {
		t.Run(filepath.Base(composeFile), func(t *testing.T) {
			raw, err := os.ReadFile(composeFile)
			if err != nil {
				t.Fatalf("read %s: %v", composeFile, err)
			}
			var doc prodComposeFile
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("parse %s: %v", composeFile, err)
			}

			worker, ok := doc.Services["worker"]
			if !ok {
				t.Fatalf("%s: expected a \"worker\" service, found none", composeFile)
			}

			if len(worker.Environment) == 0 {
				t.Fatalf("%s: worker service has no environment variables", composeFile)
			}

			for _, envVar := range requiredEnvVars {
				if _, exists := worker.Environment[envVar]; !exists {
					t.Errorf("%s: worker environment missing %q (required key not found)", composeFile, envVar)
				}
			}
		})
	}
}

// prodComposeDir locates deploy/prod from this test file's own path
// (tests/deploy/codexworkerenv_test.go -> tests/deploy -> tests -> repo root
// -> deploy/prod), using runtime.Caller(0) to stay independent of the working
// directory.
func prodComposeDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(1)
	if !ok {
		t.Fatal("runtime.Caller(1) failed; cannot locate the repo root to load prod compose files")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // tests/deploy -> tests -> repo root
	return filepath.Join(repoRoot, "deploy", "prod")
}
