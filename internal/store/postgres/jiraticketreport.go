package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
)

// appendJiraTicketReport is the #198 start/finish seam: when a lifecycle
// event belongs to a run minted from a jira-sourced trigger fact, it queues
// a post_comment intent for the narrow Jira actor bridge in the same
// transaction. Non-jira source keys (and runs with no trigger event) fall
// through without a row. Split out of engine_store.go at the WP-D merge
// gate to keep that file inside the t4 line limit.
func (eq engineQueries) appendJiraTicketReport(ctx context.Context, runID, eventType string, eventPayload json.RawMessage) error {
	phase := ""
	switch eventType {
	case engine.TypeRunCreated:
		phase = "start"
	case engine.TypeRunCompleted, engine.TypeRunFailed, engine.TypeRunCancelled, engine.TypeRunBounded:
		phase = "finish"
	default:
		return nil
	}
	var triggerEventID, sourceKey, workflowKey, outcome string
	err := eq.q.QueryRow(ctx, `SELECT r.trigger_event_id, COALESCE(se.source_key,''), wv.workflow_key, r.status
		FROM runs r JOIN workflow_versions wv ON wv.id=r.workflow_version_id
		JOIN signal_events se ON se.id=r.trigger_event_id
		WHERE r.id=$1 AND r.namespace_id=$2`, runID, eq.namespaceID).
		Scan(&triggerEventID, &sourceKey, &workflowKey, &outcome)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("postgres: engine: jira ticket report provenance: %w", err)
	}
	parts := strings.Split(sourceKey, ":")
	if len(parts) < 4 || parts[0] != "jira" || parts[1] == "" || parts[2] == "" {
		return nil
	}
	issue := parts[2]
	comment := fmt.Sprintf("culture-nodes started run %s (workflow %s, trigger event %s)", runID, workflowKey, triggerEventID)
	if phase == "finish" {
		comment = fmt.Sprintf("culture-nodes finished run %s with outcome %s", runID, outcome)
	}
	reportPayload, _ := json.Marshal(map[string]any{
		"verb": "post_comment", "issue": issue, "comment": comment,
		"run_id": runID, "workflow": workflowKey, "trigger_event_id": triggerEventID,
		"phase": phase, "outcome": outcome,
	})
	if _, err := eq.q.Exec(ctx, `INSERT INTO jira_ticket_report_outbox
		(id,namespace_id,run_id,trigger_event_id,phase,target_actor_key,issue_key,payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (run_id,phase) DO NOTHING`,
		store.NewULID(), eq.namespaceID, runID, triggerEventID, phase, JiraTicketReporterActorKey, issue, reportPayload); err != nil {
		return fmt.Errorf("postgres: engine: jira ticket report outbox: %w", err)
	}
	_ = eventPayload // lifecycle event remains the authoritative ordering source.
	return nil
}
