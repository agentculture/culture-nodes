package headspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// This file is the subprocess boundary: one exec.Command per headspace-cli
// verb, argv built as a slice (never a shell string, never interpolated from
// a format string), and the frozen exit band turned into either a parsed
// resultPackage or a typed *runners.DispatchError.

// maxDetailBytes bounds how much of a verb's stdout/stderr this bridge
// quotes into a DispatchError's Detail when the output could not be parsed.
// Wide enough to be useful in a log line, narrow enough not to flood one.
const maxDetailBytes = 2048

// buildEnv constructs the environment for a headspace-cli child process:
// this bridge's own environment, minus any existing HEADSPACE_HOME (so the
// per-Execute value set here always wins over whatever the bridge process
// happens to have), plus HEADSPACE_HOME=home, plus extra (secret values
// resolved from this process's own environment by name -- see
// Bridge.validate/resolveEnv). extra is applied last and wins over anything
// of the same name inherited from the bridge's own environment, so the
// resolved value -- not a stale same-named export sitting in the worker's
// shell -- is what headspace-cli actually reads back with `--env NAME`.
//
// Nothing here ever places a secret value in argv: the only way a value
// crosses into the child is this slice, which becomes the child's envp, never
// a command-line argument.
func buildEnv(home string, extra map[string]string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extra)+1)

	skip := make(map[string]bool, len(extra)+1)
	skip["HEADSPACE_HOME"] = true
	for name := range extra {
		skip[name] = true
	}

	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if skip[name] {
			continue
		}
		out = append(out, kv)
	}

	out = append(out, "HEADSPACE_HOME="+home)

	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, name+"="+extra[name])
	}
	return out
}

// exitCodeOf extracts a process's exit code from the error cmd.Run/cmd.Wait
// returned. ok is false when the process never produced a real exit code at
// all -- it was killed by a signal, or never started -- which this bridge
// always treats as an infrastructure question, never as one of the nine
// frozen exit codes.
func exitCodeOf(err error) (code int, ok bool) {
	if err == nil {
		return 0, true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
		return code, code != -1
	}
	return -1, false
}

// runVerbBlocking runs one headspace-cli verb to completion under ctx,
// capturing stdout/stderr, and classifies the result. It is used for every
// verb except `run`: those all return promptly (no flock, no long-lived
// child), so letting ctx kill them on cancellation is the right behaviour --
// unlike `run`, which Bridge.runAndBuildResult drives without CommandContext
// specifically so cancellation goes through `stop`, never a signal (see that
// file's comment).
func (b *Bridge) runVerbBlocking(ctx context.Context, home string, args []string, extraEnv map[string]string) (*resultPackage, error) {
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}

	cmd := exec.CommandContext(ctx, b.bin, args...)
	cmd.Env = buildEnv(home, extraEnv)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	code, ok := exitCodeOf(runErr)
	if !ok {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, &runners.DispatchError{
				Kind:   runners.ErrorCancellation,
				Detail: fmt.Sprintf("runners/headspace: %s did not produce an exit code because the context ended: %v", verb, ctxErr),
			}
		}
		return nil, &runners.DispatchError{
			Kind:   runners.ErrorRunnerUnavailable,
			Err:    runners.ErrRunnerUnavailable,
			Detail: fmt.Sprintf("runners/headspace: launch/run %s %s: %v", b.bin, verb, runErr),
		}
	}
	return classifyVerbExit(verb, code, stdout.Bytes(), stderr.Bytes())
}

