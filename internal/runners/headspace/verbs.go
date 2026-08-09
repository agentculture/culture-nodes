package headspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// create provisions the ephemeral workspace, mapping policy timeout/memory/
// cpu/pids/disk onto headspace-cli's create budget flags. Only fields the
// operation actually set are passed; a field left nil lets headspace-cli's
// own default apply rather than this bridge asserting a number the operation
// never declared.
func (b *Bridge) create(ctx context.Context, home string, op runners.Operation, profile string) (workspaceID string, err error) {
	args := []string{"create", "--json", "--provider", b.provider, "--profile", profile}

	switch op.Policy.Network {
	case runners.NetworkFull:
		args = append(args, "--network", "enabled")
	default:
		args = append(args, "--network", "disabled")
	}
	if op.Policy.MemoryMiB != nil {
		args = append(args, "--memory-bytes", strconv.FormatInt(int64(*op.Policy.MemoryMiB)*1024*1024, 10))
	}
	if op.Policy.CPU != nil {
		args = append(args, "--cpu-limit", strconv.FormatFloat(*op.Policy.CPU, 'f', -1, 64))
	}
	if op.Policy.PIDs != nil {
		args = append(args, "--pids-limit", strconv.Itoa(*op.Policy.PIDs))
	}
	if op.Policy.DiskMiB != nil {
		args = append(args, "--storage-bytes", strconv.FormatInt(int64(*op.Policy.DiskMiB)*1024*1024, 10))
	}
	// TimeoutSeconds > 0 is already enforced by checkPolicy in validate.
	args = append(args, "--wall-clock-seconds", strconv.Itoa(op.Policy.TimeoutSeconds))

	pkg, verbErr := b.runVerbBlocking(ctx, home, args, nil)
	if verbErr != nil {
		return "", verbErr
	}
	if pkg.Provenance.WorkspaceID == "" {
		return "", &runners.DispatchError{
			Kind:        runners.ErrorRunnerUnavailable,
			OperationID: op.OperationID,
			Err:         runners.ErrRunnerUnavailable,
			Detail:      "runners/headspace: create exited 0 but its result package named no workspace_id",
		}
	}
	return pkg.Provenance.WorkspaceID, nil
}

// put stages op.Workspace.SourceRef into the workspace before the job starts.
//
// This bridge treats source_ref as a path already present on the local
// filesystem the bridge process runs on -- headspace-cli's own put verb
// takes a HOST_PATH, and staging it is exactly this bridge's job. This is a
// deliberate, recorded scope decision for task t18: internal/artifacts
// defines a pod-agnostic "artifact://<namespace>/<id>" ref resolved only
// through a Store (see internal/artifacts/doc.go), and nothing in this
// package's brief wires a Store into BridgeConfig. Wiring an artifacts.Store
// resolution step in front of this call -- fetch by Ref, stage to a local
// temp path, then put that path -- is follow-up work for whichever task
// connects this bridge to the worker; today a caller handing this bridge an
// artifact:// ref gets a clear, typed refusal (os.Stat fails on it) rather
// than a silent misinterpretation.
func (b *Bridge) put(ctx context.Context, home, ws, sourceRef string) error {
	info, statErr := os.Stat(sourceRef)
	if statErr != nil {
		return &runners.DispatchError{
			Kind: runners.ErrorRejectedInput,
			Err:  runners.ErrUnsupportedOperation,
			Detail: fmt.Sprintf(
				"runners/headspace: workspace.source_ref %q is not a readable path on this bridge's local filesystem: %v -- "+
					"this bridge stages source_ref directly from local disk; it does not yet resolve an internal/artifacts.Ref (see verbs.go put doc)",
				sourceRef, statErr),
		}
	}

	dest := "."
	if !info.IsDir() {
		dest = filepath.Base(sourceRef)
	}

	args := []string{"put", "--json", "--provider", b.provider, ws, sourceRef, dest}
	_, err := b.runVerbBlocking(ctx, home, args, nil)
	return err
}

// declaredOutputPurpose is the fixed --declare purpose this bridge registers
// for every policy-declared output path. headspace-cli's own docs say the
// purpose's only programmatic use is naming protected work in a refused
// destroy; a static, identifiable string is sufficient for that.
const declaredOutputPurpose = "declared runner output path"

