// Package postgres implements the PostgreSQL-backed store for the Culture
// Nodes control plane per docs/initial-design/culture-nodes-prd-spec.md §14:
// connection pooling, schema migrations, and typed access methods.
//
// This task (t6) implements the persistence foundations only -- namespaces,
// immutable workflow versions, the append-only event log, and the
// transactional outbox. The engine, ledger runtime, and work-claiming tasks
// (t7-t11) add typed methods for the remaining §14 tables as they need them;
// migrations/ already creates all of those tables so later tasks are
// additive at the Go layer, not at the schema layer.
//
// Deviation from docs/plans/2026-08-08-culture-nodes-app-design.md (task
// t6's brief names "pgx v5 + sqlc"): sqlc IS used here. `go run
// github.com/sqlc-dev/sqlc/cmd/sqlc@latest generate` (run from this
// directory, configured by sqlc.yaml) reads migrations/ as the schema and
// queries.sql as the query set, and regenerates internal/store/postgres/sqlcgen.
// sqlcgen is committed, generated code -- do not hand-edit it; edit
// queries.sql and re-run `go run github.com/sqlc-dev/sqlc/cmd/sqlc@latest generate`
// instead.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres/sqlcgen"
	"github.com/agentculture/culture-nodes/migrations"
)

// Store is a PostgreSQL-backed handle for the control plane's authoritative
// state. It is safe for concurrent use: it wraps a pgxpool.Pool, which pools
// connections internally.
type Store struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// Connect opens a pooled connection to url (a standard PostgreSQL connection
// string, e.g. "postgres://user:pass@host:5432/dbname") and verifies it with
// a ping. Callers must call Close when done.
func Connect(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	return &Store{pool: pool, q: sqlcgen.New(pool)}, nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// Pool exposes the underlying connection pool for callers (tests, and
// future tasks' store subpackages) that need raw SQL access beyond this
// package's typed methods. Prefer a typed method when one exists.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// ensureSchemaMigrationsDDL creates the migration bookkeeping table. It is
// not itself a numbered migration -- Migrate manages it directly so it
// exists before the first numbered migration ever runs.
const ensureSchemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// migrationAdvisoryLockID serializes migrations within a PostgreSQL cluster.
// The lock is session-scoped, so Migrate holds one pool connection for the
// complete sequence and explicitly releases it before returning.
const migrationAdvisoryLockID int64 = 0x63756c747572656e

// Migrate applies every migration embedded in migrations.FS that has not
// already been recorded in the schema_migrations bookkeeping table, in
// filename order (the numeric prefix), each inside its own transaction.
// It returns the versions applied by this call -- an empty slice, not an
// error, when the schema is already up to date, so Migrate is safe to run
// repeatedly (e.g. from a k8s pre-rollout Job on every deploy).
func (s *Store) Migrate(ctx context.Context) ([]string, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationAdvisoryLockID); err != nil {
		return nil, fmt.Errorf("postgres: acquire migration lock: %w", err)
	}
	defer func() {
		// Session termination also releases this lock. An explicit unlock keeps
		// the pooled connection reusable immediately on the successful path.
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationAdvisoryLockID)
	}()

	if _, err := conn.Exec(ctx, ensureSchemaMigrationsDDL); err != nil {
		return nil, fmt.Errorf("postgres: ensure schema_migrations table: %w", err)
	}

	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("postgres: read embedded migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names) // numeric prefixes sort lexically in apply order

	applied := make([]string, 0)
	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")

		var alreadyApplied bool
		err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`,
			version,
		).Scan(&alreadyApplied)
		if err != nil {
			return applied, fmt.Errorf("postgres: check migration %s: %w", version, err)
		}
		if alreadyApplied {
			continue
		}

		contents, err := migrations.FS.ReadFile(name)
		if err != nil {
			return applied, fmt.Errorf("postgres: read migration %s: %w", name, err)
		}
		checksum := sha256.Sum256(contents)

		tx, err := conn.Begin(ctx)
		if err != nil {
			return applied, fmt.Errorf("postgres: begin migration %s: %w", version, err)
		}

		if _, err := tx.Exec(ctx, string(contents)); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("postgres: apply migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)`,
			version, hex.EncodeToString(checksum[:]),
		); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("postgres: record migration %s: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return applied, fmt.Errorf("postgres: commit migration %s: %w", version, err)
		}

		applied = append(applied, version)
	}

	return applied, nil
}

