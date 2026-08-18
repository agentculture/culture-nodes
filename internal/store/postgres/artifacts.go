package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// This file adds the artifacts-table metadata helpers task t15
// (internal/artifacts) needs. It intentionally does not use sqlc/queries.sql
// (unlike the rest of this package, see store.go's package doc): t15's brief
// asks for these as new files only, so the raw SQL here never touches the
// generated sqlcgen code or hand-maintained queries.sql that store.go's
// existing methods share. Store.Pool(), already exported for exactly this
// kind of caller, is what makes that possible without editing anything else
// in this package.
//
// The artifacts table itself is migrations/0004_observability.sql; its
// unique (namespace_id, uri) index is migrations/0006_artifact_blobs.sql
// (added alongside the Postgres small-blob driver's own table). Content
// bytes never live here -- only metadata: which backend holds them, how
// big they are, and their sha256 digest. That is what lets
// internal/artifacts.Router route a Get to the right backend, and what lets
// a Get anywhere verify what it streamed back against what Put recorded.

// ArtifactRecord is a row of the artifacts metadata table. RunID,
// AttemptID, MediaType, and Digest read back as "" when the column is NULL.
type ArtifactRecord struct {
	ID             string
	NamespaceID    string
	RunID          string
	AttemptID      string
	URI            string
	MediaType      string
	SizeBytes      int64
	Digest         string
	StorageBackend string
	Metadata       json.RawMessage
	CreatedAt      time.Time
}

type ArtifactTombstoneRecord struct {
	ID, ArtifactID, ArtifactRef, Reason, Name, MediaType, Digest, Supersedes string
	SizeBytes                                                                int64
	ReapedAt                                                                 time.Time
}

type InsertArtifactTombstoneInput struct {
	ID, ArtifactID, ArtifactRef, Reason, Name, MediaType, Digest, Supersedes string
	SizeBytes                                                                int64
	ReapedAt                                                                 time.Time
}

// InsertArtifactInput is the input to Store.InsertArtifact and
// InsertArtifactTx. RunID, AttemptID, and MediaType are optional ("" means
// NULL); ID, NamespaceID, URI, and StorageBackend are required.
type InsertArtifactInput struct {
	ID             string
	NamespaceID    string
	RunID          string
	AttemptID      string
	URI            string
	MediaType      string
	SizeBytes      int64
	Digest         string
	StorageBackend string
	Metadata       json.RawMessage
}

// ErrDuplicateArtifactURI is returned by InsertArtifact/InsertArtifactTx
// when a row already exists for the given namespace_id + uri
// (artifacts_namespace_uri_key, migrations/0006_artifact_blobs.sql). Every
// ref is minted from a fresh store.NewULID(), so this should only happen if
// a caller replays an insert for a ref that already exists.
var ErrDuplicateArtifactURI = errors.New("postgres: artifact with this namespace and uri already exists")

