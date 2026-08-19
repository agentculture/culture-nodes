package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentculture/culture-nodes/internal/store"
)

// Store-entry bindings (migration 0044, plan task t8, issue #192): the
// explicit mapping step that makes a pulled catalog entry runnable on this
// plane. Each row binds one graph-pinned capability requirement (the
// verbatim actor://…/runner://… ref from the entry's evidence manifest) to
// a LOCAL actor registration. Rows are insert-only records — who bound what
// to what, when — and "the current binding" is the newest row per
// (entry, required_ref). The graph document is never touched: resolution
// consults these rows at publish time (internal/api/storebindings.go) and
// at dispatch time (internal/worker/registry.go).
//
// Dispatch resolution is namespace-wide — the worker registry sees a ref,
// not an entry — while binding and publish validation are entry-scoped.
// Those two scopes only compose when every entry that binds a ref agrees on
// the local key: otherwise whichever entry bound last would silently
// redirect every OTHER entry's dispatches (PR #208 finding 4). So the
// resolution read reduces the trail to each entry's CURRENT binding and
// answers only on agreement (disagreement is ErrStoreBindingConflict, named
// per entry), and the create path refuses an entry JOINING an already-bound
// ref with a different key — while an entry that already binds the ref may
// rebind, so migrating a shared mapping has a first mover.

// CreateStoreEntryBindingInput is the input to CreateStoreEntryBinding.
type CreateStoreEntryBindingInput struct {
	NamespaceID   string
	EntryID       string
	RequiredRef   string
	RequiredKind  string // "actor" | "runner"
	BoundActorID  string // actors row id current at bind time
	BoundActorKey string
	BoundBy       string
}

// StoreEntryBinding is one store_entry_bindings row.
type StoreEntryBinding struct {
	ID            string
	NamespaceID   string
	EntryID       string
	RequiredRef   string
	RequiredKind  string
	BoundActorID  string
	BoundActorKey string
	BoundBy       string
	CreatedAt     time.Time
}

// CreateStoreEntryBinding appends one binding record (Store-level entry
// point, explicit namespace — this package's test-suite convention).
func (s *Store) CreateStoreEntryBinding(ctx context.Context, in CreateStoreEntryBindingInput) (StoreEntryBinding, error) {
	return createStoreEntryBinding(ctx, s.pool, in)
}

// ListStoreEntryBindings lists every binding record of one entry, newest
// first — the full trail, superseded rows included (bindings are records).
func (s *Store) ListStoreEntryBindings(ctx context.Context, namespaceID, entryID string) ([]StoreEntryBinding, error) {
	return listStoreEntryBindings(ctx, s.pool, namespaceID, entryID)
}

// CurrentActorByKey returns the newest registration revision for an actor
// key — the same "the current registration is the newest row" reading the
// worker registry resolves dispatches with. ErrNotFound when the key has
// never been registered here.
func (s *Store) CurrentActorByKey(ctx context.Context, namespaceID, actorKey string) (Actor, error) {
	if namespaceID == "" {
		return Actor{}, fmt.Errorf("postgres: CurrentActorByKey: namespaceID is required")
	}
	if actorKey == "" {
		return Actor{}, fmt.Errorf("postgres: CurrentActorByKey: actorKey is required")
	}
	row := s.pool.QueryRow(ctx, `SELECT `+actorColumns+`
		FROM actors WHERE namespace_id = $1 AND actor_key = $2
		ORDER BY revision DESC LIMIT 1`, namespaceID, actorKey)
	a, err := scanActor(row)
	if err != nil {
		if isNoRows(err) {
			return Actor{}, fmt.Errorf("postgres: actor %s: %w", actorKey, ErrNotFound)
		}
		return Actor{}, fmt.Errorf("postgres: CurrentActorByKey %s: %w", actorKey, err)
	}
	return a, nil
}

// ResolveStoreBoundActorKey returns the local actor key the current
// bindings map a graph-pinned ref to — the dispatch-resolution read
// (internal/worker/registry.go's fallback when no local registration
// answers for the ref directly). Bindings are created and superseded PER
// ENTRY, so "current" is computed per entry, and the namespace-wide answer
// exists only when every entry that binds the ref agrees on the key.
// Disagreement is ErrStoreBindingConflict, named per entry; a ref nothing
// binds is ErrNotFound.
func (s *Store) ResolveStoreBoundActorKey(ctx context.Context, namespaceID, requiredRef string) (string, error) {
	current, err := s.CurrentStoreEntryBindingsByRef(ctx, namespaceID, requiredRef)
	if err != nil {
		return "", err
	}
	if len(current) == 0 {
		return "", fmt.Errorf("postgres: binding for %s: %w", requiredRef, ErrNotFound)
	}
	key := current[0].BoundActorKey
	for _, b := range current[1:] {
		if b.BoundActorKey == key {
			continue
		}
		pairs := make([]string, 0, len(current))
		for _, c := range current {
			pairs = append(pairs, fmt.Sprintf("entry %s -> %s", c.EntryID, c.BoundActorKey))
		}
		return "", fmt.Errorf("postgres: %w: %s: %s — rebind the disagreeing entries to one key",
			ErrStoreBindingConflict, requiredRef, strings.Join(pairs, ", "))
	}
	return key, nil
}

