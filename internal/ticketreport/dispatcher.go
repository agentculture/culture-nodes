// Package ticketreport drains engine-authored Jira lifecycle reports through
// the registered narrow actor bridge. It has no Jira credentials and no Jira
// HTTP vocabulary beyond the bridge's post_comment input.
package ticketreport

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
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

// Run drains every currently pending report. Rows are locked and marked only
// after the actor accepts the invocation; the stable row id is the actor
// protocol idempotency key, so a crash after POST is an at-least-once retry
// of the same logical comment.
func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		tx, err := d.store.Pool().Begin(ctx)
		if err != nil {
			return err
		}
		var id, namespaceID, runID, triggerID, actorKey string
		var payload []byte
		err = tx.QueryRow(ctx, `SELECT id,namespace_id,run_id,trigger_event_id,target_actor_key,payload
			FROM jira_ticket_report_outbox WHERE status='pending' AND available_at<=now()
			ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id, &namespaceID, &runID, &triggerID, &actorKey, &payload)
		if err == pgx.ErrNoRows {
			_ = tx.Rollback(ctx)
			return nil
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("ticketreport: select: %w", err)
		}
		var full struct{ Issue, Comment string }
		if err := json.Unmarshal(payload, &full); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		input, _ := json.Marshal(map[string]string{"verb": "post_comment", "issue": full.Issue, "comment": full.Comment})
		registry, err := worker.NewDBRegistry(d.store, namespaceID)
		if err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		endpoint, err := registry.Resolve(ctx, actorKey)
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("ticketreport: resolve bridge: %w", err)
		}
		_, err = d.client.Invoke(ctx, endpoint, actors.InvocationRequest{
			ProtocolVersion: actors.ProtocolVersion, RunID: runID, TokenID: triggerID,
			NodeRunID: "ticket-report-" + id, AttemptID: id, Attempt: 1,
			Workflow: actors.WorkflowRef{Name: "jira-ticket-report"},
			Node:     actors.NodeRef{ID: "post-comment"}, Input: input,
			ArtifactRefs: []string{}, ContextRefs: []string{},
		})
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("ticketreport: invoke bridge: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE jira_ticket_report_outbox SET status='published',published_at=now(),attempts=attempts+1 WHERE id=$1`, id); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
}
