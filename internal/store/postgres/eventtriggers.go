package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// runEventTriggers offers an appended fact to the newest immutable version
// of each published workflow. Older versions are deliberately inactive: a
// replacement publication that removes a trigger must stop future starts.
func runEventTriggers(ctx context.Context, tx pgx.Tx, namespaceID string, runner engine.EventTriggerRunner, ev SignalEvent) ([]engine.TriggeredRun, error) {
	if runner == nil || ev.RunID != "" {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (workflow_key) content_digest, source_format, source, normalized_ir
		FROM workflow_versions
		WHERE namespace_id = $1
		ORDER BY workflow_key, version DESC`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("postgres: DeliverSignalEvent: list trigger workflows: %w", err)
	}
	var candidates []engine.TriggerWorkflow
	for rows.Next() {
		var candidate engine.TriggerWorkflow
		if err := rows.Scan(&candidate.Digest, &candidate.SourceFormat, &candidate.Source, &candidate.IR); err != nil {
			return nil, fmt.Errorf("postgres: DeliverSignalEvent: scan trigger workflow: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("postgres: DeliverSignalEvent: list trigger workflows: %w", err)
	}
	rows.Close()
	inner := &engineTx{engineQueries: engineQueries{q: tx, namespaceID: namespaceID}}
	fact := engine.PickupEvent{ID: ev.ID, Name: ev.Name, Emitter: ev.Emitter, Payload: ev.Payload, Subject: ev.Subject}
	var out []engine.TriggeredRun
	for _, candidate := range candidates {
		created, err := runner.TriggerEvent(ctx, inner, candidate, fact)
		if err != nil {
			return nil, fmt.Errorf("postgres: DeliverSignalEvent: trigger workflow %s: %w", candidate.Digest, err)
		}
		out = append(out, created...)
	}
	return out, nil
}