// querier is satisfied by both *pgxpool.Pool and pgx.Tx -- the exact subset
// of pgx's API insertArtifact needs. InsertArtifact/InsertArtifactTx take
// one instead of a *Store directly, so a caller with its own multi-table
// transaction (internal/artifacts/postgres, which must write this
// package's artifacts row and its own artifact_blobs content row
// atomically -- artifact_blobs.id has a foreign key on artifacts.id) can
// compose InsertArtifactTx into that transaction instead of opening and
// committing its own. Exec is part of the interface (not just QueryRow) so
// DeleteArtifactByURI's plain, non-transactional delete can share it too.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const artifactColumns = `id, namespace_id, run_id, attempt_id, uri, media_type,
	size_bytes, digest, storage_backend, metadata, created_at`

// InsertArtifact records one artifact metadata row using the Store's own
// pooled connection (no caller-managed transaction). Every
// internal/artifacts.Store.Put call, regardless of backend, ends with
// exactly one metadata write through this helper (directly, or via
// InsertArtifactTx) -- that single point of truth is what the pod-agnostic
// proof (t15) depends on: any pod's Get resolves the same row.
func (s *Store) InsertArtifact(ctx context.Context, in InsertArtifactInput) (ArtifactRecord, error) {
	return insertArtifact(ctx, s.pool, in)
}

// InsertArtifactTx is InsertArtifact scoped to a caller-managed transaction.
// See the querier doc comment above for why this exists.
func InsertArtifactTx(ctx context.Context, tx pgx.Tx, in InsertArtifactInput) (ArtifactRecord, error) {
	return insertArtifact(ctx, tx, in)
}

func insertArtifact(ctx context.Context, q querier, in InsertArtifactInput) (ArtifactRecord, error) {
	switch {
	case in.ID == "":
		return ArtifactRecord{}, fmt.Errorf("postgres: InsertArtifact: id is required")
	case in.NamespaceID == "":
		return ArtifactRecord{}, fmt.Errorf("postgres: InsertArtifact: namespaceID is required")
	case in.URI == "":
		return ArtifactRecord{}, fmt.Errorf("postgres: InsertArtifact: uri is required")
	case in.StorageBackend == "":
		return ArtifactRecord{}, fmt.Errorf("postgres: InsertArtifact: storageBackend is required")
	}

	row := q.QueryRow(ctx, `
		INSERT INTO artifacts (
			id, namespace_id, run_id, attempt_id, uri, media_type,
			size_bytes, digest, storage_backend, metadata
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+artifactColumns,
		in.ID, in.NamespaceID, textOrNull(in.RunID), textOrNull(in.AttemptID), in.URI,
		textOrNull(in.MediaType), int8Value(in.SizeBytes), textOrNull(in.Digest), in.StorageBackend,
		jsonOrEmptyObject(in.Metadata),
	)

	rec, err := scanArtifactRow(row)
	if err != nil {
		if isUniqueViolation(err) {
			return ArtifactRecord{}, fmt.Errorf("postgres: InsertArtifact: namespace %q uri %q: %w", in.NamespaceID, in.URI, ErrDuplicateArtifactURI)
		}
		return ArtifactRecord{}, fmt.Errorf("postgres: InsertArtifact: %w", err)
	}
	return rec, nil
}

// GetArtifactByURI fetches an artifact metadata row by its (namespaceID,
// uri) key -- the same pair every internal/artifacts.Ref encodes. It
// returns ErrNotFound if no such row exists.
func (s *Store) GetArtifactByURI(ctx context.Context, namespaceID, uri string) (ArtifactRecord, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+artifactColumns+`
		FROM artifacts
		WHERE namespace_id = $1 AND uri = $2`,
		namespaceID, uri,
	)

	rec, err := scanArtifactRow(row)
	if err != nil {
		if isNoRows(err) {
			return ArtifactRecord{}, ErrNotFound
		}
		return ArtifactRecord{}, fmt.Errorf("postgres: GetArtifactByURI: %w", err)
	}
	return rec, nil
}