// CurrentStoreEntryBindingsByRef returns each entry's CURRENT binding for a
// ref — the newest row per (entry, required_ref) — across the namespace,
// ordered by entry id for a stable read. This is the resolution view
// ResolveStoreBoundActorKey reduces, and the agreement set
// createStoreEntryBinding checks a joining entry against.
func (s *Store) CurrentStoreEntryBindingsByRef(ctx context.Context, namespaceID, requiredRef string) ([]StoreEntryBinding, error) {
	return currentStoreEntryBindingsByRef(ctx, s.pool, namespaceID, requiredRef)
}

func currentStoreEntryBindingsByRef(ctx context.Context, pool *pgxpool.Pool, namespaceID, requiredRef string) ([]StoreEntryBinding, error) {
	if namespaceID == "" {
		return nil, fmt.Errorf("postgres: CurrentStoreEntryBindingsByRef: namespaceID is required")
	}
	if requiredRef == "" {
		return nil, fmt.Errorf("postgres: CurrentStoreEntryBindingsByRef: requiredRef is required")
	}
	rows, err := pool.Query(ctx, `SELECT DISTINCT ON (entry_id) `+storeEntryBindingColumns+`
		FROM store_entry_bindings
		WHERE namespace_id = $1 AND required_ref = $2
		ORDER BY entry_id, created_at DESC, id DESC`, namespaceID, requiredRef)
	if err != nil {
		return nil, fmt.Errorf("postgres: CurrentStoreEntryBindingsByRef: %w", err)
	}
	defer rows.Close()

	out := make([]StoreEntryBinding, 0)
	for rows.Next() {
		b, err := scanStoreEntryBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: CurrentStoreEntryBindingsByRef: scan: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: CurrentStoreEntryBindingsByRef: %w", err)
	}
	return out, nil
}

// The namespace-bound mirrors for the API surface (storeentries.go's
// convention).

// CreateStoreEntryBinding appends one binding record, scoped to es's
// namespace.
func (es *EngineStore) CreateStoreEntryBinding(ctx context.Context, in CreateStoreEntryBindingInput) (StoreEntryBinding, error) {
	in.NamespaceID = es.namespaceID
	return createStoreEntryBinding(ctx, es.pool, in)
}

// ListStoreEntryBindings lists one entry's binding records, scoped to es's
// namespace.
func (es *EngineStore) ListStoreEntryBindings(ctx context.Context, entryID string) ([]StoreEntryBinding, error) {
	return listStoreEntryBindings(ctx, es.pool, es.namespaceID, entryID)
}

// CurrentBindings reduces a newest-first record trail to the current
// binding per required ref — the resolution view of an append-only table.
func CurrentBindings(records []StoreEntryBinding) map[string]StoreEntryBinding {
	current := make(map[string]StoreEntryBinding, len(records))
	for _, b := range records {
		if _, seen := current[b.RequiredRef]; !seen {
			current[b.RequiredRef] = b
		}
	}
	return current
}

const storeEntryBindingColumns = `id, namespace_id, entry_id, required_ref, required_kind,
	bound_actor_id, bound_actor_key, bound_by, created_at`

