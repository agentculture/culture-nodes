// Command nodes is the Culture Nodes control-plane entry point.
//
// It is a scaffold: it recognizes the process modes the PRD describes but
// does not yet dispatch to any of them. Real dispatch lands in later tasks.
package main

import (
	"fmt"
	"os"
)

const usage = `nodes - Culture Nodes control-plane entry point

Usage:
  nodes <mode> [args...]

Modes:
  serve      run the API server
  scheduler  run the scheduler process
  worker     run the worker process
  all        run serve+scheduler+worker in a single process (dev mode)
  validate   validate a workflow or contract definition
  run        start a workflow run
  inspect    inspect ledger records for a run
  migrate    apply pending PostgreSQL migrations (NODES_DATABASE_URL or --database-url)
`

var modes = map[string]bool{
	"serve":     true,
	"scheduler": true,
	"worker":    true,
	"all":       true,
	"validate":  true,
	"run":       true,
	"inspect":   true,
	"migrate":   true,
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	mode := os.Args[1]
	if !modes[mode] {
		fmt.Fprintf(os.Stderr, "nodes: unknown mode %q\n\n%s", mode, usage)
		os.Exit(1)
	}

	// migrate is wired up (task t6); every other mode is still a scaffold.
	if mode == "migrate" {
		os.Exit(runMigrate(os.Args[2:]))
	}

	fmt.Fprintf(os.Stderr, "nodes: mode %q is not implemented yet (scaffold)\n", mode)
	os.Exit(1)
}
