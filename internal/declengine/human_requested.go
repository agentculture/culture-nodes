package declengine

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/agentculture/culture-nodes/internal/store"
)

// HumanRequestedEvent is the pending human.ask task announced to declarations.
type HumanRequestedEvent struct {
	RunID, TaskID, NodeName, EventID string
}

// HumanRequestedVariables is the variables a human.requested event hands its
// declarations. Exported (task t48) so a template test renders against the
// shape the engine emits rather than a copy of it. firing_id is the human.ask
// firing, whose run the UI opens at /runs/{firing_id}.
func HumanRequestedVariables(event HumanRequestedEvent) map[string]any {
	return map[string]any{"human_task_id": event.TaskID, "firing_id": event.RunID, "node_name": event.NodeName}
}

// HumanRequestedBackend is optional, like NodeBackend and ReactionBackend.
type HumanRequestedBackend interface {
	EmitHumanRequested(context.Context, string, int) ([]HumanRequestedEvent, error)
}

// EmitHumanRequested offers each newly pending declaration approval task at
// its human.ask landing node. The fact is emitted once per task, before a
// decision can consume that landing node in a later driver pass.
func (e *Engine) EmitHumanRequested(ctx context.Context, namespaceID string, limit int) ([]HumanRequestedEvent, error) {
	b, ok := e.backend.(HumanRequestedBackend)
	if !ok {
		return nil, nil
	}
	events, err := b.EmitHumanRequested(ctx, namespaceID, limit)
	if err != nil {
		return events, err
	}
	var failures []error
	for _, event := range events {
		if err := e.Handle(ctx, Event{NamespaceID: namespaceID, ID: event.EventID, Kind: "human.requested", Node: event.NodeName,
			Variables: HumanRequestedVariables(event), Emitter: DeclarationEngineActorID}); err != nil {
			failures = append(failures, err)
		}
	}
	return events, errors.Join(failures...)
}

const humanRequestedCandidatesSQL = `SELECT ht.id,ht.run_id,dn.node_name FROM human_tasks ht
	JOIN declaration_firings f ON f.namespace_id=ht.namespace_id AND f.id=ht.run_id
	JOIN declaration_versions v ON v.namespace_id=f.namespace_id AND v.id=f.declaration_version
	JOIN declaration_nodes dn ON dn.namespace_id=f.namespace_id AND dn.opening_firing_id=f.id
	WHERE ht.namespace_id=$1 AND ht.status='pending' AND v.body->'action'->>'kind'='human.ask'
	AND NOT EXISTS (SELECT 1 FROM signal_events se WHERE se.namespace_id=ht.namespace_id AND se.run_id=ht.run_id
		AND se.name='human.requested' AND se.payload->>'human_task_id'=ht.id)
	ORDER BY ht.id LIMIT $2`

func (p PostgresBackend) EmitHumanRequested(ctx context.Context, namespaceID string, limit int) ([]HumanRequestedEvent, error) {
	if limit <= 0 {
		limit = DefaultDriverBatch
	}
	rows, err := p.Store.Pool().Query(ctx, humanRequestedCandidatesSQL, namespaceID, limit)
	if err != nil {
		return nil, err
	}
	var due []HumanRequestedEvent
	for rows.Next() {
		var event HumanRequestedEvent
		if err := rows.Scan(&event.TaskID, &event.RunID, &event.NodeName); err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, event)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var out []HumanRequestedEvent
	for _, event := range due {
		tx, err := p.Store.Pool().Begin(ctx)
		if err != nil {
			return out, err
		}
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "decl-human-requested:"+namespaceID+":"+event.TaskID); err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		var pending bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM human_tasks ht WHERE ht.namespace_id=$1 AND ht.id=$2 AND ht.status='pending'
			AND NOT EXISTS (SELECT 1 FROM signal_events se WHERE se.namespace_id=ht.namespace_id AND se.run_id=ht.run_id
			AND se.name='human.requested' AND se.payload->>'human_task_id'=ht.id))`, namespaceID, event.TaskID).Scan(&pending)
		if err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if !pending {
			_ = tx.Rollback(ctx)
			continue
		}
		event.EventID = store.NewULID()
		payload, err := json.Marshal(HumanRequestedVariables(event))
		if err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO signal_events(id,namespace_id,run_id,name,payload,emitter) VALUES($1,$2,$3,'human.requested',$4,$5)`, event.EventID, namespaceID, event.RunID, payload, DeclarationEngineActorID); err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if err = tx.Commit(ctx); err != nil {
			return out, err
		}
		out = append(out, event)
	}
	return out, nil
}
