// Package humanfanout is the periodic half of the human-task lane (task t11,
// spec c6): it delivers the fan-out messages a newly created human task owes,
// and it expires the tasks whose subject pull request has already merged.
//
// # Why one package for two jobs
//
// They are the two ends of one promise — "a pending decision reaches a person,
// and stops asking once it is moot". Both are periodic, both are safe to
// re-run, and both belong to human tasks rather than to run execution. Wiring
// them as one scheduler hook keeps the scheduler's Tick from growing a second
// nil-checked interface for the same lane.
//
// # Why there is no GitHub PR comment
//
// spec c6 asks a PR-sourced run's human task to reach the pull request too.
// Nothing in this repo can post there, and this is the audit behind that
// statement rather than an assumption:
//
//   - adapters/human-inbox READS a PR thread (fetch_github_issue_comments,
//     fetch_github_pull) and writes only to its own
//     /inbox/tasks/<id>/submit surface -- it holds GITHUB_TOKEN for reads;
//   - adapters/claude-code and adapters/codex run agent sessions and
//     advertise no GitHub-writing verb on /v1/capabilities at all;
//   - adapters/jira and adapters/notify are Jira and Discord only.
//
// So a PR-sourced task fans out to notify alone (engine's
// NoGitHubPRCommentReason states the same thing provider-neutrally, because
// that package may not name providers -- PRD §9.5). Adding the channel is a
// deliberate later change once a bridge advertises the capability: a new
// migration widening migration 0051's CHECK constraint, and a branch in
// engine.PlanHumanTaskFanOut.
//
// # No Jira, GitHub or Discord vocabulary
//
// Like internal/ticketreport, this package holds no credential for anything it
// talks to and knows no third-party API. It resolves the registered bridge for
// the row's target_actor_key and POSTs the payload the engine composed,
// verbatim. The Jira bridge decides whether a transition target is
// allowlisted; the notify bridge holds the webhook URL. Neither secret is ever
// in this process.
package humanfanout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// A failing fan-out is backed off and eventually dead-lettered rather than
// left as the eligible head of the queue, for the reason internal/ticketreport
// gives at length: the drain takes the globally oldest eligible row, so one
// poison row that kept status='pending' with a past available_at would block
// every later message, including other namespaces'.
const (
	maxAttempts = 5
	baseBackoff = time.Minute
	maxBackoff  = 15 * time.Minute

	// ExpiryProducerActorIDEnv overrides the registered identity an expiry's
	// derived record is written under, the same way
	// NODES_REMINT_PRODUCER_ACTOR_ID overrides the re-mint scheduler's. Unset
	// selects engine.HumanTaskExpiryActorID, which the deployment must have
	// registered (see that constant's doc comment).
	ExpiryProducerActorIDEnv = "NODES_HUMAN_TASK_EXPIRY_ACTOR_ID"

	// expiryBatch bounds one tick's expiries per namespace. Each expiry takes
	// its run's advisory lock and routes a graph, so this is a real cost; the
	// scan is re-run every tick, so a backlog drains over several ticks
	// rather than in one long transaction.
	expiryBatch = 50
)

// Lane drains the human-task fan-out outbox and expires merged-PR approvals.
type Lane struct {
	store  *postgres.Store
	client *actors.Client

	// engines caches one namespace-scoped engine per namespace, the same way
	// internal/scheduler does: NewEngine binds a namespace's contracts
	// validator and store queries, and rebuilding one per tick would recompile
	// that for nothing.
	engineMu sync.Mutex
	engines  map[string]*engine.Engine
}

func New(store *postgres.Store, client *actors.Client) *Lane {
	if client == nil {
		client = actors.NewClient()
	}
	return &Lane{store: store, client: client, engines: map[string]*engine.Engine{}}
}

// Run performs one tick of the lane. The two halves are independent: an
// unreachable bridge must not stop stale approvals expiring, and a run that
// refuses expiry must not stop the queue draining. Both halves' errors are
// joined and returned together.
func (l *Lane) Run(ctx context.Context) error {
	return errors.Join(l.drain(ctx), l.expireMergedPR(ctx))
}