// CreateNamespace inserts a new namespace row. slug must be unique across
// the installation.
func (s *Store) CreateNamespace(ctx context.Context, slug, displayName string) (Namespace, error) {
	if slug == "" {
		return Namespace{}, fmt.Errorf("postgres: CreateNamespace: slug is required")
	}
	if displayName == "" {
		return Namespace{}, fmt.Errorf("postgres: CreateNamespace: displayName is required")
	}

	row, err := s.q.CreateNamespace(ctx, sqlcgen.CreateNamespaceParams{
		ID:          store.NewULID(),
		Slug:        slug,
		DisplayName: displayName,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return Namespace{}, fmt.Errorf("postgres: CreateNamespace: slug %q already exists: %w", slug, ErrDuplicateNamespace)
		}
		return Namespace{}, fmt.Errorf("postgres: CreateNamespace: %w", err)
	}

	return Namespace{
		ID:          row.ID,
		Slug:        row.Slug,
		DisplayName: row.DisplayName,
		CreatedAt:   tsValue(row.CreatedAt),
	}, nil
}

// CreateWorkflowVersion inserts an immutable workflow version. It fails with
// ErrDuplicateDigest if a version with the same namespace and content digest
// already exists (workflow_versions.content_digest is unique per namespace,
// prd-spec §11.3) -- callers should treat that as "already published" and
// fetch the existing version with GetWorkflowVersion if they need it.
func (s *Store) CreateWorkflowVersion(ctx context.Context, in CreateWorkflowVersionInput) (WorkflowVersion, error) {
	switch {
	case in.NamespaceID == "":
		return WorkflowVersion{}, fmt.Errorf("postgres: CreateWorkflowVersion: namespaceID is required")
	case in.WorkflowKey == "":
		return WorkflowVersion{}, fmt.Errorf("postgres: CreateWorkflowVersion: workflowKey is required")
	case in.SourceFormat == "":
		return WorkflowVersion{}, fmt.Errorf("postgres: CreateWorkflowVersion: sourceFormat is required")
	case in.Source == "":
		return WorkflowVersion{}, fmt.Errorf("postgres: CreateWorkflowVersion: source is required")
	case in.ContentDigest == "":
		return WorkflowVersion{}, fmt.Errorf("postgres: CreateWorkflowVersion: contentDigest is required")
	}

	row, err := s.q.CreateWorkflowVersion(ctx, sqlcgen.CreateWorkflowVersionParams{
		ID:                 store.NewULID(),
		NamespaceID:        in.NamespaceID,
		WorkflowKey:        in.WorkflowKey,
		Version:            in.Version,
		DraftID:            textOrNull(in.DraftID),
		OwnerID:            textOrNull(in.OwnerID),
		SourceFormat:       in.SourceFormat,
		Source:             in.Source,
		NormalizedIr:       jsonOrEmptyObject(in.NormalizedIR),
		ContentDigest:      in.ContentDigest,
		PublishedByActorID: textOrNull(in.PublishedByActorID),
	})
	if err != nil {
		if constraint := uniqueViolationConstraint(err); constraint != "" {
			if isDigestConstraint(constraint) {
				return WorkflowVersion{}, ErrDuplicateDigest
			}
			return WorkflowVersion{}, fmt.Errorf("postgres: CreateWorkflowVersion: constraint %s violated: %w", constraint, err)
		}
		return WorkflowVersion{}, fmt.Errorf("postgres: CreateWorkflowVersion: %w", err)
	}

	return toWorkflowVersion(row), nil
}

// GetWorkflowVersion fetches an immutable workflow version by id. It returns
// ErrNotFound if no such version exists.
func (s *Store) GetWorkflowVersion(ctx context.Context, id string) (WorkflowVersion, error) {
	row, err := s.q.GetWorkflowVersion(ctx, id)
	if err != nil {
		if isNoRows(err) {
			return WorkflowVersion{}, ErrNotFound
		}
		return WorkflowVersion{}, fmt.Errorf("postgres: GetWorkflowVersion: %w", err)
	}
	return toWorkflowVersion(row), nil
}

func toWorkflowVersion(row sqlcgen.WorkflowVersion) WorkflowVersion {
	return WorkflowVersion{
		ID:                 row.ID,
		NamespaceID:        row.NamespaceID,
		WorkflowKey:        row.WorkflowKey,
		Version:            row.Version,
		DraftID:            textOrEmpty(row.DraftID),
		OwnerID:            textOrEmpty(row.OwnerID),
		SourceFormat:       row.SourceFormat,
		Source:             row.Source,
		NormalizedIR:       row.NormalizedIr,
		ContentDigest:      row.ContentDigest,
		PublishedByActorID: textOrEmpty(row.PublishedByActorID),
		CreatedAt:          tsValue(row.CreatedAt),
	}
}

// insertEventMaxAttempts bounds the outer retry loop in InsertEvent.
// events.sequence is monotonic per aggregate_id; the
// events_aggregate_id_sequence_key unique index (migrations/0004) is what
// actually enforces that (proven by
// TestInsertEventUniqueIndexRejectsDuplicateSequence). The per-aggregate
// advisory lock InsertEvent takes below serializes concurrent writers for
// the same aggregate so, in practice, the first attempt always succeeds;
// the retry loop exists as a backstop for the residual chance of two
// different aggregate IDs hashing to the same advisory-lock key.
const insertEventMaxAttempts = 8

