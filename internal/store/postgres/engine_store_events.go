package postgres

// Run-event and per-node evidence reads, split out of engine_store.go when the
// t15 board-move work pushed that file past the tests/lint 1000-line guard.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
)

// TransitionCount is how many transitions the run has taken, derived as
// "node runs created after entry": every transition creates exactly one node
// run — a K-way split is K transitions creating K node runs — with one
// documented exception (parallel-tokens design §5.2): join arrivals after
// the first create no node run, so a K-way join contributes K transitions'
// worth of routing but only 1 to this count. That undercount is intentional
// — an arrival does no dispatchable work, and the §9.7 limit exists to
// bound work.
func (eq engineQueries) TransitionCount(ctx context.Context, runID string) (int, error) {
	var count int32
	err := eq.q.QueryRow(ctx,
		`SELECT GREATEST(COUNT(*) - 1, 0)::int FROM node_runs WHERE run_id = $1`, runID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("postgres: engine: TransitionCount: %w", err)
	}
	return int(count), nil
}

// NodeVisits is how many node runs each node has in this run.
func (eq engineQueries) NodeVisits(ctx context.Context, runID string) (map[string]int, error) {
	rows, err := eq.q.Query(ctx,
		`SELECT node_key, COUNT(*)::int FROM node_runs WHERE run_id = $1 GROUP BY node_key`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: NodeVisits: %w", err)
	}
	defer rows.Close()

	visits := make(map[string]int)
	for rows.Next() {
		var (
			nodeKey string
			count   int32
		)
		if err := rows.Scan(&nodeKey, &count); err != nil {
			return nil, fmt.Errorf("postgres: engine: NodeVisits: scan: %w", err)
		}
		visits[nodeKey] = int(count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: engine: NodeVisits: %w", err)
	}
	return visits, nil
}

// NodeOutput returns the output of a node's most recent succeeded attempt in
// this run — what `/nodes/<id>/output` resolves to. Only a succeeded attempt
// counts: a failed or contract-rejected attempt produced no output a binding
// may treat as the node's answer.
func (eq engineQueries) NodeOutput(ctx context.Context, runID, nodeID string) (json.RawMessage, error) {
	var result []byte
	err := eq.q.QueryRow(ctx, `
		SELECT a.result
		FROM attempts AS a
		JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = $2 AND a.status = 'succeeded'
		ORDER BY a.completed_at DESC, a.id DESC
		LIMIT 1
	`, runID, nodeID).Scan(&result)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("postgres: engine: NodeOutput: %w", err)
	}
	return json.RawMessage(result), nil
}

// NodeEvidence returns the run's live evidence ledger records belonging to a
// node's node runs, in id order — what `/nodes/<id>/evidence` resolves to.
// It is queryNodeEvidence, the same statement Store.NodeEvidence runs, so a
// worker resolving bindings outside an engine transaction gets the same
// answer the engine's end-node output binding gets inside one.
func (eq engineQueries) NodeEvidence(ctx context.Context, runID, nodeID string) ([]ledger.Record, error) {
	return queryNodeEvidence(ctx, eq.q, runID, nodeID)
}

const insertEngineEventSQL = `
INSERT INTO events (id, namespace_id, aggregate_type, aggregate_id, sequence, event_type, source, data, occurred_at)
VALUES ($1, $2, 'run', $3, $4, $5, 'nodes', $6, now())
`

const insertEngineOutboxSQL = `
INSERT INTO outbox (id, namespace_id, topic, payload, status, available_at)
VALUES ($1, $2, $3, $4, 'pending', now())
`

// JiraTicketReporterActorKey is the registered narrow bridge deployment must
// provide before ticket reports can drain. It is a target identity, not an
// emitter credential; the sweep never reads it.
const JiraTicketReporterActorKey = "company/jira-comment"

// AppendEvent writes one audit event for a run and its outbox row (PRD §12.5
// steps 7 and 10).
//
// The sequence is assigned by reading MAX(sequence) + 1 for the aggregate,
// which is only safe because the caller holds the run's advisory lock for the
// whole transaction — the engine takes ledger.RunLockKey(runID) before it
// writes anything, so two completions of the same run cannot both read the
// same maximum. The events(aggregate_id, sequence) unique index is the
// backstop if that discipline ever slips: a race becomes a rolled-back
// transaction, never a duplicate sequence.
func (eq engineQueries) AppendEvent(ctx context.Context, runID string, in engine.EventInput) (int64, error) {
	if runID == "" {
		return 0, errors.New("postgres: engine: AppendEvent requires a run id")
	}
	if in.Type == "" {
		return 0, errors.New("postgres: engine: AppendEvent requires an event type")
	}
	payload := jsonOrEmptyObject(in.Data)

	var sequence int64
	if err := eq.q.QueryRow(ctx,
		`SELECT (COALESCE(MAX(sequence), 0) + 1)::bigint FROM events WHERE aggregate_id = $1`, runID,
	).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("postgres: engine: AppendEvent: next sequence: %w", err)
	}

	if _, err := eq.q.Exec(ctx, insertEngineEventSQL,
		store.NewULID(), eq.namespaceID, runID, sequence, in.Type, []byte(payload),
	); err != nil {
		return 0, fmt.Errorf("postgres: engine: AppendEvent: %w", err)
	}
	if _, err := eq.q.Exec(ctx, insertEngineOutboxSQL,
		store.NewULID(), eq.namespaceID, in.Type, []byte(payload),
	); err != nil {
		return 0, fmt.Errorf("postgres: engine: AppendEvent: outbox: %w", err)
	}
	if err := eq.appendJiraTicketReport(ctx, runID, in.Type, payload); err != nil {
		return 0, err
	}
	return sequence, nil
}

// Compile-time proof that the two halves implement the interfaces the engine
// declares. A missing method should be a build failure here, not a runtime
// surprise inside a transaction.
var (
	_ engine.Store = (*EngineStore)(nil)
	_ engine.Tx    = (*engineTx)(nil)
)