func createStoreEntryBinding(ctx context.Context, pool *pgxpool.Pool, in CreateStoreEntryBindingInput) (StoreEntryBinding, error) {
	switch {
	case in.NamespaceID == "":
		return StoreEntryBinding{}, fmt.Errorf("postgres: CreateStoreEntryBinding: namespaceID is required")
	case in.EntryID == "":
		return StoreEntryBinding{}, fmt.Errorf("postgres: CreateStoreEntryBinding: entryID is required")
	case in.RequiredRef == "":
		return StoreEntryBinding{}, fmt.Errorf("postgres: CreateStoreEntryBinding: requiredRef is required")
	case in.RequiredKind != "actor" && in.RequiredKind != "runner":
		return StoreEntryBinding{}, fmt.Errorf("postgres: CreateStoreEntryBinding: requiredKind must be %q or %q, got %q", "actor", "runner", in.RequiredKind)
	case in.BoundActorID == "":
		return StoreEntryBinding{}, fmt.Errorf("postgres: CreateStoreEntryBinding: boundActorID is required")
	case in.BoundActorKey == "":
		return StoreEntryBinding{}, fmt.Errorf("postgres: CreateStoreEntryBinding: boundActorKey is required")
	case in.BoundBy == "":
		return StoreEntryBinding{}, fmt.Errorf("postgres: CreateStoreEntryBinding: boundBy is required")
	}

	// Dispatch resolution (ResolveStoreBoundActorKey) is namespace-wide:
	// two entries binding one ref to DIFFERENT actors would leave it no
	// honest single answer. So an entry JOINING an already-bound ref must
	// agree with every other entry's CURRENT binding — refused by name
	// otherwise. An entry that already binds the ref may still rebind
	// (append-only corrections, and migrating a shared mapping needs a
	// first mover); until the other entries follow, resolution refuses the
	// ref as conflicting rather than picking a side.
	current, err := currentStoreEntryBindingsByRef(ctx, pool, in.NamespaceID, in.RequiredRef)
	if err != nil {
		return StoreEntryBinding{}, fmt.Errorf("postgres: CreateStoreEntryBinding: conflict probe: %w", err)
	}
	entryAlreadyBinds := false
	var disagreeing *StoreEntryBinding
	for i := range current {
		if current[i].EntryID == in.EntryID {
			entryAlreadyBinds = true
			continue
		}
		if current[i].BoundActorKey != in.BoundActorKey {
			disagreeing = &current[i]
		}
	}
	if !entryAlreadyBinds && disagreeing != nil {
		return StoreEntryBinding{}, fmt.Errorf(
			"postgres: CreateStoreEntryBinding: entry %s currently binds %s to %q, so binding it to %q here would make dispatch resolution ambiguous: %w",
			disagreeing.EntryID, in.RequiredRef, disagreeing.BoundActorKey, in.BoundActorKey, ErrStoreBindingConflict)
	}

	id := store.NewULID()
	_, err = pool.Exec(ctx, `
		INSERT INTO store_entry_bindings (
			id, namespace_id, entry_id, required_ref, required_kind,
			bound_actor_id, bound_actor_key, bound_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, in.NamespaceID, in.EntryID, in.RequiredRef, in.RequiredKind,
		in.BoundActorID, in.BoundActorKey, in.BoundBy)
	if err != nil {
		return StoreEntryBinding{}, fmt.Errorf("postgres: CreateStoreEntryBinding: insert: %w", err)
	}

	row := pool.QueryRow(ctx, `SELECT `+storeEntryBindingColumns+`
		FROM store_entry_bindings WHERE id = $1`, id)
	b, err := scanStoreEntryBinding(row)
	if err != nil {
		return StoreEntryBinding{}, fmt.Errorf("postgres: CreateStoreEntryBinding: read back %s: %w", id, err)
	}
	return b, nil
}

func listStoreEntryBindings(ctx context.Context, pool *pgxpool.Pool, namespaceID, entryID string) ([]StoreEntryBinding, error) {
	if namespaceID == "" {
		return nil, fmt.Errorf("postgres: ListStoreEntryBindings: namespaceID is required")
	}
	if entryID == "" {
		return nil, fmt.Errorf("postgres: ListStoreEntryBindings: entryID is required")
	}
	rows, err := pool.Query(ctx, `SELECT `+storeEntryBindingColumns+`
		FROM store_entry_bindings
		WHERE namespace_id = $1 AND entry_id = $2
		ORDER BY created_at DESC, id DESC`, namespaceID, entryID)
	if err != nil {
		return nil, fmt.Errorf("postgres: ListStoreEntryBindings: %w", err)
	}
	defer rows.Close()

	out := make([]StoreEntryBinding, 0)
	for rows.Next() {
		b, err := scanStoreEntryBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: ListStoreEntryBindings: scan: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: ListStoreEntryBindings: %w", err)
	}
	return out, nil
}

func scanStoreEntryBinding(row scanRow) (StoreEntryBinding, error) {
	var (
		b         StoreEntryBinding
		createdAt pgtype.Timestamptz
	)
	if err := row.Scan(&b.ID, &b.NamespaceID, &b.EntryID, &b.RequiredRef, &b.RequiredKind,
		&b.BoundActorID, &b.BoundActorKey, &b.BoundBy, &createdAt); err != nil {
		return StoreEntryBinding{}, err
	}
	b.CreatedAt = tsValue(createdAt)
	return b, nil
}
