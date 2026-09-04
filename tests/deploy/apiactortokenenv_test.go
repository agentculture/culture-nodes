// Package deploytest (see codexworkerenv_test.go's doc comment for the
// package's purpose).
//
// This file is task t4's (engine-hardening, issue #19): cancelRun's
// PROPAGATE step (internal/api/cancelpropagate.go) now calls
// actors.Client.Cancel directly from the api service, resolving the
// actor's endpoint+credential through worker.DBRegistry -- the same
// registry the worker service already uses to dispatch. Before this task
// the api service's compose environment carried no actor credentials at
// all (probed live against the running production deployment), so a
// Cancel call could never authenticate.
package deploytest

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// loadProdComposeFile reads and parses one prod compose manifest into the
// prodComposeFile shape codexworkerenv_test.go already declares (same
// package, so reused rather than redefined).
func loadProdComposeFile(t *testing.T, path string) prodComposeFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc prodComposeFile
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

// requiredActorTokenEnvVars are the actor-credential keys every
// service that calls into an actor endpoint needs -- the same set
// codexworkerenv_test.go already requires of the worker service.
var requiredActorTokenEnvVars = []string{
	"NODES_ACTOR_CLAUDE_TOKEN",
	"NODES_ACTOR_CODEX_THOR_TOKEN",
	"NODES_ACTOR_CODEX_ORIN_TOKEN",
	"NODES_ACTOR_QWEN_THOR_TOKEN",
	"NODES_ACTOR_QWEN_ORIN_TOKEN",
	"NODES_ACTOR_PI_THOR_TOKEN",
	"NODES_ACTOR_PI_ORIN_TOKEN",
}

// TestActorCancelTokenEnvInThorAPIService asserts thor's api service
// (deploy/prod/compose.thor.yml) carries the same three NODES_ACTOR_*_TOKEN
// keys the worker service already carries -- without them, cancelRun's
// best-effort POST .../cancel to an actor endpoint that requires a bearer
// credential fails to authenticate every time.
func TestActorCancelTokenEnvInThorAPIService(t *testing.T) {
	composeFile := filepath.Join(prodComposeDir(t), "compose.thor.yml")
	doc := loadProdComposeFile(t, composeFile)

	api, ok := doc.Services["api"]
	if !ok {
		t.Fatalf("%s: expected an \"api\" service, found none", composeFile)
	}
	if len(api.Environment) == 0 {
		t.Fatalf("%s: api service has no environment variables", composeFile)
	}
	for _, envVar := range requiredActorTokenEnvVars {
		if _, exists := api.Environment[envVar]; !exists {
			t.Errorf("%s: api environment missing %q (required key not found)", composeFile, envVar)
		}
	}
}

// TestOrinProdComposeHasNoAPIService documents a deliberate deviation from
// this task's literal brief ("the api service env blocks in BOTH ... gain
// the same lines"): orin (deploy/prod/compose.orin.yml) runs no api service
// at all. Its own header comment is explicit -- "No local Postgres, no
// MinIO, no API: the control plane lives on thor." -- so there is no api
// environment block on orin for NODES_ACTOR_*_TOKEN to be added to. This
// test pins that architecture down as a fact the suite checks, rather than
// silently assuming a service that does not exist; it should fail (and
// prompt adding the same env block) the day orin's compose file grows an
// api service of its own.
func TestOrinProdComposeHasNoAPIService(t *testing.T) {
	composeFile := filepath.Join(prodComposeDir(t), "compose.orin.yml")
	doc := loadProdComposeFile(t, composeFile)

	if _, ok := doc.Services["api"]; ok {
		t.Fatalf("%s: an \"api\" service now exists; give it the same NODES_ACTOR_*_TOKEN keys thor's api service carries", composeFile)
	}
}
