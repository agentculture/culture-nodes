// Package deploytest holds manifest-as-Go-test checks over deploy/ that are
// cheaper to run as part of `go test ./...` than to stand up as a separate
// tool — the same rationale tests/lint/awsisolation_test.go documents for
// its own AWS-SDK-isolation lint.
//
// This file is task t23's (h4/h7): parse deploy/compose/docker-compose.yml
// and assert the properties README.md and docker-compose.yml's own
// comments promise in prose — no Docker socket in any service, only the
// two stateful backing services (postgres, minio) hold volumes, the four
// control-plane role containers (migrate/api/scheduler/worker) all run the
// one culture-nodes image, and worker carries no elevated container
// privileges. A doc that says "no service mounts a Docker socket" is a
// claim; this test is the evidence.
//
// t22 (deploy/helm) is expected to add its own manifest test to this same
// package (helm_test.go, package deploytest) — this file defines no
// exported symbols and shares none with it, so the two can land in either
// order without colliding.
package deploytest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// composeService is the subset of the Compose Specification's service
// shape this test cares about. Every field this repo's docker-compose.yml
// actually uses stays typed (volumes and environment are both written in
// their short/map forms only, never the long form), so a service that
// drifted onto a shape this struct cannot decode would fail loudly here
// rather than silently parsing into a zero value.
type composeService struct {
	Image       string            `json:"image"`
	Volumes     []string          `json:"volumes"`
	Environment map[string]string `json:"environment"`
	Privileged  bool              `json:"privileged"`
	CapAdd      []string          `json:"cap_add"`
}

type composeFile struct {
	Services map[string]composeService `json:"services"`
}

// controlPlaneImage is the one image migrate/api/scheduler/worker must all
// run — see docker-compose.yml's header comment: "one role per container",
// the same binary under four commands.
const controlPlaneImage = "culture-nodes:local"

// controlPlaneServices are the four role containers built from this repo's
// own Dockerfile. colleague-bridge is deliberately excluded — it is an
// EXAMPLE EXTERNAL agent host built from its own, separate Dockerfile
// (adapters/colleague/Dockerfile) and image, never the control-plane image
// (README.md's "colleague-bridge (agents profile)" section).
var controlPlaneServices = []string{"migrate", "api", "scheduler", "worker"}

// statefulServices are the only services this profile expects to hold a
// volume: the two backing stores. Every control-plane role container is
// stateless by design (state lives in Postgres/MinIO, never on a
// container's own filesystem), and colleague-bridge's repo mount (if any)
// is an operator's own `docker run` deployment concern, not part of this
// reference compose file.
var statefulServices = map[string]bool{"postgres": true, "minio": true}

func loadCompose(t *testing.T) composeFile {
	t.Helper()
	path := composeFilePath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc composeFile
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(doc.Services) == 0 {
		t.Fatalf("%s declared no services", path)
	}
	return doc
}

// composeFilePath locates deploy/compose/docker-compose.yml from this test
// file's own path (tests/deploy/compose_test.go -> tests/deploy -> tests ->
// repo root -> deploy/compose/docker-compose.yml), the same
// runtime.Caller(0) technique tests/lint/awsisolation_test.go and
// tests/parity/parity_test.go both use to stay independent of the working
// directory a test runner invokes `go test` from.
func composeFilePath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repo root to load docker-compose.yml")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // tests/deploy -> tests -> repo root
	return filepath.Join(repoRoot, "deploy", "compose", "docker-compose.yml")
}