// runAndBuildResult runs the operation's command in ws and maps the result
// onto a schema-valid runners.Result.
//
// Unlike every other verb, run is launched with plain exec.Command, not
// exec.CommandContext: run blocks for the whole job and holds the
// workspace's flock the entire time (verified live), so ctx cancellation
// must never SIGKILL it -- doing so would abandon the flock and the running
// container mid-flight with nothing left to observe the outcome. Instead a
// goroutine watches ctx.Done() and, if it fires while run is still blocked,
// launches `headspace stop <ws> --apply` as a SEPARATE process sharing the
// same $HEADSPACE_HOME (verified live: stop takes no lock of its own, and
// the run invocation that is actually blocked observes the signal and
// records the outcome itself). run then exits on its own -- exit 5,
// status "cancelled" -- and this function reaps that exit exactly as it
// would any other, through the same classifyVerbExit path.
func (b *Bridge) runAndBuildResult(
	ctx context.Context,
	home, ws string,
	op runners.Operation,
	envValues map[string]string,
) (runners.Result, error) {
	args := []string{"run", "--json", "--provider", b.provider}

	envNames := make([]string, 0, len(envValues))
	for name := range envValues {
		envNames = append(envNames, name)
	}
	sortStrings(envNames)
	for _, name := range envNames {
		args = append(args, "--env", name)
	}

	for _, path := range op.Policy.AllowedOutputPaths {
		args = append(args, "--declare", path+"="+declaredOutputPurpose)
	}

	args = append(args, ws, "--")
	args = append(args, op.Command.Argv...)

	cmd := exec.Command(b.bin, args...) //nolint:gosec // argv is a slice, never a shell string; see doc.go.
	cmd.Env = buildEnv(home, envValues)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	started := b.now().UTC()
	if startErr := cmd.Start(); startErr != nil {
		return runners.Result{}, &runners.DispatchError{
			Kind:        runners.ErrorRunnerUnavailable,
			OperationID: op.OperationID,
			Err:         runners.ErrRunnerUnavailable,
			Detail:      "runners/headspace: launch headspace run: " + startErr.Error(),
		}
	}

	stopRequested := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			b.requestStop(home, ws)
		case <-stopRequested:
		}
	}()

	waitErr := cmd.Wait()
	close(stopRequested)
	finished := b.now().UTC()

	code, ok := exitCodeOf(waitErr)
	if !ok {
		return runners.Result{}, &runners.DispatchError{
			Kind:        runners.ErrorRunnerUnavailable,
			OperationID: op.OperationID,
			Err:         runners.ErrRunnerUnavailable,
			Detail:      "runners/headspace: headspace run ended without a normal exit (signal): " + errString(waitErr),
		}
	}

	pkg, classifyErr := classifyVerbExit("run", code, stdout.Bytes(), stderr.Bytes())
	if classifyErr != nil {
		if de, isDE := classifyErr.(*runners.DispatchError); isDE {
			de.OperationID = op.OperationID
		}
		return runners.Result{}, classifyErr
	}

	return buildResult(op, pkg, started, finished, b.revision)
}

// requestStop asks headspace-cli's engine to end the job running in ws, from
// a separate process, while the blocking `run` invocation still holds the
// workspace's flock. Its own exit code (5 on a successful stop) is not this
// call's concern: run's own exit, reaped by runAndBuildResult, is the
// authoritative outcome. A failure here (headspace-cli not found, stop
// itself erroring) is intentionally swallowed rather than propagated --
// run is still blocked and will eventually finish or hit its own wall-clock
// timeout on its own, and there is no result to attach a second error to yet.
func (b *Bridge) requestStop(home, ws string) {
	ctx, cancel := context.WithTimeout(context.Background(), b.stopTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.bin, "stop", "--json", "--provider", b.provider, "--apply", ws)
	cmd.Env = buildEnv(home, nil)
	_ = cmd.Run()
}

// sanitizeArtifactName turns a workspace-relative declared path into a flat
// local filename for the export step's own temp directory. Nested paths
// (e.g. "reports/final.csv") are flattened, not mirrored into subdirectories
// -- a documented limitation of this bridge's local export staging, not of
// headspace-cli's export verb itself.
func sanitizeArtifactName(name string) string {
	return strings.ReplaceAll(name, string(filepath.Separator), "_")
}

