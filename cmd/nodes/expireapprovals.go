package main

// `nodes expire-approvals` (task t11, spec c6): the one-shot backfill for
// pending human tasks whose subject pull request has already merged.
//
// # Why a command and not just the periodic consumer
//
// internal/humanfanout expires tasks the control plane has been TOLD about:
// it joins pending tasks against delivered `pr.merged` facts. The 26 stale
// approvals on prod that this task exists to clear are not in that set. The
// sweep only emits pr.merged for a pull request whose branch or body carries
// a correlatable Jira key (examples/pr-upkeep/sweep.py's merged_pr_fact), and
// PRs #236/#238/#244/#246 carried none — so no fact exists, and no consumer
// reading facts can ever see them.
//
// This command closes that gap honestly rather than by widening the
// consumer's evidence: the operator names the merged pull requests, and the
// recorded expiry detail says the operator named them. An expiry backed by a
// person's assertion and one backed by a delivered fact are different, and
// they read differently in the ledger afterwards.
//
// # Dry run by default
//
// Nothing is written without --apply. The dry run prints exactly the task ids
// the same invocation would expire, so "what would this do to prod" is
// answerable before it does it.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/humanfanout"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// mergedPRFlag collects repeated --pr owner/repo#number values.
type mergedPRFlag []mergedPR

type mergedPR struct {
	Repository string
	Number     int
}

func (f *mergedPRFlag) String() string {
	parts := make([]string, 0, len(*f))
	for _, pr := range *f {
		parts = append(parts, fmt.Sprintf("%s#%d", pr.Repository, pr.Number))
	}
	return strings.Join(parts, ",")
}

func (f *mergedPRFlag) Set(value string) error {
	repo, number, ok := strings.Cut(strings.TrimSpace(value), "#")
	if !ok || repo == "" {
		return fmt.Errorf("expected owner/repo#number, got %q", value)
	}
	n, err := strconv.Atoi(number)
	if err != nil || n <= 0 {
		return fmt.Errorf("expected a positive pull-request number in %q", value)
	}
	*f = append(*f, mergedPR{Repository: repo, Number: n})
	return nil
}

// expireApprovalsPayload is `--json`'s stable result shape.
type expireApprovalsPayload struct {
	NamespaceID string                `json:"namespace_id"`
	Applied     bool                  `json:"applied"`
	Selected    int                   `json:"selected"`
	Expired     int                   `json:"expired"`
	Tasks       []expireApprovalsTask `json:"tasks"`
}