// TestNoServiceMountsTheDockerSocket is h4's manifest test: no service's
// volumes reference docker.sock, anywhere in the file. This is the
// strongest and simplest form of the c4 guarantee — a docker.sock bind
// mount is always a literal substring of a volumes entry
// ("/var/run/docker.sock:/var/run/docker.sock" or a named-pipe equivalent),
// so a substring scan over every declared volume, on every service
// (not just the control-plane ones), catches it regardless of which
// service it might have been added to.
func TestNoServiceMountsTheDockerSocket(t *testing.T) {
	doc := loadCompose(t)

	scanned := 0
	for name, svc := range doc.Services {
		for _, v := range svc.Volumes {
			scanned++
			if strings.Contains(v, "docker.sock") {
				t.Errorf("service %q mounts a Docker socket via volume %q; no compose service may ever do this (spec claim c4)", name, v)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no volume entries were scanned across any service; this test is not proving anything")
	}
}

// TestOnlyBackingStoresHaveVolumes is the complementary half of h4/h7's
// "complete local system, no socket in control-plane containers": not just
// that no volume names a Docker socket, but that only postgres and minio
// (the two stateful backing services) declare a volumes: key at all. Every
// control-plane role container, and the colleague-bridge example, must
// stay stateless in this reference file.
func TestOnlyBackingStoresHaveVolumes(t *testing.T) {
	doc := loadCompose(t)

	for name, svc := range doc.Services {
		hasVolumes := len(svc.Volumes) > 0
		wantVolumes := statefulServices[name]
		if hasVolumes && !wantVolumes {
			t.Errorf("service %q declares volumes %v, but only %v are expected to hold any volume in this profile",
				name, svc.Volumes, sortedKeys(statefulServices))
		}
		if wantVolumes && !hasVolumes {
			t.Errorf("service %q is expected to declare a volume (it is a stateful backing service) but declares none", name)
		}
	}
}

// TestControlPlaneServicesShareOneImage asserts migrate, api, scheduler,
// and worker all run the same culture-nodes image — the "one role per
// container" property docker-compose.yml's header comment and README.md's
// service-map table both describe, and the thing that makes "every role
// process is exactly what the Dockerfile built, under a different command"
// a checked fact instead of an assertion in prose.
func TestControlPlaneServicesShareOneImage(t *testing.T) {
	doc := loadCompose(t)

	for _, name := range controlPlaneServices {
		svc, ok := doc.Services[name]
		if !ok {
			t.Errorf("expected a %q service in docker-compose.yml, found none", name)
			continue
		}
		if svc.Image != controlPlaneImage {
			t.Errorf("service %q has image %q, want %q", name, svc.Image, controlPlaneImage)
		}
	}
}

// TestWorkerHasNoElevatedPrivileges is h4's other half for the worker
// specifically: the role container that dispatches to actors must not run
// privileged or hold any added Linux capability. Combined with
// TestNoServiceMountsTheDockerSocket, this rules out both the direct route
// to host/container escape (a mounted socket) and the indirect one
// (container-breakout via privileged/cap_add) for the one control-plane
// process that talks to the outside world beyond the API's own HTTP port.
func TestWorkerHasNoElevatedPrivileges(t *testing.T) {
	doc := loadCompose(t)

	worker, ok := doc.Services["worker"]
	if !ok {
		t.Fatal("expected a \"worker\" service in docker-compose.yml, found none")
	}
	if worker.Privileged {
		t.Error("worker service is privileged: true; a control-plane container must never be")
	}
	if len(worker.CapAdd) > 0 {
		t.Errorf("worker service adds capabilities %v; a control-plane container must add none", worker.CapAdd)
	}
}

// TestNoServiceIsPrivilegedOrAddsCapabilities extends the worker-specific
// check above to every service this file declares, including
// colleague-bridge and the two backing stores: nothing in this reference
// profile needs privileged mode or an added capability, so nothing should
// carry one.
func TestNoServiceIsPrivilegedOrAddsCapabilities(t *testing.T) {
	doc := loadCompose(t)

	for name, svc := range doc.Services {
		if svc.Privileged {
			t.Errorf("service %q is privileged: true; no service in this profile should be", name)
		}
		if len(svc.CapAdd) > 0 {
			t.Errorf("service %q adds capabilities %v; no service in this profile should add any", name, svc.CapAdd)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Small, fixed set (2 entries today) -- a manual insertion sort keeps
	// this file dependency-free rather than importing "sort" for two items.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
