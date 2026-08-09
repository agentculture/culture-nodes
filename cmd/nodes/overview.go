package main

import (
	"fmt"
	"strings"

	"github.com/agentculture/culture-nodes/internal/clifmt"
)

// overviewReport is the shared JSON shape for both `nodes overview`
// (describes the agent/binary) and `nodes cli overview` (describes the CLI
// surface itself) — distinct subjects, same one-paragraph-snapshot shape.
type overviewReport struct {
	Subject string `json:"subject"`
	Summary string `json:"summary"`
}

// emitOverview renders subject/summary as either a small markdown heading
// (text mode) or {"subject","summary"} (JSON mode) and returns success.
func emitOverview(subject, summary string, jsonMode bool) (int, error) {
	if jsonMode {
		if err := clifmt.EmitResultJSON(overviewReport{Subject: subject, Summary: summary}); err != nil {
			return 0, err
		}
		return clifmt.ExitSuccess, nil
	}
	clifmt.EmitResult(fmt.Sprintf("# %s\n\n%s", subject, summary))
	return clifmt.ExitSuccess, nil
}

// agentOverviewSummary is `nodes overview`'s one-paragraph snapshot: this
// agent/binary's identity plus its verb surface.
func agentOverviewSummary() string {
	report := newWhoamiReport()
	verbs := []string{"whoami", "learn", "explain <path>", "overview", "doctor", "cli overview", "validate <file>"}
	stubModes := []string{"scheduler", "worker", "run", "inspect"}
	return fmt.Sprintf(
		"nodes is the Culture Nodes control-plane CLI, identified here as nick %q on backend %q "+
			"(version %s); it currently exposes the verbs %s, runs the control-plane API via "+
			"serve and all (api/openapi/openapi.yaml), and recognizes but does not yet implement "+
			"the process-lifecycle modes %s described in docs/initial-design/ — every command "+
			"accepts --json and follows the stdout-results/stderr-errors contract documented by "+
			"'nodes learn'.",
		report.Nick, report.Backend, report.Version, strings.Join(verbs, ", "), strings.Join(stubModes, ", "),
	)
}

// cliOverviewSummary is `nodes cli overview`'s one-paragraph snapshot: the
// CLI-surface conventions themselves, not this agent's identity.
func cliOverviewSummary() string {
	return "The nodes CLI follows the agent-first contract cited from the AgentCulture repo " +
		"family (devague, headspace-cli, ec2bedrock-cli, and this repo's own Python culture_nodes " +
		"CLI): command results are written to stdout and errors/diagnostics to stderr, and the two " +
		"streams are never mixed; every command accepts --json, including parse-time failures such " +
		"as an unknown command or an unrecognized flag, rendering {\"code\",\"message\",\"remediation\"} " +
		"on stderr; exit codes are 0 for success, 1 for a user-input error, 2 for an " +
		"environment/setup error, and 3+ reserved; and text-mode errors always render as two lines, " +
		"'error: <message>' followed by 'hint: <remediation>'."
}

func cmdOverview(args []string, jsonMode bool) (int, error) {
	fs := newFlagSet("overview")
	// `target` is accepted and ignored (nargs="?" equivalent) so a stray
	// path argument never hard-fails — overview always describes this
	// binary itself, mirroring culture_nodes/cli/_commands/overview.py.
	if err := fs.Parse(args); err != nil {
		return 0, parseError("overview", err)
	}
	return emitOverview("nodes", agentOverviewSummary(), jsonMode)
}