// classifyVerbExit turns one headspace-cli process's exit code and captured
// output into either a parsed resultPackage or a typed DispatchError,
// applying the frozen ADDITIVE exit band verified against headspace-cli
// 0.11.0 (0=success 1=user_error 2=environment_error 3=policy_denied
// 4=timeout 5=cancelled 6=computation_failed 7=infrastructure_failure
// 8=resource_exhausted):
//
//   - 0, 4, 5, 6: headspace-cli wrote a resultPackage to stdout (verified for
//     every one of these codes against the real CLI -- see doc.go's table).
//     These map onto a Result: an execution happened and headspace-cli can
//     honestly describe it, even when the description is "it timed out" or
//     "it exited nonzero" (task t18: exit band 6, "computation failure IS a
//     domain-mappable exit").
//   - 8: headspace-cli ALSO writes a resultPackage to stdout for this one
//     (verified live: a real OOM kill produces a full package with
//     status=resource_exhausted, a real "exit status 137" finding, and a
//     sampled-floor memory reading) -- but per task t18's explicit exit-band
//     table this is grouped with 1/2/7 as a typed DispatchError, not a
//     Result, because runners.State has no ResourceExhausted member and
//     force-fitting it into StateFailed would erase the distinction the whole
//     point of parsing this package was to preserve. The package is still
//     parsed here so the DispatchError's Detail can quote headspace-cli's own
//     diagnosis (the enforced ceiling, in bytes) instead of a bare "exit 8".
//   - 1, 2, 3, 7: headspace-cli wrote a cliError to stderr (verified for 1 and
//     3 against the real CLI; 2 and 7 raise through the same CliError/
//     emit_error path in headspace-cli's own source, so the shape is the
//     same). These never produced a Result headspace-cli itself would stand
//     behind, so they are always a DispatchError.
//   - anything else: outside the documented band. Treated conservatively as
//     an unavailable runner rather than guessed at.
func classifyVerbExit(verb string, code int, stdout, stderr []byte) (*resultPackage, error) {
	switch code {
	case 0, 4, 5, 6:
		pkg, err := parseResultPackage(stdout)
		if err != nil {
			return nil, &runners.DispatchError{
				Kind: runners.ErrorContractFailure,
				Err:  runners.ErrUnsupportedOperation,
				Detail: fmt.Sprintf(
					"runners/headspace: %s exited %d but stdout was not the result package this bridge expects: %v\nstdout: %s",
					verb, code, err, truncateForDetail(stdout)),
			}
		}
		return pkg, nil

	case 8:
		pkg, _ := parseResultPackage(stdout)
		detail := fmt.Sprintf(
			"runners/headspace: %s exited 8 (resource_exhausted): the job was killed for exceeding a declared resource ceiling",
			verb)
		if pkg != nil {
			if len(pkg.KeyFindings) > 0 {
				detail += "; " + strings.Join(pkg.KeyFindings, "; ")
			}
			if len(pkg.Attention) > 0 {
				detail += " -- " + strings.Join(pkg.Attention, "; ")
			}
		}
		return nil, &runners.DispatchError{Kind: runners.ErrorExecutionFailure, Detail: detail}

	case 1, 2, 3, 7:
		kind := kindForExit(code)
		detail := fmt.Sprintf("runners/headspace: %s exited %d (%s)", verb, code, categoryForExit(code))
		if ce, err := parseCLIError(stderr); err == nil {
			detail += ": " + ce.Message
			if ce.Remediation != "" {
				detail += " -- " + ce.Remediation
			}
		} else {
			detail += fmt.Sprintf(" (stderr was not the structured error this bridge expects: %v)\nstderr: %s", err, truncateForDetail(stderr))
		}
		return nil, &runners.DispatchError{Kind: kind, Err: runners.SentinelFor(kind), Detail: detail}

	default:
		return nil, &runners.DispatchError{
			Kind: runners.ErrorRunnerUnavailable,
			Err:  runners.ErrRunnerUnavailable,
			Detail: fmt.Sprintf(
				"runners/headspace: %s exited %d, outside the documented 0-8 exit band\nstdout: %s\nstderr: %s",
				verb, code, truncateForDetail(stdout), truncateForDetail(stderr)),
		}
	}
}

// kindForExit maps a dispatch-error exit code onto this codebase's
// runners.ErrorKind vocabulary. See doc.go for the full table and the
// reasoning behind each choice.
func kindForExit(code int) runners.ErrorKind {
	switch code {
	case 1:
		return runners.ErrorRejectedInput
	case 2:
		return runners.ErrorRunnerUnavailable
	case 3:
		return runners.ErrorAuthOrPolicy
	case 7:
		return runners.ErrorRunnerUnavailable
	default:
		return runners.ErrorRunnerUnavailable
	}
}

// categoryForExit names the exit code the way headspace-cli's own
// EXIT_CATEGORIES does, purely for readable DispatchError messages.
func categoryForExit(code int) string {
	switch code {
	case 0:
		return "success"
	case 1:
		return "user_error"
	case 2:
		return "environment_error"
	case 3:
		return "policy_denied"
	case 4:
		return "timeout"
	case 5:
		return "cancelled"
	case 6:
		return "computation_failed"
	case 7:
		return "infrastructure_failure"
	case 8:
		return "resource_exhausted"
	default:
		return "unknown"
	}
}

// truncateForDetail bounds captured output quoted into an error message.
func truncateForDetail(b []byte) string {
	if len(b) <= maxDetailBytes {
		return string(b)
	}
	return string(b[:maxDetailBytes]) + fmt.Sprintf("...[%d more bytes]", len(b)-maxDetailBytes)
}
