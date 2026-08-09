package main

import (
	"fmt"

	"github.com/agentculture/culture-nodes/internal/clifmt"
)

// planPointer is where a recognized-but-not-implemented mode's remediation
// sends the reader for the delivery plan.
const planPointer = "docs/plans/2026-08-08-culture-nodes-app-design.md"

// stubModeHandler builds the handler for a PRD process-lifecycle mode
// (serve, scheduler, worker, all, validate, run, inspect) that the CLI
// recognizes but does not implement yet. It fails as a genuine CliError
// (not a domain result) since "this mode does not exist yet" is an
// environment/capability gap, not an outcome of running the mode.
func stubModeHandler(name string) handlerFunc {
	return func(_ []string, _ bool) (int, error) {
		return 0, &clifmt.CliError{
			Code:    clifmt.ExitEnvError,
			Message: "not implemented yet",
			Remediation: fmt.Sprintf(
				"mode %q is recognized but not implemented; see %s for the delivery plan",
				name, planPointer,
			),
		}
	}
}
