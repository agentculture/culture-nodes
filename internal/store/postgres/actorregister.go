package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/preflight"
	"github.com/agentculture/culture-nodes/internal/store"
)

// RegisterActorParams carries the caller-supplied columns of one new actors
// row (migrations/0001_namespaces_and_identity.sql). The id, namespace, and
// revision are never the caller's to choose: the id is minted here, the
// namespace is the store's own scope, and the revision is always the next
// one for ActorKey — actor identity is append-only (see Actor's doc
// comment), so registration can only ever add the next revision row.
type RegisterActorParams struct {
	ActorKey     string
	Kind         string
	Protocol     string
	EndpointRef  string
	Capabilities json.RawMessage
	Metadata     json.RawMessage
}

// RegisterActor appends the next revision row for p.ActorKey and returns it
// — deploy/prod/register-actor.sh's INSERT semantics, moved behind the API
// (task t13): a first registration lands revision 1, and re-registering an
// existing key INSERTs revision max+1, never an UPDATE to any existing row.
// Empty Capabilities/Metadata land as the columns' '{}' shape rather than
// SQL NULL (both columns are NOT NULL DEFAULT '{}'); an empty EndpointRef
// lands as NULL, matching what scanActor reads back as "".
//
// Two concurrent registrations of the same key can both compute the same
// next revision; the actors_namespace_key_revision_key unique constraint
// turns that race into an error for one of them rather than two rows
// claiming one revision — the caller retries, the ledger-style append-only
// shape stays intact.
func (eq engineQueries) RegisterActor(ctx context.Context, p RegisterActorParams) (Actor, error) {
	// Task t14 / issue #67, acceptance criterion 3: enabling the
	// clarify-then-commit gate for an actor whose registration advertises no
	// capability surface is refused HERE, when the actor is registered,
	// rather than discovered later by a run that stalls against a gate
	// nothing can satisfy.
	//
	// The same check runs in the API handler, which renders it as a 400 with
	// the remediation; this one covers every caller that does not go through
	// that handler, and migration 0026's CHECK constraint covers raw SQL. An
	// actor that does not enable the gate is never refused for anything
	// here — the surface is only load-bearing once something depends on it.
	if err := preflight.CheckConfiguration(p.Capabilities, p.Metadata); err != nil {
		return Actor{}, fmt.Errorf("postgres: engine: RegisterActor %s: %w", p.ActorKey, err)
	}

	capabilities := p.Capabilities
	if len(capabilities) == 0 {
		capabilities = json.RawMessage(`{}`)
	}
	metadata := p.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	var endpointRef any
	if p.EndpointRef != "" {
		endpointRef = p.EndpointRef
	}

	row := eq.q.QueryRow(ctx, `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref, capabilities, metadata)
		SELECT $1, $2, $3, COALESCE(MAX(revision), 0) + 1, $4, $5, $6, $7, $8
		  FROM actors WHERE namespace_id = $2 AND actor_key = $3
		RETURNING `+actorColumns,
		store.NewULID(), eq.namespaceID, p.ActorKey, p.Kind, p.Protocol, endpointRef, capabilities, metadata)
	a, err := scanActor(row)
	if err != nil {
		return Actor{}, fmt.Errorf("postgres: engine: RegisterActor %s: %w", p.ActorKey, err)
	}
	return a, nil
}
