package runnerconformance_test

import (
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/runners/headspace"
	"github.com/agentculture/culture-nodes/internal/runners/runnerservice"
	"github.com/agentculture/culture-nodes/tests/runnerconformance"
)

// The kit against the deployment the protocol document actually describes:
// the runner service wrapping the real headspace-cli bridge, executing real
// containers through a real Docker engine.
//
// It is the same Run(cfg) call service_test.go makes against the fake runner.
// That is the point of a conformance kit — the protocol surface is identical
// whether the thing behind it runs containers or nothing at all, and the only
// way to know the wrapped deployment is conformant is to run the same checks
// against it.
//
// Not gated behind a build tag: on a machine with headspace-cli and Docker,
// `go test ./...` runs it for real. Everywhere else it skips.

// realHeadspaceImageDigest is the pinned digest headspace-cli's python3.12
// profile resolves to, mirroring internal/runners/headspace/live_test.go's
// constant of the same name. It is only ever a map key into
// BridgeConfig.Profile — never sent to headspace-cli — so a drift in the real
// image's digest cannot break this test, only the comment's currency.
const realHeadspaceImageDigest = "sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de"

func requireRealHeadspace(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("headspace"); err != nil {
		t.Skip("skipping: no `headspace` binary on PATH")
	}
	out, err := exec.Command("headspace", "doctor", "--json").Output()
	if err != nil {
		t.Skipf("skipping: `headspace doctor --json` failed: %v", err)
	}
	var doctor struct {
		Healthy bool `json:"healthy"`
	}
	if jsonErr := json.Unmarshal(out, &doctor); jsonErr != nil || !doctor.Healthy {
		t.Skipf("skipping: `headspace doctor --json` reported unhealthy: %s", out)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("skipping: no `docker` binary on PATH")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("skipping: docker engine not reachable (`docker info` failed)")
	}
}

func liveOperation(revision string, argv []string, timeoutSeconds int) runners.Operation {
	memory := 256
	captureUsage := true
	return runners.Operation{
		OperationID:    "op-live-template",
		Runner:         headspace.RunnerName,
		RunnerRevision: revision,
		Execution: runners.Execution{
			Kind:        runners.ExecutionContainer,
			ImageDigest: realHeadspaceImageDigest,
		},
		Command: runners.Command{Argv: argv},
		Policy: runners.Policy{
			TimeoutSeconds:     timeoutSeconds,
			Network:            runners.NetworkNone,
			MemoryMiB:          &memory,
			AllowedOutputPaths: []string{},
		},
		Evidence: runners.EvidenceRequest{
			CaptureExit:          true,
			CaptureLogs:          true,
			CaptureResourceUsage: &captureUsage,
		},
	}
}

func TestHeadspaceBackedServicePassesTheRunnerConformanceKit(t *testing.T) {
	requireRealHeadspace(t)

	bridge, err := headspace.New(headspace.BridgeConfig{
		Profile:     map[string]string{realHeadspaceImageDigest: headspace.DefaultProfilePython312},
		Provider:    "docker",
		StopTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("headspace.New: %v", err)
	}

	store, err := runnerservice.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	svc, err := runnerservice.New(runnerservice.Config{
		Runner:  bridge,
		Store:   store,
		Secret:  "live-headspace-runner-secret",
		OnError: func(err error) { t.Logf("runner service diagnostic: %v", err) },
	})
	if err != nil {
		t.Fatalf("runnerservice.New: %v", err)
	}
	server := httptest.NewServer(svc.Handler())
	defer func() {
		server.Close()
		svc.Close()
	}()

	revision := bridge.RunnerRevision()

	// A shell is exactly the boundary this protocol exists to hold: the
	// bridge refuses requires_shell before it launches a single subprocess.
	refused := liveOperation(revision, []string{"python3", "-c", "print('never runs')"}, 60)
	shell := true
	refused.Command.RequiresShell = &shell

	cancellable := liveOperation(revision, []string{"python3", "-c", "import time; time.sleep(300)"}, 600)

	t.Logf("running the runner conformance kit against a headspace-backed service at %s", server.URL)
	runnerconformance.Run(t, runnerconformance.Config{
		Endpoint:  server.URL,
		AuthToken: "live-headspace-runner-secret",

		Operation:            liveOperation(revision, []string{"python3", "-c", "print('hello from the runner service')"}, 60),
		RefusedOperation:     &refused,
		CancellableOperation: &cancellable,

		Timeout:            60 * time.Second,
		TerminalWait:       5 * time.Minute,
		PollInterval:       time.Second,
		RetentionReadDelay: time.Second,
		// Long enough that the cancel lands against a container that is
		// genuinely running, not against a workspace still being created.
		CancelAfter: 15 * time.Second,
	})
}
