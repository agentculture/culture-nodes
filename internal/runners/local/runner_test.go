package local

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/runners"
)

func operation(language, script string) runners.Operation {
	return runners.Operation{
		OperationID: "local-test", Runner: RunnerName, RunnerRevision: Revision,
		Execution: runners.Execution{Kind: runners.ExecutionContainer, ImageDigest: ImageDigest},
		Command:   runners.Command{Argv: []string{language, "-c", script}},
		Policy:    runners.Policy{TimeoutSeconds: 1, Network: runners.NetworkNone},
	}
}

func TestLanguagesAndTimeout(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap unavailable")
	}
	if err := exec.Command("bwrap", "--unshare-all", "--share-net", "--ro-bind", "/", "/", "--", "/usr/bin/true").Run(); err != nil {
		t.Skipf("bubblewrap namespace setup unavailable on this host: %v", err)
	}
	// This host forbids the network namespace's loopback setup. The wrapper
	// exercises all other isolation and process timeout paths in that case.
	bin := "bwrap"
	if err := exec.Command("bwrap", "--unshare-all", "--ro-bind", "/", "/", "--", "/usr/bin/true").Run(); err != nil {
		bin = filepath.Join(t.TempDir(), "bwrap-test")
		if err := os.WriteFile(bin, []byte("#!/bin/bash\nexec /usr/bin/bwrap \"$1\" \"$2\" --share-net \"${@:3}\"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ language, script string }{
		{"bash", "exit 0"}, {"python", "raise SystemExit(0)"}, {"js", "process.exit(0)"},
	} {
		t.Run(tc.language, func(t *testing.T) {
			r, err := New(Config{Bubblewrap: bin})
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.Execute(context.Background(), operation(tc.language, tc.script))
			if err != nil {
				t.Fatal(err)
			}
			if result.State != runners.StateCompleted {
				t.Fatalf("state = %s, want completed", result.State)
			}
		})
	}
	r, err := New(Config{Bubblewrap: bin})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	result, err := r.Execute(context.Background(), operation("bash", "sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	if result.State != runners.StateTimedOut {
		t.Fatalf("state = %s, want timed_out", result.State)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("timeout did not kill the script promptly")
	}
}

func TestRejectsUnregisteredExecution(t *testing.T) {
	r, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	op := operation("bash", "exit 0")
	op.Runner = "remote"
	_, err = r.Execute(context.Background(), op)
	var dispatch *runners.DispatchError
	if !errors.As(err, &dispatch) {
		t.Fatalf("want dispatch refusal, got %v", err)
	}
	op = operation("bash", "exit 0")
	op.Execution.ImageDigest = "sha256:unregistered"
	_, err = r.Execute(context.Background(), op)
	if !errors.As(err, &dispatch) {
		t.Fatalf("want image refusal, got %v", err)
	}
}

func TestProductionRunnerRequiresNetworkIsolation(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap unavailable")
	}
	r, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Execute(context.Background(), operation("bash", "exit 0"))
	if err != nil {
		var dispatch *runners.DispatchError
		if !errors.As(err, &dispatch) || dispatch.Kind != runners.ErrorRunnerUnavailable {
			t.Fatalf("unexpected refusal: %v", err)
		}
		return // host denied network namespace; production did not run unsandboxed
	}
	if result.State != runners.StateCompleted {
		t.Fatalf("state = %s", result.State)
	}
}
