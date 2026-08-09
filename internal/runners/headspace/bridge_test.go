package headspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/runners/headspace"
)

// fakeDigest is a syntactically valid (but meaningless) sha256 digest, used
// as the pinned image digest in every test operation. It is not any real
// image's digest.
const fakeDigest = "sha256:" + "ab" + "0000000000000000000000000000000000000000000000000000000000"

func fakeScriptPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("testdata/fakeheadspace/fake-headspace.sh")
	if err != nil {
		t.Fatalf("resolve fake script path: %v", err)
	}
	if err := os.Chmod(path, 0o755); err != nil { //nolint:gosec // test fixture, deliberately executable.
		t.Fatalf("chmod fake script: %v", err)
	}
	return path
}

// newTestBridge builds a Bridge pointed at the fake headspace-cli script.
func newTestBridge(t *testing.T, mutate func(*headspace.BridgeConfig)) *headspace.Bridge {
	t.Helper()
	cfg := headspace.BridgeConfig{
		HeadspaceBin:  fakeScriptPath(t),
		Profile:       map[string]string{fakeDigest: headspace.DefaultProfilePython312},
		Provider:      "fake",
		HeadspaceHome: t.TempDir(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	b, err := headspace.New(cfg)
	if err != nil {
		t.Fatalf("headspace.New: %v", err)
	}
	return b
}

// baseOperation builds a minimal, valid Operation targeting the test bridge.
func baseOperation(t *testing.T, b *headspace.Bridge, argv []string) runners.Operation {
	t.Helper()
	return runners.Operation{
		OperationID:    "op-" + t.Name(),
		Runner:         headspace.RunnerName,
		RunnerRevision: b.RunnerRevision(),
		Execution: runners.Execution{
			Kind:        runners.ExecutionContainer,
			ImageDigest: fakeDigest,
		},
		Command: runners.Command{Argv: argv},
		Policy: runners.Policy{
			TimeoutSeconds:     30,
			Network:            runners.NetworkNone,
			AllowedOutputPaths: []string{},
		},
		Evidence: runners.EvidenceRequest{CaptureExit: true, CaptureLogs: true},
	}
}

// recordFile sets NODES_FAKE_RECORD_FILE for the current test and returns its
// path, so a test can assert exactly which headspace-cli verbs the bridge
// actually invoked (and in what order), by reading the recorded ARGV lines.
func recordFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "record.txt")
	t.Setenv("NODES_FAKE_RECORD_FILE", path)
	return path
}