// drain publishes every currently eligible fan-out row. Rows are locked and
// marked only after the bridge accepts the invocation; the stable row id is
// the actor protocol idempotency key, so a crash after POST is an
// at-least-once retry of the same logical message rather than a second one.
func (l *Lane) drain(ctx context.Context) error {
	var errs []error
	for {
		tx, err := l.store.Pool().Begin(ctx)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		var id, namespaceID, taskID, runID, channel, actorKey string
		var payload []byte
		var attempts int
		err = tx.QueryRow(ctx, `SELECT id,namespace_id,human_task_id,run_id,channel,target_actor_key,payload,attempts
			FROM human_task_fanout_outbox
			WHERE status='pending' AND available_at<=now()
			ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`).
			Scan(&id, &namespaceID, &taskID, &runID, &channel, &actorKey, &payload, &attempts)
		if err == pgx.ErrNoRows {
			_ = tx.Rollback(ctx)
			return errors.Join(errs...)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return errors.Join(append(errs, fmt.Errorf("humanfanout: select: %w", err))...)
		}
		if dispatchErr := l.dispatch(ctx, dispatchRow{
			id: id, namespaceID: namespaceID, taskID: taskID, runID: runID,
			channel: channel, actorKey: actorKey, payload: payload,
		}); dispatchErr != nil {
			if err := recordFailure(ctx, tx, id, attempts+1); err != nil {
				_ = tx.Rollback(ctx)
				return errors.Join(append(errs, fmt.Errorf("humanfanout: record failure: %w", err))...)
			}
			if err := tx.Commit(ctx); err != nil {
				return errors.Join(append(errs, err)...)
			}
			errs = append(errs, fmt.Errorf("humanfanout: message %s (%s): %w", id, channel, dispatchErr))
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE human_task_fanout_outbox
			SET status='published',published_at=now(),attempts=attempts+1 WHERE id=$1`, id); err != nil {
			_ = tx.Rollback(ctx)
			return errors.Join(append(errs, err)...)
		}
		if err := tx.Commit(ctx); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
}

type dispatchRow struct {
	id, namespaceID, taskID, runID, channel, actorKey string
	payload                                           []byte
}

// dispatch performs the side-effecting half of one message: resolve the target
// bridge in the row's own namespace and POST the payload the engine composed.
//
// The payload is passed through UNCHANGED. internal/ticketreport re-builds its
// invocation input from two decoded fields, which is why it can only ever send
// post_comment; this lane carries three different verbs across two bridges, so
// re-deriving the input here would mean this package learning each bridge's
// vocabulary — exactly the coupling the outbox exists to avoid.
func (l *Lane) dispatch(ctx context.Context, row dispatchRow) error {
	if !json.Valid(row.payload) {
		return errors.New("queued payload is not valid JSON")
	}
	registry, err := worker.NewDBRegistry(l.store, row.namespaceID)
	if err != nil {
		return err
	}
	endpoint, err := registry.Resolve(ctx, row.actorKey)
	if err != nil {
		return fmt.Errorf("resolve bridge %s: %w", row.actorKey, err)
	}
	_, err = l.client.Invoke(ctx, endpoint, actors.InvocationRequest{
		ProtocolVersion: actors.ProtocolVersion,
		RunID:           row.runID,
		TokenID:         row.taskID,
		NodeRunID:       "human-task-fanout-" + row.taskID,
		AttemptID:       row.id,
		Attempt:         1,
		Workflow:        actors.WorkflowRef{Name: "human-task-fanout"},
		Node:            actors.NodeRef{ID: row.channel},
		Input:           json.RawMessage(row.payload),
		ArtifactRefs:    []string{},
		ContextRefs:     []string{},
	})
	if err != nil {
		return fmt.Errorf("invoke bridge: %w", err)
	}
	return nil
}

// recordFailure moves a failed message out of the eligible set on the row the
// caller's transaction still holds: exponential backoff bounded by maxBackoff,
// then a terminal 'failed' status once maxAttempts is spent.
func recordFailure(ctx context.Context, tx pgx.Tx, id string, attempts int) error {
	if attempts >= maxAttempts {
		_, err := tx.Exec(ctx, `UPDATE human_task_fanout_outbox SET status='failed',attempts=$2 WHERE id=$1`, id, attempts)
		return err
	}
	backoff := baseBackoff << (attempts - 1)
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	_, err := tx.Exec(ctx, `UPDATE human_task_fanout_outbox
		SET attempts=$2,available_at=now()+($3*interval '1 second') WHERE id=$1`,
		id, attempts, int(backoff.Seconds()))
	return err
}

// expireMergedPR is the pr.merged consumer: for every namespace holding a
// pending human task whose subject PR has already merged, expire those tasks
// with reason pr_merged and let each run route its `expired` edge.
//
// A task that refuses (its node declares no `expired` outcome, its run is
// already terminal, a human decided it between the scan and the write) is
// joined into the returned error and the rest continue — a refusal is
// information, not a reason to stop clearing the backlog.
func (l *Lane) expireMergedPR(ctx context.Context) error {
	namespaces, err := l.store.NamespacesWithMergedPRHumanTasks(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, namespaceID := range namespaces {
		eng, err := l.engineFor(namespaceID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		expired, err := eng.ExpirePendingTasksForMergedPR(ctx, expiryBatch, os.Getenv(ExpiryProducerActorIDEnv))
		if err != nil {
			errs = append(errs, fmt.Errorf("humanfanout: expire in namespace %s: %w", namespaceID, err))
			continue
		}
		for _, one := range expired {
			if one.Err != nil {
				errs = append(errs, fmt.Errorf("humanfanout: expire human task %s: %w", one.HumanTaskID, one.Err))
			}
		}
	}
	return errors.Join(errs...)
}

func (l *Lane) engineFor(namespaceID string) (*engine.Engine, error) {
	l.engineMu.Lock()
	defer l.engineMu.Unlock()
	if eng, ok := l.engines[namespaceID]; ok {
		return eng, nil
	}
	eng, err := postgres.NewEngine(l.store, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("humanfanout: build engine for namespace %s: %w", namespaceID, err)
	}
	l.engines[namespaceID] = eng
	return eng, nil
}
