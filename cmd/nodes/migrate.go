package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// runMigrate implements `nodes migrate`: connect to PostgreSQL and apply
// every pending migration in migrations/ (embedded into the binary), in
// order, recording each in the schema_migrations bookkeeping table. It is
// meant to run standalone -- e.g. as a k8s pre-install/pre-upgrade Job
// (docs/adr/0002-migration-policy.md) -- ahead of a control-plane rollout.
//
// Agent-first output contract, ahead of task t4's full CLI conventions
// landing on this binary: results (one applied migration version per line)
// go to stdout; failures are "error: <msg>" plus a "hint: <remediation>"
// line on stderr. Exit codes: 0 success, 1 user/configuration error, 2
// environment error (unreachable database, failed migration).
func runMigrate(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	databaseURL := fs.String("database-url", "", "PostgreSQL connection URL (defaults to NODES_DATABASE_URL)")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	url := *databaseURL
	if url == "" {
		url = os.Getenv("NODES_DATABASE_URL")
	}
	if url == "" {
		fmt.Fprintln(os.Stderr, "error: no database URL configured")
		fmt.Fprintln(os.Stderr, "hint: set NODES_DATABASE_URL or pass --database-url postgres://...")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db, err := postgres.Connect(ctx, url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: connecting to database: %v\n", err)
		fmt.Fprintln(os.Stderr, "hint: verify NODES_DATABASE_URL is reachable and credentials are correct")
		return 2
	}
	defer db.Close()

	applied, err := db.Migrate(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: applying migrations: %v\n", err)
		fmt.Fprintln(os.Stderr, "hint: inspect the schema_migrations table and the failing migration file")
		return 2
	}

	if len(applied) == 0 {
		fmt.Println("no pending migrations")
		return 0
	}
	for _, version := range applied {
		fmt.Println(version)
	}
	return 0
}
