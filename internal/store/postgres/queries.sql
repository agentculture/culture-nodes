-- name: CreateNamespace :one
INSERT INTO namespaces (id, slug, display_name)
VALUES ($1, $2, $3)
RETURNING id, slug, display_name, created_at;

-- name: CreateWorkflowVersion :one
INSERT INTO workflow_versions (
    id, namespace_id, workflow_key, version, draft_id, owner_id,
    source_format, source, normalized_ir, content_digest, published_by_actor_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
)
RETURNING id, namespace_id, workflow_key, version, draft_id, owner_id,
    source_format, source, normalized_ir, content_digest, published_by_actor_id, created_at;

-- name: GetWorkflowVersion :one
SELECT id, namespace_id, workflow_key, version, draft_id, owner_id,
    source_format, source, normalized_ir, content_digest, published_by_actor_id, created_at
FROM workflow_versions
WHERE id = $1;

-- name: NextEventSequence :one
SELECT (COALESCE(MAX(sequence), 0) + 1)::bigint AS next_sequence
FROM events
WHERE aggregate_id = $1;

-- name: InsertEvent :one
INSERT INTO events (
    id, namespace_id, aggregate_type, aggregate_id, sequence, event_type, source, data, occurred_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
RETURNING id, namespace_id, aggregate_type, aggregate_id, sequence, event_type, source, data, occurred_at;

-- name: InsertOutbox :one
INSERT INTO outbox (
    id, namespace_id, topic, payload, status, available_at
) VALUES (
    $1, $2, $3, $4, $5, $6
)
RETURNING id, namespace_id, topic, payload, status, available_at, published_at, attempts, created_at;