type expireApprovalsTask struct {
	HumanTaskID string `json:"human_task_id"`
	RunID       string `json:"run_id,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	Error       string `json:"error,omitempty"`
}

func cmdExpireApprovals(args []string, jsonMode bool) (int, error) {
	fs := newFlagSet("expire-approvals")
	databaseURL := fs.String("database-url", "", "PostgreSQL connection URL (defaults to "+envDatabaseURL+")")
	namespaceID := fs.String("namespace", "", "namespace to sweep, by id (defaults to "+envNamespace+")")
	apply := fs.Bool("apply", false, "actually expire the selected tasks; without it this is a dry run")
	limit := fs.Int("limit", 200, "maximum number of tasks to select")
	producer := fs.String("producer-actor-id", "",
		"registered identity the derived expiry record is written under (defaults to "+
			humanfanout.ExpiryProducerActorIDEnv+", then "+engine.HumanTaskExpiryActorID+")")
	var prs mergedPRFlag
	fs.Var(&prs, "pr", "a merged pull request as owner/repo#number, repeatable; without any, the delivered pr.merged facts decide")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			clifmt.EmitResult(explainExpireApprovals)
			return clifmt.ExitSuccess, nil
		}
		return 0, parseError("expire-approvals", err)
	}

	url := firstNonEmpty(*databaseURL, os.Getenv(envDatabaseURL))
	if url == "" {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     "no database URL configured",
			Remediation: "set " + envDatabaseURL + " or pass --database-url postgres://...",
		}
	}
	namespace := firstNonEmpty(*namespaceID, os.Getenv(envNamespace))
	if namespace == "" {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     "no namespace configured",
			Remediation: "set " + envNamespace + " or pass --namespace <id>",
		}
	}

	ctx := context.Background()
	db, err := postgres.Connect(ctx, url)
	if err != nil {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("connecting to the database: %v", err),
			Remediation: "verify " + envDatabaseURL + " is reachable and the credentials are correct",
		}
	}
	defer db.Close()

	eng, err := postgres.NewEngine(db, namespace)
	if err != nil {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("building the engine for namespace %s: %v", namespace, err),
			Remediation: "verify the namespace id exists in this database",
		}
	}

	ids, detail, err := selectApprovalsToExpire(ctx, db, eng, namespace, prs, *limit)
	if err != nil {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("selecting pending approvals: %v", err),
			Remediation: "verify the database is reachable and the namespace id is correct",
		}
	}

	producerID := firstNonEmpty(*producer, os.Getenv(humanfanout.ExpiryProducerActorIDEnv))
	payload := expireApprovalsPayload{NamespaceID: namespace, Applied: *apply, Selected: len(ids)}
	for _, id := range ids {
		task := expireApprovalsTask{HumanTaskID: id}
		if *apply {
			result, expireErr := eng.ExpireHumanTask(ctx, engine.ExpireHumanTaskRequest{
				HumanTaskID:     id,
				Reason:          engine.HumanTaskExpiryReasonPRMerged,
				Detail:          detail,
				ProducerActorID: producerID,
			})
			task.RunID, task.Outcome = result.RunID, result.Outcome
			if expireErr != nil {
				task.Error = expireErr.Error()
			} else {
				payload.Expired++
			}
		}
		payload.Tasks = append(payload.Tasks, task)
	}

	if jsonMode {
		if err := clifmt.EmitResultJSON(payload); err != nil {
			return 0, err
		}
	} else {
		clifmt.EmitResult(renderExpireApprovals(payload))
	}
	// A task that refused is a domain outcome, not an invocation failure:
	// results on stdout, exit 1, the same contract `nodes validate` and
	// `nodes chain-verify` follow for a failing check.
	for _, task := range payload.Tasks {
		if task.Error != "" {
			return clifmt.ExitUserError, nil
		}
	}
	return clifmt.ExitSuccess, nil
}

// selectApprovalsToExpire resolves the task ids and the provenance sentence
// that will be recorded on each expiry. The two selection modes are kept
// distinct all the way to the recorded detail, because they are backed by
// different evidence.
func selectApprovalsToExpire(ctx context.Context, db *postgres.Store, eng *engine.Engine, namespace string, prs mergedPRFlag, limit int) ([]string, string, error) {
	if len(prs) == 0 {
		ids, err := eng.PendingHumanTasksWithMergedPR(ctx, limit)
		return ids, "the pull request this approval is for is already merged (pr.merged fact)", err
	}
	seen := map[string]bool{}
	var ids []string
	for _, pr := range prs {
		found, err := db.PendingHumanTasksForPR(ctx, namespace, pr.Repository, pr.Number)
		if err != nil {
			return nil, "", err
		}
		for _, id := range found {
			if !seen[id] && len(ids) < limit {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids, "the pull request this approval is for was named as merged by an operator running " +
		"'nodes expire-approvals --pr " + prs.String() + "'", nil
}

func renderExpireApprovals(payload expireApprovalsPayload) string {
	var b strings.Builder
	if payload.Applied {
		fmt.Fprintf(&b, "expired %d of %d selected pending approval(s) in namespace %s\n",
			payload.Expired, payload.Selected, payload.NamespaceID)
	} else {
		fmt.Fprintf(&b, "dry run: %d pending approval(s) in namespace %s would be expired with reason %s\n"+
			"pass --apply to write\n",
			payload.Selected, payload.NamespaceID, engine.HumanTaskExpiryReasonPRMerged)
	}
	for _, task := range payload.Tasks {
		switch {
		case task.Error != "":
			fmt.Fprintf(&b, "  %s  REFUSED: %s\n", task.HumanTaskID, task.Error)
		case task.RunID != "":
			fmt.Fprintf(&b, "  %s  run %s -> %s\n", task.HumanTaskID, task.RunID, task.Outcome)
		default:
			fmt.Fprintf(&b, "  %s\n", task.HumanTaskID)
		}
	}
	return b.String()
}
