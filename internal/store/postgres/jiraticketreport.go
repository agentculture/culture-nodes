package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (run_id,phase) WHERE run_id IS NOT NULL DO NOTHING`,
		store.NewULID(), eq.namespaceID, runID, triggerEventID, phase, JiraTicketReporterActorKey, issue, reportPayload); err != nil {
		return fmt.Errorf("postgres: engine: jira ticket report outbox: %w", err)
	}
	if phase == "start" {
		pagePayload, _ := json.Marshal(map[string]any{
			"verb": "post_comment", "issue": issue, "comment": jiraTicketPageComment(issue), "phase": "page-link",
		})
		if _, err := eq.q.Exec(ctx, `INSERT INTO jira_ticket_report_outbox
			(id,namespace_id,phase,target_actor_key,issue_key,payload)
			VALUES ($1,$2,'page-link',$3,$4,$5)
			ON CONFLICT (namespace_id,issue_key,phase) WHERE phase='page-link' DO NOTHING`,
			store.NewULID(), eq.namespaceID, JiraTicketReporterActorKey, issue, pagePayload); err != nil {
			return fmt.Errorf("postgres: engine: jira ticket page-link outbox: %w", err)
		}
	}
	_ = eventPayload // lifecycle event remains the authoritative ordering source.
	return nil
}

// UIBaseURLEnv names the origin the ticket page is served from -- the value
// that decides whether the link this file posts to a Jira ticket is clickable.
// It is exported so the deployment manifests can be pinned against it from
// tests/deploy (the same cross-boundary pin telemetry.EndpointEnvVar carries):
// the variable is useless unless every compose service that can mint a run
// declares it, and a rename in either half must fail the build rather than
// quietly restore the bare path.
const UIBaseURLEnv = "NODES_UI_BASE_URL"

// jiraTicketPageComment renders the one page-link comment a ticket gets, from
// the deployment's own origin.
//
// Trimming is not cosmetic: this value is read from ~/.culture-nodes/prod.env,
// where a trailing slash is what an operator types and surrounding whitespace
// is what a hand-edited line leaves behind -- either one concatenated naively
// gives a URL that is wrong in a way nothing downstream reports.
//
// An unset (or empty) variable renders the BARE PATH, deliberately. A default
// invented here would be a second, unmanaged opinion about where this
// deployment is served from, sitting in a code path no deploy can correct; the
// compose files carry the fallback origin instead, where an operator can see
// and override it. Task t16 / spec c10.
func jiraTicketPageComment(issue string) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv(UIBaseURLEnv)), "/")
	return fmt.Sprintf("culture-nodes page: %s/tickets/%s [culture-nodes:ticket-page-link]", base, issue)
}
