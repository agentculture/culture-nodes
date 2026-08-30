package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
)

// The store half of human-task fan-out and expiry (task t11, spec c6).
// internal/engine/humantaskfanout.go decides WHAT a task owes; this file
// writes it, transactionally, beside the row that created the obligation —
// the same discipline jiraticketreport.go follows for lifecycle comments.

// HumanTaskNotifierActorKey is the registered notify bridge a fan-out's
// Discord post is dispatched through: the same actor examples/notify-message
// names (`actor://company/notify-discord`), holding the webhook URL in its own
// environment so the control plane never sees it. Deployment must register
// this key for the notify channel to publish; an unregistered key fails the
// drain of that one row and leaves the rest of the queue alone.
const HumanTaskNotifierActorKey = "company/notify-discord"

// fanOutActorKey routes a channel to the bridge that can carry it. Both Jira
// channels go to the one narrow Jira actor: post_comment and transition_issue
// are two verbs on the same bridge, and the bridge's own allowlist — not this
// map — is what decides whether the transition is permitted.
func fanOutActorKey(channel string) string {
	if strings.HasPrefix(channel, "jira_") {
		return JiraTicketReporterActorKey
	}
	return HumanTaskNotifierActorKey
}

// EnqueueHumanTaskFanOut is engine.Tx's EnqueueHumanTaskFanOut: queue the
// messages this task owes, once.
//
// Idempotency is the ON CONFLICT clause, not a pre-read: two concurrent
// callers both plan the same three rows and both attempt the insert, and the
// UNIQUE (human_task_id, channel) index in migration 0051 decides which one
// wins each row. The returned count is how many rows THIS call inserted, so a
// second call for the same task returns 0 — which is the acceptance criterion
// "the same task twice emits nothing more", read straight off the write.
func (eq engineQueries) EnqueueHumanTaskFanOut(ctx context.Context, taskID string) (int, error) {
	task, err := eq.GetHumanTask(ctx, taskID)
	if err != nil {
		return 0, err
	}
	var input []byte
	err = eq.q.QueryRow(ctx, `SELECT COALESCE(input,'null'::jsonb) FROM runs WHERE id=$1 AND namespace_id=$2`,
		task.RunID, eq.namespaceID).Scan(&input)
	if isNoRows(err) {
		// A task whose run this namespace cannot see is not an error to
		// raise from inside the transaction that created it; it simply has
		// no subject to address.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("postgres: engine: human task fan-out: read run input: %w", err)
	}

	subject := engine.SubjectOfRunInput(input)
	queued := 0
	for _, intent := range engine.PlanHumanTaskFanOut(task, subject, engine.UIBaseURL()) {
		tag, err := eq.q.Exec(ctx, `INSERT INTO human_task_fanout_outbox
			(id,namespace_id,human_task_id,run_id,channel,target_actor_key,payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (human_task_id,channel) DO NOTHING`,
			store.NewULID(), eq.namespaceID, task.ID, task.RunID,
			intent.Channel, fanOutActorKey(intent.Channel), []byte(intent.Payload))
		if err != nil {
			return queued, fmt.Errorf("postgres: engine: human task fan-out %s/%s: %w", task.ID, intent.Channel, err)
		}
		queued += int(tag.RowsAffected())
	}
	return queued, nil
}

const markHumanTaskExpiredSQL = `
UPDATE human_tasks SET status = $2, response = $3, resolved_at = $4, expiry_reason = $5
WHERE id = $1 AND namespace_id = $6 AND status = $7
`

