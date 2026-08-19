// Command nodes is the Culture Nodes control-plane entry point.
//
// It wires the agent-first CLI contract from internal/clifmt onto the
// verb surface: the introspection verbs whoami, learn, explain, overview,
// doctor, and cli overview; the process modes the PRD describes (serve,
// scheduler, worker, all — real; inspect is still recognized-but-stubbed,
// reporting a structured CliError instead of dispatching to work);
// validate, which compiles a workflow definition through internal/compiler;
// and run, the first-class ad-hoc run lane (task t19, issue #36).
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/agentculture/culture-nodes/internal/clifmt"
)

// version is the Go control-plane CLI's own version. t1's scaffold did not
// establish a Go versioning scheme (only pyproject.toml's is CI-enforced
// via version-check); this is a placeholder until the Go side gets one.
//
// It is a VAR rather than a const because the Dockerfile overrides it with
// `-ldflags "-X main.version=$VERSION"`, which cannot write to a constant —
// so the build flag that has been in the Dockerfile since t1 has never
// actually done anything. Task t32 makes it, and its new sibling below, real.
var version = "0.1.0-dev"

// revision is the git commit this binary was built from, injected by
// `-ldflags "-X main.revision=<sha>"` and served on GET /v1alpha1/version.
//
// It exists because the container is built from a source tree with NO .git in
// it (see the Dockerfile: it COPYs cmd/, internal/, schemas/ and migrations/,
// never the repository), so the Go toolchain stamps no vcs information and
// the binary has no other way to know. Empty is honest and is reported as
// unknown — internal/api's handleVersion falls back to the toolchain's own
// stamp for a binary built inside a checkout, which is the developer case.
//
// Issue #104 is what its absence cost: `culture-nodes:prod` on thor was 15
// hours old and running none of a merged batch, and the only way to find out
// was to POST at a route that should have existed and read the 405.
var revision = ""

const usageText = `nodes - Culture Nodes control-plane entry point

Usage:
  nodes <command> [args...]

Introspection:
  whoami             identity from culture.yaml (nick, backend, version)
  learn              self-teaching prompt naming every verb
  explain <path>...  markdown docs for a verb/mode path
  overview           one-paragraph descriptive snapshot
  doctor             environment/identity checks
  cli overview       describe the CLI surface itself

Database:
  migrate            apply pending schema migrations (NODES_DATABASE_URL)
  cutover-adopt      adopt Jira history heads before resuming sweeps

Workflows:
  validate <file>    compile a workflow definition and report diagnostics

Runs:
  run                create a first-class ad-hoc run from an instruction

Plan import:
  plan-import        import an external plan's faithful view (--plan, --deviations)

Decompose pipeline:
  chain-verify       check a run's claim/decision chain (--run)

Runner services:
  runner-services    list or register file-backed runner services

Process modes:
  serve  scheduler  worker  all

Recognized, not yet implemented:
  inspect

Every command supports --json, including parse-time failures. Run
'nodes learn' for the full contract, or 'nodes explain <path>' for docs
on any single verb.
`

// handlerFunc is a verb implementation. It returns the process exit code
// for its *domain* outcome (e.g. doctor returning non-zero because a check
// failed, not because anything went wrong invoking it) and/or a non-nil
// error — which must be a *clifmt.CliError for a deliberate failure; any
// other error, or a panic, is normalised by clifmt.Guard.
type handlerFunc func(args []string, jsonMode bool) (int, error)

// processModes are the PRD's process-lifecycle modes: recognized by the
// CLI, not yet implemented.
var processModes = []string{"serve", "scheduler", "worker", "all", "run", "inspect"}

func commands() map[string]handlerFunc {
	m := map[string]handlerFunc{
		"whoami":   cmdWhoami,
		"learn":    cmdLearn,
		"explain":  cmdExplain,
		"overview": cmdOverview,
		"doctor":   cmdDoctor,
		"cli":      cmdCLI,
		"validate": cmdValidate,
		// migrate predates full clifmt wiring: runMigrate owns its own
		// stream/exit contract (results stdout, error:/hint: stderr).
		"migrate": func(args []string, _ bool) (int, error) {
			return runMigrate(args), nil
		},
		"cutover-adopt": cmdCutoverAdopt,
	}
	for _, name := range processModes {
		m[name] = stubModeHandler(name)
	}
	// Implemented process modes replace their stub. Registered after the loop
	// so adding one is a single line and the stub list stays the source of
	// truth for what is still outstanding.
	m["serve"] = cmdServe
	m["all"] = cmdAll
	m["worker"] = cmdWorker
	m["scheduler"] = cmdScheduler
	m["run"] = cmdRun
	m["plan-import"] = cmdPlanImport
	m["chain-verify"] = cmdChainVerify
	m["runner-services"] = cmdRunnerServices
	return m
}

func main() {
	os.Exit(run(os.Args[1:]))
}

// run implements the full dispatch: pre-scan for --json (so it applies
// even to parse-time failures below), resolve the verb, run it under
// clifmt.Guard, and translate the result into results-to-stdout /
// errors-to-stderr / an exit code. It never touches os.Exit directly so it
// stays unit-testable.
func run(argv []string) int {
	rest, jsonMode := clifmt.StripJSONFlag(argv)

	if len(rest) == 0 {
		clifmt.EmitResult(usageText)
		return clifmt.ExitSuccess
	}
	if rest[0] == "-h" || rest[0] == "--help" {
		clifmt.EmitResult(usageText)
		return clifmt.ExitSuccess
	}

	verb, verbArgs := rest[0], rest[1:]
	handler, ok := commands()[verb]
	if !ok {
		err := &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("unknown command %q", verb),
			Remediation: "run 'nodes learn' to see valid commands",
		}
		_ = clifmt.EmitError(err, jsonMode)
		return err.Code
	}

	var exitCode int
	if cliErr := clifmt.Guard(func() error {
		code, err := handler(verbArgs, jsonMode)
		exitCode = code
		return err
	}); cliErr != nil {
		_ = clifmt.EmitError(cliErr, jsonMode)
		return cliErr.Code
	}
	return exitCode
}

// newFlagSet builds a flag.FlagSet for verb whose errors are reported by
// the caller (never printed by the flag package itself) — ContinueOnError
// plus a discarded Output means a bad flag returns an error instead of
// writing "flag provided but not defined" straight to the real stderr and
// calling os.Exit(2), which would bypass the CliError contract entirely.
func newFlagSet(verb string) *flag.FlagSet {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseError wraps a flag-parsing failure as a CliError in the
// user-error bucket (code 1) with a remediation pointing at explain.
func parseError(verb string, err error) *clifmt.CliError {
	return &clifmt.CliError{
		Code:        clifmt.ExitUserError,
		Message:     err.Error(),
		Remediation: fmt.Sprintf("run 'nodes explain %s' to see usage", verb),
	}
}
