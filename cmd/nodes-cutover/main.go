// Command nodes-cutover adopts every pending Jira history-head cutover row
// at the issue's current Jira head (migration 0041, PR #208 finding 7). It
// is deliberately a HOST one-shot rather than a `nodes` verb or an engine
// feature, for two reasons that must not erode:
//
//   - custody: it briefly reads the runner's Jira Basic-auth pair, which
//     must never enter a long-lived control-plane process (spec boundary
//     c4); a transient process that exits when adoption ends is the
//     narrowest holder possible.
//   - the deploy lane's own deviation d1 (tests/deploy/codexdeploylane_test
//     .go): the host QUERY CLI is the Python `nodes` package, and deploy.sh
//     may not build or ship ./cmd/nodes — so the adopter gets its own
//     binary, the same shape as the nodes-runner / nodes-harvest one-shots.
//
// Exit status: 0 adopted (prints the count), 2 environment/refusal — the
// deploy must leave sweeps stopped, fix access, and retry.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/agentculture/culture-nodes/internal/jiracutover"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

func main() {
	os.Exit(run())
}

func run() int {
	databaseURL := flag.String("database-url", "", "PostgreSQL connection URL (default $NODES_DATABASE_URL)")
	flag.Parse()

	logger := log.New(os.Stderr, "nodes-cutover: ", 0)
	dbURL := *databaseURL
	if dbURL == "" {
		dbURL = os.Getenv("NODES_DATABASE_URL")
	}
	if dbURL == "" {
		logger.Print("NODES_DATABASE_URL is required: load the host production environment and retry before resuming sweeps")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	db, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		logger.Printf("connecting to database: %v (verify NODES_DATABASE_URL)", err)
		return 2
	}
	defer db.Close()

	pending, err := db.ListPendingJiraHistoryCutovers(ctx)
	if err != nil {
		logger.Printf("listing pending Jira history heads: %v (verify the schema is migrated)", err)
		return 2
	}
	if len(pending) == 0 {
		fmt.Println("adopted Jira history heads: 0")
		return 0
	}

	email, token := os.Getenv("JIRA_ACCOUNT_EMAIL"), os.Getenv("JIRA_API_TOKEN")
	if email == "" || token == "" {
		logger.Print("pending Jira history heads require JIRA_ACCOUNT_EMAIL and JIRA_API_TOKEN: load the host runner Jira environment and retry before resuming sweeps")
		return 2
	}

	n, err := jiracutover.AdoptPending(ctx, db, jiracutover.JiraClient{Email: email, Token: token}, logger)
	if err != nil {
		logger.Printf("adopting Jira history heads: %v (leave sweeps stopped, correct Jira/database access, and retry)", err)
		return 2
	}
	fmt.Println("adopted Jira history heads:", n)
	return 0
}
