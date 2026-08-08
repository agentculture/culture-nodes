package main

import "github.com/agentculture/culture-nodes/internal/clifmt"

// cmdCLI implements the `cli` noun group. It exists to satisfy the
// agent-first rubric's overview_cli_noun_exists check: any noun with
// action-verbs must also expose overview. There is only one action today
// (overview); `cli` with no sub-verb also prints it, mirroring
// culture_nodes/cli/_commands/cli.py's `_no_verb`.
func cmdCLI(args []string, jsonMode bool) (int, error) {
	if len(args) == 0 {
		return emitOverview("nodes cli", cliOverviewSummary(), jsonMode)
	}

	if args[0] == "overview" {
		fs := newFlagSet("cli overview")
		if err := fs.Parse(args[1:]); err != nil {
			return 0, parseError("cli overview", err)
		}
		return emitOverview("nodes cli", cliOverviewSummary(), jsonMode)
	}

	return 0, &clifmt.CliError{
		Code:        clifmt.ExitUserError,
		Message:     "unknown cli subcommand \"" + args[0] + "\"",
		Remediation: "run 'nodes cli overview' — that is the only cli subcommand today",
	}
}
