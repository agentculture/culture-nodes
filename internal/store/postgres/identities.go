package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/auth"
	"github.com/agentculture/culture-nodes/internal/store"
)

// Identity is one durable binding between a provider subject and an actor.
// Revoked bindings remain stored as history but are excluded from lookup.
type Identity struct {
	ID          string
	NamespaceID string
	Provider    string
	Subject     string
	ActorID     string
	Roles       []string
	CreatedAt   time.Time
	RevokedAt   *time.Time
}

var (
	// ErrIdentityAlreadyBound marks an attempt to append a second live binding
	// for the same namespace, provider, and subject.
	ErrIdentityAlreadyBound = errors.New("postgres: identity is already bound")
	// ErrIdentityNotFound marks a lookup with no live binding, or a revoke of
	// an unknown identity row.
	ErrIdentityNotFound = errors.New("postgres: identity not found")
)

const identityColumns = `id, namespace_id, provider, subject, actor_id, roles, created_at, revoked_at`

// BindIdentity appends a live provider-subject binding. A revoked binding does
// not prevent a later bind of the same key; a second live binding does.
func (s *Store) BindIdentity(ctx context.Context, namespaceID, provider, subject, actorID string, roles []string) (Identity, error) {
	if namespaceID == "" {
		return Identity{}, errors.New("postgres: BindIdentity: namespaceID is required")
	}
	if subject == "" {
		return Identity{}, errors.New("postgres: BindIdentity: subject is required")
	}
	if actorID == "" {
		return Identity{}, errors.New("postgres: BindIdentity: actorID is required")
	}
	for _, role := range roles {
		if _, err := auth.ParseRole(role); err != nil {
			return Identity{}, fmt.Errorf("postgres: BindIdentity: %w", err)
		}
	}
	if roles == nil {
		roles = []string{}
	}

	row := s.pool.QueryRow(ctx, `INSERT INTO actor_identities
		(id, namespace_id, provider, subject, actor_id, roles)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+identityColumns,
		store.NewULID(), namespaceID, provider, subject, actorID, roles)
	identity, err := scanIdentity(row)
	if err != nil {
		if uniqueViolationConstraint(err) == "actor_identities_live_key" {
			return Identity{}, fmt.Errorf("postgres: BindIdentity %s/%s: %w", provider, subject, ErrIdentityAlreadyBound)
		}
		return Identity{}, fmt.Errorf("postgres: BindIdentity: %w", err)
	}
	return identity, nil
}

// LookupIdentity returns the live binding for a provider subject. Revoked rows
// are deliberately invisible to authentication callers.
func (s *Store) LookupIdentity(ctx context.Context, namespaceID, provider, subject string) (Identity, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+identityColumns+`
		FROM actor_identities
		WHERE namespace_id = $1 AND provider = $2 AND subject = $3 AND revoked_at IS NULL`,
		namespaceID, provider, subject)
	identity, err := scanIdentity(row)
	if err != nil {
		if isNoRows(err) {
			return Identity{}, fmt.Errorf("postgres: LookupIdentity %s/%s: %w", provider, subject, ErrIdentityNotFound)
		}
		return Identity{}, fmt.Errorf("postgres: LookupIdentity: %w", err)
	}
	return identity, nil
}

// RevokeIdentity records revocation without deleting the binding history.
func (s *Store) RevokeIdentity(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE actor_identities
		SET revoked_at = now()
		WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("postgres: RevokeIdentity: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: RevokeIdentity %s: %w", id, ErrIdentityNotFound)
	}
	return nil
}

type identityScanner interface {
	Scan(dest ...any) error
}

func scanIdentity(row identityScanner) (Identity, error) {
	var identity Identity
	err := row.Scan(
		&identity.ID,
		&identity.NamespaceID,
		&identity.Provider,
		&identity.Subject,
		&identity.ActorID,
		&identity.Roles,
		&identity.CreatedAt,
		&identity.RevokedAt,
	)
	return identity, err
}