func readRecord(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-controlled path.
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read record file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// verbsInvoked extracts just the verb (first ARGV token) from each recorded
// line, in call order.
func verbsInvoked(lines []string) []string {
	verbs := make([]string, 0, len(lines))
	for _, line := range lines {
		rest, ok := strings.CutPrefix(line, "ARGV: [")
		if !ok {
			continue
		}
		verb, _, _ := strings.Cut(rest, "]")
		verbs = append(verbs, verb)
	}
	return verbs
}

func TestNew_RequiresProfileMapping(t *testing.T) {
	_, err := headspace.New(headspace.BridgeConfig{HeadspaceBin: "headspace"})
	if err == nil {
		t.Fatal("expected New to refuse an empty Profile map")
	}
}

func TestNew_RejectsMalformedRunnerRevision(t *testing.T) {
	_, err := headspace.New(headspace.BridgeConfig{
		Profile:        map[string]string{fakeDigest: "python3.12"},
		RunnerRevision: "not-a-digest",
	})
	if err == nil {
		t.Fatal("expected New to refuse a non-digest RunnerRevision")
	}
}

func TestNew_DefaultRevisionIsStable(t *testing.T) {
	b1, err := headspace.New(headspace.BridgeConfig{Profile: map[string]string{fakeDigest: "python3.12"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b2, err := headspace.New(headspace.BridgeConfig{Profile: map[string]string{fakeDigest: "python3.12"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b1.RunnerRevision() != b2.RunnerRevision() {
		t.Fatalf("default runner revision is not stable across New calls: %s vs %s", b1.RunnerRevision(), b2.RunnerRevision())
	}
	if !strings.HasPrefix(b1.RunnerRevision(), contracts.DigestPrefix) {
		t.Fatalf("default runner revision %q does not carry the digest prefix", b1.RunnerRevision())
	}
}

// TestExecute_ExitBandTable drives Execute's run step through every one of
// headspace-cli's nine frozen exit codes against the fake binary, and checks
// this bridge's classification of each one: 0/4/5/6 become a Result with the
// state task t18 specifies, and 1/2/3/7/8 become a typed DispatchError.
func TestExecute_ExitBandTable(t *testing.T) {
	tests := []struct {
		exitCode     int
		wantState    runners.State
		wantExitCode *int
		wantDispatch runners.ErrorKind
	}{
		{exitCode: 0, wantState: runners.StateCompleted, wantExitCode: intPtr(0)},
		{exitCode: 1, wantDispatch: runners.ErrorRejectedInput},
		{exitCode: 2, wantDispatch: runners.ErrorRunnerUnavailable},
		{exitCode: 3, wantDispatch: runners.ErrorAuthOrPolicy},
		{exitCode: 4, wantState: runners.StateTimedOut},
		{exitCode: 5, wantState: runners.StateCancelled},
		{exitCode: 6, wantState: runners.StateCompleted, wantExitCode: intPtr(1)},
		{exitCode: 7, wantDispatch: runners.ErrorRunnerUnavailable},
		{exitCode: 8, wantDispatch: runners.ErrorExecutionFailure},
	}

	for _, tt := range tests {
		t.Run("exit_"+strconv.Itoa(tt.exitCode), func(t *testing.T) {
			t.Setenv("NODES_FAKE_RUN_EXIT", strconv.Itoa(tt.exitCode))
			b := newTestBridge(t, nil)
			op := baseOperation(t, b, []string{"python3", "-c", "print(1)"})

			result, err := b.Execute(context.Background(), op)

			if tt.wantDispatch != "" {
				if err == nil {
					t.Fatalf("exit %d: expected a DispatchError, got a Result: %+v", tt.exitCode, result)
				}
				var de *runners.DispatchError
				if !errors.As(err, &de) {
					t.Fatalf("exit %d: error %v is not a *runners.DispatchError", tt.exitCode, err)
				}
				if de.Kind != tt.wantDispatch {
					t.Fatalf("exit %d: DispatchError.Kind = %s, want %s", tt.exitCode, de.Kind, tt.wantDispatch)
				}
				return
			}

			if err != nil {
				t.Fatalf("exit %d: unexpected error: %v", tt.exitCode, err)
			}
			if result.State != tt.wantState {
				t.Fatalf("exit %d: State = %s, want %s", tt.exitCode, result.State, tt.wantState)
			}
			if tt.wantExitCode != nil {
				code, ok := result.ExitCode()
				if !ok {
					t.Fatalf("exit %d: expected an exit code on the result, got none", tt.exitCode)
				}
				if code != *tt.wantExitCode {
					t.Fatalf("exit %d: result exit code = %d, want %d", tt.exitCode, code, *tt.wantExitCode)
				}
			} else if code, ok := result.ExitCode(); ok {
				t.Fatalf("exit %d: expected no exit code (state %s), got %d", tt.exitCode, tt.wantState, code)
			}

			// Every returned Result must carry the four required observations
			// and be schema-shaped: exit_status honesty in particular is the
			// point of this bridge.
			if result.Observations.ExitStatus.Method == "" {
				t.Fatalf("exit %d: exit_status observation has no method", tt.exitCode)
			}
			if result.Observations.ChangedPaths.Measured {
				t.Fatalf("exit %d: changed_paths must never be measured -- headspace-cli has no snapshot/diff verb", tt.exitCode)
			}
		})
	}
}

// TestExecute_Exit0_ExitStatusObservationIsHonest checks the exit_status
// observation's honesty fields directly, not just the Result's Exit code.
func TestExecute_Exit0_ExitStatusObservationIsHonest(t *testing.T) {
	t.Setenv("NODES_FAKE_RUN_EXIT", "0")
	b := newTestBridge(t, nil)
	op := baseOperation(t, b, []string{"python3", "-c", "print('hi')"})

	result, err := b.Execute(context.Background(), op)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	obs := result.Observations.ExitStatus
	if !obs.Measured || !obs.Complete {
		t.Fatalf("exit_status should be measured+complete when headspace-cli reported one: %+v", obs)
	}
	if obs.Note == "" {
		t.Fatal("exit_status observation should explain that headspace-cli, not this bridge, watched the process")
	}
}

// TestExecute_CreateFailure_NeverAttemptsRun proves the bridge aborts before
// ever launching `run` (or `destroy`, since no workspace exists) when create
// itself is refused.
func TestExecute_CreateFailure_NeverAttemptsRun(t *testing.T) {
	record := recordFile(t)
	t.Setenv("NODES_FAKE_CREATE_EXIT", "3")
	b := newTestBridge(t, nil)
	op := baseOperation(t, b, []string{"python3", "-c", "print(1)"})

	_, err := b.Execute(context.Background(), op)
	var de *runners.DispatchError
	if !errors.As(err, &de) {
		t.Fatalf("expected a DispatchError, got %v", err)
	}
	if de.Kind != runners.ErrorAuthOrPolicy {
		t.Fatalf("Kind = %s, want %s", de.Kind, runners.ErrorAuthOrPolicy)
	}

	verbs := verbsInvoked(readRecord(t, record))
	if len(verbs) != 1 || verbs[0] != "create" {
		t.Fatalf("expected only [create] to be invoked, got %v", verbs)
	}
}

// TestExecute_MissingEnvironmentRef_RefusesBeforeAnyProcess proves an
// unresolved environment_ref is refused by validate(), before create (or
// anything else) ever launches a subprocess.
func TestExecute_MissingEnvironmentRef_RefusesBeforeAnyProcess(t *testing.T) {
	record := recordFile(t)
	os.Unsetenv("NODES_HEADSPACE_TEST_UNSET_VAR") //nolint:errcheck
	b := newTestBridge(t, nil)
	op := baseOperation(t, b, []string{"python3", "-c", "print(1)"})
	op.Command.EnvironmentRefs = []string{"NODES_HEADSPACE_TEST_UNSET_VAR"}

	_, err := b.Execute(context.Background(), op)
	var de *runners.DispatchError
	if !errors.As(err, &de) {
		t.Fatalf("expected a DispatchError, got %v", err)
	}
	if de.Kind != runners.ErrorRejectedInput {
		t.Fatalf("Kind = %s, want %s", de.Kind, runners.ErrorRejectedInput)
	}
	if !strings.Contains(de.Detail, "NODES_HEADSPACE_TEST_UNSET_VAR") {
		t.Fatalf("Detail should name the missing ref, got: %s", de.Detail)
	}

	if verbs := verbsInvoked(readRecord(t, record)); len(verbs) != 0 {
		t.Fatalf("expected zero subprocesses launched, got %v", verbs)
	}
}

// TestExecute_SecretValueNeverInArgv is the fake-binary-side companion to
// live_test.go's real-CLI secret test: it proves this bridge's own argv
// construction never places a resolved secret value in the recorded argv,
// only its name behind --env.
func TestExecute_SecretValueNeverInArgv(t *testing.T) {
	record := recordFile(t)
	const secretValue = "sk-test-super-secret-value-12345"
	t.Setenv("NODES_HEADSPACE_TEST_SECRET", secretValue)
	t.Setenv("NODES_FAKE_RUN_EXIT", "0")

	b := newTestBridge(t, nil)
	op := baseOperation(t, b, []string{"python3", "-c", "import os; print(os.environ['NODES_HEADSPACE_TEST_SECRET'])"})
	op.Command.EnvironmentRefs = []string{"NODES_HEADSPACE_TEST_SECRET"}

	if _, err := b.Execute(context.Background(), op); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	raw, err := os.ReadFile(record) //nolint:gosec // test-controlled path.
	if err != nil {
		t.Fatalf("read record file: %v", err)
	}
	if strings.Contains(string(raw), secretValue) {
		t.Fatalf("secret value leaked into recorded argv:\n%s", raw)
	}
	if !strings.Contains(string(raw), "--env") || !strings.Contains(string(raw), "NODES_HEADSPACE_TEST_SECRET") {
		t.Fatalf("expected --env NODES_HEADSPACE_TEST_SECRET in recorded argv:\n%s", raw)
	}
}

// TestExecute_ArtifactExport proves a declared output path is exported after
// a successful run and recorded on Result.Artifacts.
func TestExecute_ArtifactExport(t *testing.T) {
	t.Setenv("NODES_FAKE_RUN_EXIT", "0")
	b := newTestBridge(t, nil)
	op := baseOperation(t, b, []string{"python3", "-c", "print(1)"})
	op.Policy.AllowedOutputPaths = []string{"out.txt"}

	result, err := b.Execute(context.Background(), op)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Artifacts == nil || result.Artifacts.Additional["out.txt"] == "" {
		t.Fatalf("expected an exported reference for out.txt, got: %+v", result.Artifacts)
	}
	exportedPath := result.Artifacts.Additional["out.txt"]
	content, err := os.ReadFile(exportedPath) //nolint:gosec // path built by this bridge in a per-test temp dir.
	if err != nil {
		t.Fatalf("read exported artifact at %s: %v", exportedPath, err)
	}
	if string(content) != "fake-artifact-bytes" {
		t.Fatalf("exported artifact content = %q, want %q", content, "fake-artifact-bytes")
	}

	obs, ok := result.Observations.Get("artifacts_export")
	if !ok || !obs.Measured || !obs.Complete {
		t.Fatalf("expected a measured, complete artifacts_export observation, got: %+v", obs)
	}
}

// TestExecute_DestroyRefusal_FallsBackToForce proves that when destroy is
// refused (a declared artifact was never exported, from headspace-cli's own
// point of view), this bridge retries with --force rather than leaking the
// workspace, and records that choice honestly.
func TestExecute_DestroyRefusal_FallsBackToForce(t *testing.T) {
	record := recordFile(t)
	t.Setenv("NODES_FAKE_RUN_EXIT", "0")
	t.Setenv("NODES_FAKE_DESTROY_MODE", "refuse-then-force")
	b := newTestBridge(t, nil)
	op := baseOperation(t, b, []string{"python3", "-c", "print(1)"})

	result, err := b.Execute(context.Background(), op)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	obs, ok := result.Observations.Get("workspace_cleanup")
	if !ok {
		t.Fatal("expected a workspace_cleanup observation")
	}
	if !strings.Contains(obs.Note, "force") {
		t.Fatalf("expected workspace_cleanup to mention the --force fallback, got: %s", obs.Note)
	}

	lines := readRecord(t, record)
	var sawPlainDestroy, sawForcedDestroy bool
	for _, line := range lines {
		if strings.HasPrefix(line, "ARGV: [destroy]") {
			if strings.Contains(line, "[--force]") {
				sawForcedDestroy = true
			} else {
				sawPlainDestroy = true
			}
		}
	}
	if !sawPlainDestroy {
		t.Fatalf("expected a plain destroy attempt before the forced one; record:\n%s", strings.Join(lines, "\n"))
	}
	if !sawForcedDestroy {
		t.Fatalf("expected a --force destroy fallback; record:\n%s", strings.Join(lines, "\n"))
	}
}

// TestExecute_Cancellation_UsesSeparateStopProcess proves the cancellation
// mechanism itself: ctx cancellation while `run` is still blocked spawns
// `stop --apply` as a separate process (the fake run verb only ever exits by
// observing a marker file that only the fake stop verb writes -- there is no
// other way for it to reach exit 5), and Execute reaps the resulting
// cancelled Result.
func TestExecute_Cancellation_UsesSeparateStopProcess(t *testing.T) {
	record := recordFile(t)
	marker := filepath.Join(t.TempDir(), "stop.marker")
	t.Setenv("NODES_FAKE_RUN_MODE", "await-stop")
	t.Setenv("NODES_FAKE_STOP_MARKER", marker)

	b := newTestBridge(t, nil)
	op := baseOperation(t, b, []string{"python3", "-c", "print(1)"})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	result, err := b.Execute(ctx, op)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.State != runners.StateCancelled {
		t.Fatalf("State = %s, want %s", result.State, runners.StateCancelled)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("expected the stop marker to exist (proving `stop --apply` ran as a separate process): %v", statErr)
	}
	// The fake run verb polls the marker every 50ms and cancel() fires at
	// 150ms; this should resolve in well under the fake's own 10s safety
	// valve if the separate-process signal actually worked.
	if elapsed > 5*time.Second {
		t.Fatalf("cancellation took %s -- the stop signal likely did not reach the blocked run", elapsed)
	}

	verbs := verbsInvoked(readRecord(t, record))
	var sawStopApply bool
	for _, line := range readRecord(t, record) {
		if strings.HasPrefix(line, "ARGV: [stop]") && strings.Contains(line, "[--apply]") {
			sawStopApply = true
		}
	}
	if !sawStopApply {
		t.Fatalf("expected a `stop --apply` invocation; verbs invoked: %v", verbs)
	}
}

func intPtr(n int) *int { return &n }

// The working-directory boundary (task t27). headspace-cli's run verb has no
// working-directory flag, but every job it runs starts in
// headspace.JobWorkingDirectory — which is also the directory
// internal/compiler stamps on every code operation by default. Refusing that
// value would make the compiler's own §13.7 safe default undispatchable to
// the only real runner in the build; refusing any OTHER value is still
// mandatory, because this bridge cannot make it true.
func TestExecute_WorkingDirectory(t *testing.T) {
	t.Run("the directory headspace already runs in is accepted", func(t *testing.T) {
		b := newTestBridge(t, nil)
		op := baseOperation(t, b, []string{"true"})
		op.Command.WorkingDirectory = headspace.JobWorkingDirectory

		result, err := b.Execute(context.Background(), op)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if result.State != runners.StateCompleted {
			t.Fatalf("State = %s, want completed", result.State)
		}
	})

	t.Run("any other directory is refused before a subprocess runs", func(t *testing.T) {
		record := recordFile(t)
		b := newTestBridge(t, nil)
		op := baseOperation(t, b, []string{"true"})
		op.Command.WorkingDirectory = "/somewhere/else"

		_, err := b.Execute(context.Background(), op)
		var dispatchErr *runners.DispatchError
		if !errors.As(err, &dispatchErr) {
			t.Fatalf("Execute error = %v, want a *runners.DispatchError", err)
		}
		if dispatchErr.Kind != runners.ErrorRejectedInput {
			t.Errorf("refusal kind = %q, want rejected_input", dispatchErr.Kind)
		}
		if !strings.Contains(dispatchErr.Detail, headspace.JobWorkingDirectory) {
			t.Errorf("refusal %q does not name the directory headspace does run in", dispatchErr.Detail)
		}
		if verbs := readRecord(t, record); len(verbs) != 0 {
			t.Errorf("the refusal launched %d subprocesses: %v", len(verbs), verbs)
		}
	})
}
