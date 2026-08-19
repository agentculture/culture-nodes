// Package ticketreport drains engine-authored Jira lifecycle reports through
// the registered narrow actor bridge. It has no Jira credentials and no Jira
// HTTP vocabulary beyond the bridge's post_comment input.
package ticketreport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// A failing report is backed off and eventually dead-lettered instead of
// staying the eligible head of the queue: the drain selects the globally
// oldest eligible row, so a poison row that kept status='pending' and a past
// available_at would block every later report -- including other
// namespaces' -- on each scheduler tick.
const (
	maxAttempts = 5
	baseBackoff = time.Minute
	maxBackoff  = 15 * time.Minute
)

type Dispatcher struct {
	store  *postgres.Store
	client *actors.Client
}

func New(store *postgres.Store, client *actors.Client) *Dispatcher {
	if client == nil {
		client = actors.NewClient()
	}
	return &Dispatcher{store: store, client: client}
}

// Run drains every currently eligible report. Rows are locked and marked only
// after the actor accepts the invocation; the stable row id is the actor
// protocol idempotency key, so a crash after POST is an at-least-once retry
// of the same logical comment. A report whose dispatch fails is recorded
// (attempts+1, bounded-backoff available_at, status='failed' once maxAttempts
// is spent) and the drain continues with the next eligible row; those
// per-row failures are joined into the returned error for observability.
func (d *Dispatcher) Run(ctx context.Context) error {
	var errs []error
	for {
		tx, err := d.store.Pool().Begin(ctx)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		var id, namespaceID, runID, triggerID, actorKey string
		var payload []byte
		var attempts int
		err = tx.QueryRow(ctx, `SELECT id,namespace_id,run_id,trigger_event_id,target_actor_key,payload,attempts
			FROM jira_ticket_report_outbox WHERE status='pending' AND available_at<=now()
			ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id, &namespaceID, &runID, &triggerID, &actorKey, &payload, &attempts)
		if err == pgx.ErrNoRows {
			_ = tx.Rollback(ctx)
			return errors.Join(errs...)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return errors.Join(append(errs, fmt.Errorf("ticketreport: select: %w", err))...)
		}
		if dispatchErr := d.dispatch(ctx, id, namespaceID, runID, triggerID, actorKey, payload); dispatchErr != nil {
			if err := recordFailure(ctx, tx, id, attempts+1); err != nil {
				_ = tx.Rollback(ctx)
				return errors.Join(append(errs, fmt.Errorf("ticketreport: record failure: %w", err))...)
			}
			if err := tx.Commit(ctx); err != nil {
				return errors.Join(append(errs, err)...)
			}
			errs = append(errs, fmt.Errorf("ticketreport: report %s: %w", id, dispatchErr))
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE jira_ticket_report_outbox SET status='published',published_at=now(),attempts=attempts+1 WHERE id=$1`, id); err != nil {
			_ = tx.Rollback(ctx)
			return errors.Join(append(errs, err)...)
		}
		if err := tx.Commit(ctx); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
}

// dispatch performs the side-effecting half of one report: decode the queued
// payload, resolve the target bridge in the report's own namespace, and POST
// the invocation. Any error here is a per-row delivery failure for Run to
// record, never a reason to abandon the rest of the queue.
func (d *Dispatcher) dispatch(ctx context.Context, id, namespaceID, runID, triggerID, actorKey string, payload []byte) error {
	var full struct{ Issue, Comment string }
	if err := json.Unmarshal(payload, &full); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	input, _ := json.Marshal(map[string]string{"verb": "post_comment", "issue": full.Issue, "comment": full.Comment})
	registry, err := worker.NewDBRegistry(d.store, namespaceID)
	if err != nil {
		return err
	}
	endpoint, err := registry.Resolve(ctx, actorKey)
	if err != nil {
		return fmt.Errorf("resolve bridge: %w", err)
	}
	_, err = d.client.Invoke(ctx, endpoint, actors.InvocationRequest{
		ProtocolVersion: actors.ProtocolVersion, RunID: runID, TokenID: triggerID,
		NodeRunID: "ticket-report-" + id, AttemptID: id, Attempt: 1,
		Workflow: actors.WorkflowRef{Name: "jira-ticket-report"},
		Node:     actors.NodeRef{ID: "post-comment"}, Input: input,
		ArtifactRefs: []string{}, ContextRefs: []string{},
	})
	if err != nil {
		return fmt.Errorf("invoke bridge: %w", err)
	}
	return nil
}

// recordFailure moves a failed report out of the eligible set on the row
// still locked by the caller's transaction: exponential backoff on
// available_at bounded by maxBackoff, then a terminal 'failed' status once
// maxAttempts is spent.
func recordFailure(ctx context.Context, tx pgx.Tx, id string, attempts int) error {
	if attempts >= maxAttempts {
		_, err := tx.Exec(ctx, `UPDATE jira_ticket_report_outbox SET status='failed',attempts=$2 WHERE id=$1`, id, attempts)
		return err
	}
	backoff := baseBackoff << (attempts - 1)
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	_, err := tx.Exec(ctx, `UPDATE jira_ticket_report_outbox SET attempts=$2,available_at=now()+($3*interval '1 second') WHERE id=$1`,
		id, attempts, int(backoff.Seconds()))
	return err
}