// exportDeclared exports every path the operation's policy declared as an
// allowed output, into a local staging directory under home, and records
// each exported reference on result.Artifacts. A per-artifact export failure
// does not fail Execute -- the run already genuinely happened and this
// bridge can honestly report on it; the failure is instead recorded as an
// honest, incomplete "artifacts_export" observation (see buildResult) rather
// than discarding an otherwise-valid Result.
//
// These exported references are local filesystem paths on the machine
// running this bridge, not internal/artifacts.Ref values -- see put's doc
// comment for the same, deliberate t18 scope boundary.
func (b *Bridge) exportDeclared(ctx context.Context, home, ws string, op runners.Operation, result *runners.Result) {
	paths := op.Policy.AllowedOutputPaths
	if result.Observations.Additional == nil {
		result.Observations.Additional = map[string]runners.Observation{}
	}
	if len(paths) == 0 {
		return
	}

	exportDir, mkErr := os.MkdirTemp(home, "export-")
	if mkErr != nil {
		result.Observations.Additional["artifacts_export"] = runners.Observation{
			Method: "headspace_export",
			Note:   "could not create a local export staging directory: " + mkErr.Error(),
		}
		return
	}

	exported := map[string]string{}
	var failures []string
	for _, name := range paths {
		to := filepath.Join(exportDir, sanitizeArtifactName(name))
		args := []string{"export", "--json", "--provider", b.provider, "--to", to, ws, name}
		pkg, err := b.runVerbBlocking(ctx, home, args, nil)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if len(pkg.Artifacts) > 0 && pkg.Artifacts[0].Reference != "" {
			exported[name] = pkg.Artifacts[0].Reference
		} else {
			failures = append(failures, fmt.Sprintf("%s: export exited 0 but named no artifact reference", name))
		}
	}

	if len(exported) > 0 {
		if result.Artifacts == nil {
			result.Artifacts = &runners.Artifacts{}
		}
		if result.Artifacts.Additional == nil {
			result.Artifacts.Additional = map[string]string{}
		}
		for name, ref := range exported {
			result.Artifacts.Additional[name] = ref
		}
	}

	obs := runners.Observation{Method: "headspace_export"}
	if len(failures) == 0 {
		obs.Measured = true
		obs.Complete = true
		obs.Scope = fmt.Sprintf("All %d declared output path(s) exported and digest-verified by headspace-cli.", len(paths))
	} else {
		obs.Measured = true
		obs.Complete = false
		obs.Scope = fmt.Sprintf("%d of %d declared output path(s) exported.", len(exported), len(paths))
		obs.Note = "export failed for: " + strings.Join(failures, "; ")
	}
	result.Observations.Additional["artifacts_export"] = obs
}

// cleanupWorkspace destroys ws, always, as Execute's last step regardless of
// how the run went. It never uses ctx directly: Execute may be returning
// precisely because ctx was cancelled, and destroy still has to run to avoid
// leaking a workspace.
//
// The two-step destroy-refusal handling this implements (task t18 item 4):
// try destroy without --force first. headspace-cli refuses (exit 1) when a
// declared artifact was never exported, which this bridge's own flow avoids
// by exporting every declared path before reaching here -- so under normal
// operation the plain destroy succeeds and nothing is discarded. If it is
// refused anyway (an export genuinely failed, or a declared path was never
// produced by the job), this bridge falls back to `destroy --force`, which
// discards the orphaned artifact(s) and always removes the workspace. The
// choice is: never leak a workspace for the sake of protecting an artifact
// this bridge already tried and failed to save -- that failure is already
// recorded in the artifacts_export observation, so a forced destroy on top
// of it does not silently lose information, it just stops holding a
// container open over data nothing will ever collect.
func (b *Bridge) cleanupWorkspace(parent context.Context, home, ws string) string {
	ctx, cancel := context.WithTimeout(detachedContext(parent), b.stopTimeout)
	defer cancel()

	plain := []string{"destroy", "--json", "--provider", b.provider, ws}
	_, err := b.runVerbBlocking(ctx, home, plain, nil)
	if err == nil {
		return "workspace destroyed cleanly; no declared artifact was left unexported."
	}

	var de *runners.DispatchError
	if !errors.As(err, &de) || de.Kind != runners.ErrorRejectedInput {
		return fmt.Sprintf("destroy failed and was not the declared-but-unexported-artifact refusal this bridge falls back on: %v -- the workspace may have leaked.", err)
	}

	forced := []string{"destroy", "--json", "--provider", b.provider, "--force", ws}
	_, forceErr := b.runVerbBlocking(ctx, home, forced, nil)
	if forceErr != nil {
		return fmt.Sprintf("destroy was refused (%v) and the --force fallback also failed (%v) -- the workspace may have leaked.", err, forceErr)
	}
	return fmt.Sprintf("destroy was refused for declared-but-unexported artifact(s) (%v); forced with --force, discarding them (see artifacts_export for which export(s) failed).", err)
}

// sortStrings is sort.Strings, named locally so verbs.go's imports stay
// self-explanatory at the call site.
func sortStrings(s []string) { sort.Strings(s) }

// errString renders err's message, or "" for a nil error.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// detachedContext keeps parent's values (none this bridge relies on today,
// but the right default) while dropping its cancellation, so a cleanup step
// that must run precisely because parent was cancelled is not immediately
// cancelled itself.
func detachedContext(parent context.Context) context.Context {
	return context.WithoutCancel(parent)
}
