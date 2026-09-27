// Package local implements the server-host code runner. It has no database or
// control-plane dependency; each operation executes in a bubblewrap process.
package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/runners"
)

const RunnerName = "local"

var (
	Revision    = contracts.Digest([]byte("culture-nodes/internal/runners/local@v1"))
	ImageDigest = contracts.Digest([]byte("culture-nodes/local/bash-python-js@v1"))
)

type Config struct{ Bubblewrap string }
type Runner struct{ bwrap string }

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 4096 {
		_, _ = b.Buffer.Write(p[:min(len(p), 4096-b.Len())])
	}
	return n, nil
}

var _ runners.Runner = (*Runner)(nil)

func New(cfg Config) (*Runner, error) {
	bin := cfg.Bubblewrap
	if bin == "" {
		bin = "bwrap"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("local runner requires bubblewrap: %w", err)
	}
	return &Runner{bwrap: path}, nil
}

func (r *Runner) Execute(ctx context.Context, op runners.Operation) (runners.Result, error) {
	refuse := func(detail string) (runners.Result, error) {
		return runners.Result{}, &runners.DispatchError{Kind: runners.ErrorRejectedInput, OperationID: op.OperationID, Detail: detail, Err: runners.ErrUnsupportedOperation}
	}
	if op.Runner != RunnerName || op.RunnerRevision != Revision || op.Execution.Kind != runners.ExecutionContainer || op.Execution.ImageDigest != ImageDigest {
		return refuse("runner, revision or execution image is not registered by the local adapter")
	}
	if op.Workspace != nil || op.Policy.Network != runners.NetworkNone || op.Policy.TimeoutSeconds < 1 || op.Policy.TimeoutSeconds > 900 || op.Policy.CPU != nil || op.Policy.MemoryMiB != nil || op.Policy.PIDs != nil || op.Policy.DiskMiB != nil || len(op.Policy.EgressAllowlist) != 0 || len(op.Policy.AllowedOutputPaths) != 0 || len(op.Command.EnvironmentRefs) != 0 || op.Command.WorkingDirectory != "" && op.Command.WorkingDirectory != "/workspace" {
		return refuse("local sandbox cannot enforce this workspace, environment or resource policy")
	}
	if len(op.Command.Argv) != 3 || op.Command.Argv[1] != "-c" || op.Command.Argv[2] == "" {
		return refuse("command must be language, -c, script")
	}
	name := map[string]string{"bash": "bash", "python": "python3", "js": "node"}[op.Command.Argv[0]]
	if name == "" {
		return refuse("language must be bash, python or js")
	}
	interpreter, err := exec.LookPath(name)
	if err != nil {
		return runners.Result{}, &runners.DispatchError{Kind: runners.ErrorRunnerUnavailable, OperationID: op.OperationID, Err: err}
	}
	interpreter, err = filepath.EvalSymlinks(interpreter)
	if err != nil {
		return runners.Result{}, &runners.DispatchError{Kind: runners.ErrorRunnerUnavailable, OperationID: op.OperationID, Err: err}
	}
	policyDigest, err := contracts.DigestValue(op.Policy)
	if err != nil {
		return refuse("cannot digest policy")
	}

	args := []string{"--die-with-parent", "--unshare-all", "--new-session", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--dir", "/workspace"}
	for _, dir := range []string{"/usr", "/bin", "/lib", "/lib64", "/etc"} {
		if _, err := os.Stat(dir); err == nil {
			args = append(args, "--ro-bind", dir, dir)
		}
	}
	// A node installed outside the system directories needs only its binary
	// directory mounted. Other host home directories stay invisible.
	base := filepath.Dir(interpreter)
	if !strings.HasPrefix(base, "/usr/") && !strings.HasPrefix(base, "/bin/") {
		args = append(args, "--ro-bind", base, base)
	}
	for key, value := range runners.ContextEnvironment(op) {
		args = append(args, "--setenv", key, value)
	}
	args = append(args, "--chdir", "/workspace", "--", interpreter, "-c", op.Command.Argv[2])
	deadlineCtx, cancel := context.WithTimeout(ctx, time.Duration(op.Policy.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(deadlineCtx, r.bwrap, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/workspace"}
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	started := time.Now().UTC()
	// Output is intentionally discarded: without an artifact store it cannot
	// be claimed as complete or made durable.
	err = cmd.Run()
	finished := time.Now().UTC()
	if err != nil && cmd.ProcessState == nil {
		return runners.Result{}, &runners.DispatchError{Kind: runners.ErrorRunnerUnavailable, OperationID: op.OperationID, Err: err}
	}
	if strings.HasPrefix(stderr.String(), "bwrap:") {
		return runners.Result{}, &runners.DispatchError{Kind: runners.ErrorRunnerUnavailable, OperationID: op.OperationID, Detail: strings.TrimSpace(stderr.String()), Err: runners.ErrRunnerUnavailable}
	}
	result := runners.Result{
		OperationID: op.OperationID, State: runners.StateCompleted,
		Timing:      runners.Timing{StartedAt: started, FinishedAt: finished, DurationMs: int(finished.Sub(started).Milliseconds())},
		Environment: runners.Environment{RunnerRevision: Revision, ImageDigest: ImageDigest, PolicyDigest: policyDigest},
		Changes:     runners.Changes{Complete: false},
		Observations: runners.Observations{
			ExitStatus:    runners.Observation{Measured: true, Complete: true, Method: "waitpid", Scope: "sandbox process exit"},
			ChangedPaths:  runners.Observation{Measured: false, Complete: false, Note: "ephemeral workspace was not compared"},
			Logs:          runners.Observation{Measured: false, Complete: false, Note: "output was not captured"},
			ResourceUsage: runners.Observation{Measured: false, Complete: false, Note: "resource use was not sampled"},
		},
	}
	if errors.Is(deadlineCtx.Err(), context.DeadlineExceeded) {
		result.State = runners.StateTimedOut
		result.Error = &runners.ResultError{Kind: runners.ErrorTimeout, Message: "sandbox process exceeded timeout"}
		result.Observations.ExitStatus = runners.Observation{Measured: false, Complete: false, Note: "process killed at timeout"}
	} else if errors.Is(deadlineCtx.Err(), context.Canceled) {
		result.State = runners.StateCancelled
		result.Error = &runners.ResultError{Kind: runners.ErrorCancellation, Message: "operation cancelled"}
	} else if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		result.Exit = &runners.Exit{Code: &code}
	}
	return result, nil
}