// InsertEvent appends an audit event, assigning it the next sequence number
// for its AggregateID.
//
// Sequence assignment happens inside a transaction holding
// pg_advisory_xact_lock(hashtextextended(aggregate_id, 0)): without that
// lock, two concurrent InsertEvent calls for the same aggregate can both
// read the same MAX(sequence) and then race on the unique index, and under
// enough concurrency (see TestInsertEventConcurrentSameAggregateStaysMonotonic,
// which drives 25 goroutines at one aggregate) that race can outrun a
// bounded retry budget. The advisory lock is scoped to the transaction and
// keyed per aggregate, so unrelated aggregates never contend with each
// other; it is released automatically on commit or rollback.
func (s *Store) InsertEvent(ctx context.Context, in InsertEventInput) (Event, error) {
	switch {
	case in.NamespaceID == "":
		return Event{}, fmt.Errorf("postgres: InsertEvent: namespaceID is required")
	case in.AggregateType == "":
		return Event{}, fmt.Errorf("postgres: InsertEvent: aggregateType is required")
	case in.AggregateID == "":
		return Event{}, fmt.Errorf("postgres: InsertEvent: aggregateID is required")
	case in.EventType == "":
		return Event{}, fmt.Errorf("postgres: InsertEvent: eventType is required")
	}

	source := in.Source
	if source == "" {
		source = "nodes"
	}
	data := jsonOrEmptyObject(in.Data)

	var lastErr error
	for attempt := 0; attempt < insertEventMaxAttempts; attempt++ {
		ev, err := s.insertEventOnce(ctx, in, source, data)
		if err == nil {
			return ev, nil
		}
		if isUniqueViolation(err) {
			lastErr = err
			continue
		}
		return Event{}, fmt.Errorf("postgres: InsertEvent: %w", err)
	}

	return Event{}, fmt.Errorf(
		"postgres: InsertEvent: exhausted %d attempts assigning a sequence for aggregate %s: %w",
		insertEventMaxAttempts, in.AggregateID, lastErr,
	)
}

func (s *Store) insertEventOnce(ctx context.Context, in InsertEventInput, source string, data []byte) (Event, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Event{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, in.AggregateID); err != nil {
		return Event{}, fmt.Errorf("advisory lock: %w", err)
	}

	qtx := s.q.WithTx(tx)

	seq, err := qtx.NextEventSequence(ctx, in.AggregateID)
	if err != nil {
		return Event{}, fmt.Errorf("compute next sequence: %w", err)
	}

	row, err := qtx.InsertEvent(ctx, sqlcgen.InsertEventParams{
		ID:            store.NewULID(),
		NamespaceID:   in.NamespaceID,
		AggregateType: in.AggregateType,
		AggregateID:   in.AggregateID,
		Sequence:      seq,
		EventType:     in.EventType,
		Source:        source,
		Data:          data,
		OccurredAt:    tsOrNow(time.Time{}),
	})
	if err != nil {
		return Event{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Event{}, fmt.Errorf("commit: %w", err)
	}

	return Event{
		ID:            row.ID,
		NamespaceID:   row.NamespaceID,
		AggregateType: row.AggregateType,
		AggregateID:   row.AggregateID,
		Sequence:      row.Sequence,
		EventType:     row.EventType,
		Source:        row.Source,
		Data:          row.Data,
		OccurredAt:    tsValue(row.OccurredAt),
	}, nil
}

// InsertOutbox inserts a pending outbox row for transactional publication
// (prd-spec §12.5). AvailableAt defaults to now() when zero.
func (s *Store) InsertOutbox(ctx context.Context, in InsertOutboxInput) (OutboxRecord, error) {
	switch {
	case in.NamespaceID == "":
		return OutboxRecord{}, fmt.Errorf("postgres: InsertOutbox: namespaceID is required")
	case in.Topic == "":
		return OutboxRecord{}, fmt.Errorf("postgres: InsertOutbox: topic is required")
	}

	row, err := s.q.InsertOutbox(ctx, sqlcgen.InsertOutboxParams{
		ID:          store.NewULID(),
		NamespaceID: in.NamespaceID,
		Topic:       in.Topic,
		Payload:     jsonOrEmptyObject(in.Payload),
		Status:      "pending",
		AvailableAt: tsOrNow(in.AvailableAt),
	})
	if err != nil {
		return OutboxRecord{}, fmt.Errorf("postgres: InsertOutbox: %w", err)
	}

	return OutboxRecord{
		ID:          row.ID,
		NamespaceID: row.NamespaceID,
		Topic:       row.Topic,
		Payload:     row.Payload,
		Status:      row.Status,
		AvailableAt: tsValue(row.AvailableAt),
		PublishedAt: tsPtr(row.PublishedAt),
		Attempts:    row.Attempts,
		CreatedAt:   tsValue(row.CreatedAt),
	}, nil
}
