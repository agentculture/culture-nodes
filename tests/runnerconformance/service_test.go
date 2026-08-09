package runnerconformance_test

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/runners/runnerservice"
	"github.com/agentculture/culture-nodes/tests/runnerconformance"
)

// The kit, run against the in-repo reference runner service over a fake
// runner.
//
// This file is the answer to "how do I know the runner conformance kit is not
// asserting something impossible?" — it stands up a correct
// api/runner-protocol implementation in-process and requires the whole suite
// to pass against it, on every `go test ./...`, with no container runtime and
// no flags. live_test.go then runs the identical kit against the identical
// service wrapping the real headspace-cli bridge.

const (
	referenceSecret   = "reference-runner-service-secret"
	referenceRevision = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	referenceDigest   = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
	referencePolicyD  = "sha256:6666666666666666666666666666666666666666666666666666666666666666"
)

// fakeRunner is a runners.Runner that executes nothing and says so honestly.
// It exists so the kit's own self-test needs no Docker: what is under test
// here is the protocol surface, not an execution engine.
type fakeRunner struct{}

func (fakeRunner) Execute(ctx context.Context, op runners.Operation) (runners.Result, error) {
	if op.Command.RequiresShell != nil && *op.Command.RequiresShell {
		return runners.Result{}, &runners.DispatchError{
			Kind:        runners.ErrorAuthOrPolicy,
			OperationID: op.OperationID,
			Detail:      "operation declares requires_shell; argv is executed directly, never through a shell",
			Err:         runners.ErrUnsupportedOperation,
		}
	}

	started := time.Now().UTC()
	if len(op.Command.Argv) > 0 && op.Command.Argv[0] == "sleep" {
		<-ctx.Done()
		finished := time.Now().UTC()
		return runners.Result{
			OperationID: op.OperationID,
			State:       runners.StateCancelled,
			Timing: runners.Timing{
				StartedAt:  started,
				FinishedAt: finished,
				DurationMs: int(finished.Sub(started).Milliseconds()),
			},
			Environment:  referenceEnvironment(op),
			Changes:      runners.Changes{Complete: false},
			Observations: unmeasured("the operation was cancelled before it exited"),
			Error:        &runners.ResultError{Kind: runners.ErrorCancellation, Retryable: false, Message: "cancelled"},
		}, nil
	}

	code := 0
	finished := time.Now().UTC()
	return runners.Result{
		OperationID: op.OperationID,
		State:       runners.StateCompleted,
		Exit:        &runners.Exit{Code: &code},
		Timing: runners.Timing{
			StartedAt:  started,
			FinishedAt: finished,
			DurationMs: int(finished.Sub(started).Milliseconds()),
		},
		Environment: referenceEnvironment(op),
		Changes:     runners.Changes{Complete: true, Paths: []string{}},
		Observations: runners.Observations{
			ExitStatus:    runners.Observation{Measured: true, Complete: true, Method: "fake_wait_status"},
			ChangedPaths:  runners.Observation{Measured: true, Complete: true, Method: "fake_workspace_diff"},
			Logs:          runners.Observation{Measured: true, Complete: true, Method: "fake_capture"},
			ResourceUsage: runners.Observation{Measured: false, Complete: false, Note: "the fake runner measures no resource usage"},
		},
	}, nil
}

func referenceEnvironment(op runners.Operation) runners.Environment {
	return runners.Environment{
		RunnerRevision: op.RunnerRevision,
		ImageDigest:    op.Execution.ImageDigest,
		PolicyDigest:   referencePolicyD,
	}
}

func unmeasured(note string) runners.Observations {
	obs := runners.Observation{Measured: false, Complete: false, Note: note}
	return runners.Observations{ExitStatus: obs, ChangedPaths: obs, Logs: obs, ResourceUsage: obs}
}