// MarkHumanTaskExpired is engine.Tx's MarkHumanTaskExpired. It repeats
// MarkHumanTaskDecided's status-guarded-WHERE shape deliberately rather than
// sharing it: the two write different columns and mean different things, and a
// shared helper parameterised by status would make "a decision and an expiry
// race, and exactly one wins" a property of an argument rather than of the
// statement.
func (eq engineQueries) MarkHumanTaskExpired(ctx context.Context, id, reason string, response json.RawMessage, resolvedAt time.Time) (bool, error) {
	tag, err := eq.q.Exec(ctx, markHumanTaskExpiredSQL,
		id, engine.HumanTaskStatusExpired, []byte(jsonOrEmptyObject(response)), tsOrNow(resolvedAt),
		textOrNull(reason), eq.namespaceID, engine.HumanTaskStatusPending,
	)
	if err != nil {
		return false, fmt.Errorf("postgres: engine: MarkHumanTaskExpired: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// pendingHumanTasksWithMergedPRSQL finds the tasks this task exists to stop
// accumulating: a pending decision on a run whose subject pull request has
// already been merged.
//
// The join is on the FACT, not on GitHub. signal_events holds the sweep's
// delivered `pr.merged` facts (examples/pr-upkeep/sweep.py's merged_pr_fact),
// so this query asks "has this control plane been TOLD the PR merged", which
// is the only question it can answer without a credential. A merged PR the
// sweep never emitted a fact for — because merged_pr_fact requires a
// correlatable Jira key in the branch or body — is invisible here by
// construction; the backfill command exists for exactly those.
const pendingHumanTasksWithMergedPRSQL = `
SELECT ht.id
FROM human_tasks ht
JOIN runs r ON r.id = ht.run_id AND r.namespace_id = ht.namespace_id
WHERE ht.namespace_id = $1
  AND ht.status = $2
  AND r.input->>'source' = 'github_pr'
  AND EXISTS (
    SELECT 1 FROM signal_events se
    WHERE se.namespace_id = ht.namespace_id
      AND se.name = 'pr.merged'
      AND se.payload->>'repository' = r.input->>'repository'
      AND se.payload->>'number' = r.input->>'number')
ORDER BY ht.created_at, ht.id
LIMIT $3
`

// PendingHumanTasksWithMergedPR is engine.Tx's method of the same name.
func (eq engineQueries) PendingHumanTasksWithMergedPR(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := eq.q.Query(ctx, pendingHumanTasksWithMergedPRSQL, eq.namespaceID, engine.HumanTaskStatusPending, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: PendingHumanTasksWithMergedPR: %w", err)
	}
	ids, err := scanIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: PendingHumanTasksWithMergedPR: %w", err)
	}
	return ids, nil
}

// namespacesWithMergedPRHumanTasksSQL is the deployment-wide half of the same
// question PendingHumanTasksWithMergedPR asks per namespace: which namespaces
// have anything to expire at all. The periodic consumer needs it because an
// engine is namespace-scoped (NewEngine binds one namespace's contracts and
// store queries) while the scheduler is not, and building an engine for every
// registered namespace on every tick to discover that none of them has work
// would be the expensive way to learn nothing.
const namespacesWithMergedPRHumanTasksSQL = `
SELECT DISTINCT ht.namespace_id
FROM human_tasks ht
JOIN runs r ON r.id = ht.run_id AND r.namespace_id = ht.namespace_id
WHERE ht.status = $1
  AND r.input->>'source' = 'github_pr'
  AND EXISTS (
    SELECT 1 FROM signal_events se
    WHERE se.namespace_id = ht.namespace_id
      AND se.name = 'pr.merged'
      AND se.payload->>'repository' = r.input->>'repository'
      AND se.payload->>'number' = r.input->>'number')
`

// NamespacesWithMergedPRHumanTasks lists the namespaces holding at least one
// pending human task whose subject pull request has already merged.
func (s *Store) NamespacesWithMergedPRHumanTasks(ctx context.Context) ([]string, error) {
	rows, err := s.Pool().Query(ctx, namespacesWithMergedPRHumanTasksSQL, engine.HumanTaskStatusPending)
	if err != nil {
		return nil, fmt.Errorf("postgres: NamespacesWithMergedPRHumanTasks: %w", err)
	}
	ids, err := scanIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("postgres: NamespacesWithMergedPRHumanTasks: %w", err)
	}
	return ids, nil
}

// pendingHumanTasksForPRSQL is the backfill's query, and it deliberately does
// NOT consult signal_events. The 26 stale approvals this task exists to clear
// belong to pull requests the sweep never emitted a pr.merged fact for --
// merged_pr_fact only emits when the branch or body carries a correlatable
// Jira key, and those PRs carried none. So the periodic consumer, which can
// only act on facts the control plane holds, cannot see them at all.
//
// The backfill closes that gap by making the OPERATOR the source of the
// merge fact: they name the pull request, and the recorded expiry detail says
// so. That is a weaker provenance than a delivered fact and it is recorded as
// such rather than dressed up as one.
const pendingHumanTasksForPRSQL = `
SELECT ht.id
FROM human_tasks ht
JOIN runs r ON r.id = ht.run_id AND r.namespace_id = ht.namespace_id
WHERE ht.namespace_id = $1
  AND ht.status = $2
  AND r.input->>'source' = 'github_pr'
  AND r.input->>'repository' = $3
  AND r.input->>'number' = $4
ORDER BY ht.created_at, ht.id
`

// PendingHumanTasksForPR lists pending human tasks whose run's subject is
// exactly this pull request.
func (s *Store) PendingHumanTasksForPR(ctx context.Context, namespaceID, repository string, number int) ([]string, error) {
	rows, err := s.Pool().Query(ctx, pendingHumanTasksForPRSQL,
		namespaceID, engine.HumanTaskStatusPending, repository, strconv.Itoa(number))
	if err != nil {
		return nil, fmt.Errorf("postgres: PendingHumanTasksForPR: %w", err)
	}
	ids, err := scanIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("postgres: PendingHumanTasksForPR: %w", err)
	}
	return ids, nil
}