// ListArtifactsByAttempt returns every artifact metadata row recorded for one
// attempt, oldest first. Attempt ids are ULIDs minted by this store, so the
// attempt id alone identifies the rows without a namespace qualifier -- the
// same single-metadata-table property Router relies on for Get.
func (s *Store) ListArtifactsByAttempt(ctx context.Context, attemptID string) ([]ArtifactRecord, error) {
	if attemptID == "" {
		return nil, fmt.Errorf("postgres: ListArtifactsByAttempt: attemptID is required")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+artifactColumns+`
		FROM artifacts
		WHERE attempt_id = $1
		ORDER BY created_at, id`,
		attemptID,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: ListArtifactsByAttempt: %w", err)
	}
	defer rows.Close()
	var out []ArtifactRecord
	for rows.Next() {
		rec, scanErr := scanArtifactRow(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("postgres: ListArtifactsByAttempt: %w", scanErr)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: ListArtifactsByAttempt: %w", err)
	}
	return out, nil
}

func (s *Store) GetArtifactTombstone(ctx context.Context, artifactID string) (ArtifactTombstoneRecord, error) {
	return getArtifactTombstone(ctx, s.pool, artifactID)
}

func GetArtifactTombstoneTx(ctx context.Context, tx pgx.Tx, artifactID string) (ArtifactTombstoneRecord, error) {
	return getArtifactTombstone(ctx, tx, artifactID)
}

func getArtifactTombstone(ctx context.Context, q querier, artifactID string) (ArtifactTombstoneRecord, error) {
	var rec ArtifactTombstoneRecord
	err := q.QueryRow(ctx, `SELECT id, artifact_id, artifact_ref, reaped_at, reason,
		name, media_type, size_bytes, digest, COALESCE(supersedes, '')
		FROM artifact_tombstones WHERE artifact_id=$1
		ORDER BY reaped_at DESC, id DESC LIMIT 1`, artifactID).Scan(
		&rec.ID, &rec.ArtifactID, &rec.ArtifactRef, &rec.ReapedAt, &rec.Reason,
		&rec.Name, &rec.MediaType, &rec.SizeBytes, &rec.Digest, &rec.Supersedes)
	if isNoRows(err) {
		return ArtifactTombstoneRecord{}, ErrNotFound
	}
	if err != nil {
		return ArtifactTombstoneRecord{}, fmt.Errorf("postgres: GetArtifactTombstone: %w", err)
	}
	return rec, nil
}

func InsertArtifactTombstoneTx(ctx context.Context, tx pgx.Tx, in InsertArtifactTombstoneInput) (ArtifactTombstoneRecord, error) {
	if in.ID == "" || in.ArtifactID == "" || in.ArtifactRef == "" || in.Reason == "" {
		return ArtifactTombstoneRecord{}, fmt.Errorf("postgres: InsertArtifactTombstone: id, artifact, ref, and reason are required")
	}
	var rec ArtifactTombstoneRecord
	err := tx.QueryRow(ctx, `INSERT INTO artifact_tombstones
		(id, artifact_id, artifact_ref, reaped_at, reason, name, media_type, size_bytes, digest, supersedes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING id, artifact_id, artifact_ref, reaped_at, reason, name, media_type,
		size_bytes, digest, COALESCE(supersedes, '')`, in.ID, in.ArtifactID, in.ArtifactRef,
		in.ReapedAt, in.Reason, in.Name, in.MediaType, in.SizeBytes, in.Digest, textOrNull(in.Supersedes)).Scan(
		&rec.ID, &rec.ArtifactID, &rec.ArtifactRef, &rec.ReapedAt, &rec.Reason,
		&rec.Name, &rec.MediaType, &rec.SizeBytes, &rec.Digest, &rec.Supersedes)
	if err != nil {
		return ArtifactTombstoneRecord{}, fmt.Errorf("postgres: InsertArtifactTombstone: %w", err)
	}
	return rec, nil
}

// DeleteArtifactByURI is retained as a fail-closed compatibility method.
// Artifact metadata is part of immutable ref resolution and may only lose
// content through artifacts.Store.Reap after a tombstone is durable.
func (s *Store) DeleteArtifactByURI(ctx context.Context, namespaceID, uri string) error {
	return fmt.Errorf("postgres: DeleteArtifactByURI: raw artifact deletion is forbidden; use artifacts.Store.Reap")
}

// artifactRowScanner is satisfied by pgx.Row (both *pgxpool.Pool.QueryRow
// and pgx.Tx.QueryRow return one), letting scanArtifactRow serve every
// querier above.
type artifactRowScanner interface {
	Scan(dest ...any) error
}

func scanArtifactRow(row artifactRowScanner) (ArtifactRecord, error) {
	var (
		id, namespaceID, uri, storageBackend string
		runID, attemptID, mediaType, digest  pgtype.Text
		sizeBytes                            pgtype.Int8
		metadata                             []byte
		createdAt                            pgtype.Timestamptz
	)

	if err := row.Scan(
		&id, &namespaceID, &runID, &attemptID, &uri, &mediaType,
		&sizeBytes, &digest, &storageBackend, &metadata, &createdAt,
	); err != nil {
		return ArtifactRecord{}, err
	}

	return ArtifactRecord{
		ID:             id,
		NamespaceID:    namespaceID,
		RunID:          textOrEmpty(runID),
		AttemptID:      textOrEmpty(attemptID),
		URI:            uri,
		MediaType:      textOrEmpty(mediaType),
		SizeBytes:      int8OrZero(sizeBytes),
		Digest:         textOrEmpty(digest),
		StorageBackend: storageBackend,
		Metadata:       jsonOrEmptyObject(metadata),
		CreatedAt:      tsValue(createdAt),
	}, nil
}

func int8Value(v int64) pgtype.Int8 {
	return pgtype.Int8{Int64: v, Valid: true}
}

func int8OrZero(v pgtype.Int8) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}
