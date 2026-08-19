package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/jiracutover"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

func cmdCutoverAdopt(args []string, _ bool) (int, error) {
	fs := newFlagSet("cutover-adopt")
	databaseURL := fs.String("database-url", "", "PostgreSQL connection URL")
	if err := fs.Parse(args); err != nil {
		return 0, parseError("cutover-adopt", err)
	}
	dbURL := *databaseURL
	if dbURL == "" {
		dbURL = os.Getenv("NODES_DATABASE_URL")
	}
	email, token := os.Getenv("JIRA_ACCOUNT_EMAIL"), os.Getenv("JIRA_API_TOKEN")
	if dbURL == "" {
		return 0, &clifmt.CliError{Code: clifmt.ExitEnvError, Message: "cutover-adopt requires NODES_DATABASE_URL", Remediation: "load the host production environment and retry before resuming sweeps"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	db, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		return 0, envError("connecting to database", err, "verify NODES_DATABASE_URL")
	}
	defer db.Close()
	pending, err := db.ListPendingJiraHistoryCutovers(ctx)
	if err != nil {
		return 0, envError("listing pending Jira history heads", err, "verify the schema is migrated")
	}
	if len(pending) == 0 {
		clifmt.EmitResult("adopted Jira history heads: 0")
		return clifmt.ExitSuccess, nil
	}
	if email == "" || token == "" {
		return 0, &clifmt.CliError{Code: clifmt.ExitEnvError, Message: "pending Jira history heads require JIRA_ACCOUNT_EMAIL and JIRA_API_TOKEN", Remediation: "load the host runner Jira environment and retry before resuming sweeps"}
	}
	n, err := jiracutover.AdoptPending(ctx, db, jiracutover.JiraClient{Email: email, Token: token}, log.New(os.Stderr, "cutover-adopt: ", 0))
	if err != nil {
		return 0, envError("adopting Jira history heads", err, "leave sweeps stopped, correct Jira/database access, and retry")
	}
	clifmt.EmitResult("adopted Jira history heads: " + fmt.Sprint(n))
	return clifmt.ExitSuccess, nil
}