func referenceOperation() runners.Operation {
	return runners.Operation{
		OperationID:    "op-template",
		Runner:         "fake",
		RunnerRevision: referenceRevision,
		Execution:      runners.Execution{Kind: runners.ExecutionContainer, ImageDigest: referenceDigest},
		Command:        runners.Command{Argv: []string{"true"}},
		Policy: runners.Policy{
			TimeoutSeconds:     30,
			Network:            runners.NetworkNone,
			AllowedOutputPaths: []string{},
		},
		Evidence: runners.EvidenceRequest{CaptureExit: true, CaptureLogs: true},
	}
}

// newReferenceService stands up the in-repo runner service over a durable
// (file-backed) store, which is the posture a deployment runs in.
func newReferenceService(t *testing.T, runner runners.Runner) string {
	t.Helper()
	store, err := runnerservice.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	svc, err := runnerservice.New(runnerservice.Config{
		Runner:  runner,
		Store:   store,
		Secret:  referenceSecret,
		OnError: func(err error) { t.Logf("runner service diagnostic: %v", err) },
	})
	if err != nil {
		t.Fatalf("runnerservice.New: %v", err)
	}
	server := httptest.NewServer(svc.Handler())
	t.Cleanup(func() {
		server.Close()
		svc.Close()
	})
	return server.URL
}

func TestReferenceServicePassesTheRunnerConformanceKit(t *testing.T) {
	endpoint := newReferenceService(t, fakeRunner{})

	refused := referenceOperation()
	shell := true
	refused.Command.RequiresShell = &shell

	cancellable := referenceOperation()
	cancellable.Command.Argv = []string{"sleep", "600"}

	runnerconformance.Run(t, runnerconformance.Config{
		Endpoint:  endpoint,
		AuthToken: referenceSecret,

		Operation:            referenceOperation(),
		RefusedOperation:     &refused,
		CancellableOperation: &cancellable,

		TerminalWait:       20 * time.Second,
		RetentionReadDelay: 100 * time.Millisecond,
		CancelAfter:        100 * time.Millisecond,
	})
}

// negativeEnv makes the inner half of the negative self-check runnable.
const negativeEnv = "CULTURE_NODES_RUNNER_CONFORMANCE_NEGATIVE"

// TestKitDetectsANonConformingRunner proves the kit is not vacuous: a service
// whose secret the config does not know must FAIL it.
//
// The check runs in a subprocess because a failing subtest marks its parent
// failed, and there is no supported way to observe a *testing.T failure
// without propagating it — the same shape tests/conformance uses, and with
// the same bonus: what is asserted is the process exit status a runner author
// would actually see.
func TestKitDetectsANonConformingRunner(t *testing.T) {
	if os.Getenv(negativeEnv) != "" {
		t.Skip("this is the outer half of the negative self-check; the inner half is TestKitNegativeInner")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH, so the negative self-check cannot re-exec the test binary")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-run", "TestKitNegativeInner", ".")
	cmd.Env = append(os.Environ(), negativeEnv+"=1")
	output, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("the kit PASSED a service the config cannot authenticate to; it is not asserting anything\n%s", output)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("the negative self-check subprocess failed to run: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("operation-lifecycle")) {
		t.Fatalf("the subprocess failed for a reason other than a kit check:\n%s", output)
	}
	t.Logf("the kit correctly failed a non-conforming run (exit: %v)", err)
}

// TestKitNegativeInner is the inner half: the kit driven with a secret the
// service does not accept, so every authenticated call is refused and the
// lifecycle checks fail. It is EXPECTED TO FAIL, and only runs when its
// parent asks for it.
func TestKitNegativeInner(t *testing.T) {
	if os.Getenv(negativeEnv) == "" {
		t.Skip("inner half of the negative self-check; run by TestKitDetectsANonConformingRunner")
	}
	endpoint := newReferenceService(t, fakeRunner{})
	runnerconformance.Run(t, runnerconformance.Config{
		Endpoint:     endpoint,
		AuthToken:    "the-wrong-secret",
		Operation:    referenceOperation(),
		TerminalWait: 5 * time.Second,
	})
}
