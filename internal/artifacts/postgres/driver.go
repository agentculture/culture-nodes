// Package postgres implements the small-artifact internal/artifacts.Store
// driver: artifact bytes live directly in PostgreSQL
// (migrations/0006_artifact_blobs.sql, a BYTEA column keyed by the same id
// as the artifacts metadata row), behind a hard, configurable size cap --
// callers whose payload exceeds it get artifacts.ErrTooLarge and are
// expected to use internal/artifacts/s3 instead, or let
// internal/artifacts.Router make that decision for them automatically.
//
// This driver suits small, frequent artifacts (a short stdout tail, a JSON
// diff, a brief report) where a round trip to the object store would be
// pure overhead. It is exactly as pod-agnostic as the S3 driver: every Put
// commits both the bytes and their metadata to the shared, authoritative
// Postgres database (docs/initial-design/culture-nodes-prd-spec.md §14),
// never to any pod's local disk, so any replica reading the returned
// artifacts.Ref back gets byte-identical, digest-verified content (task
// t15, spec claim c38).
package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/artifacts"
	"github.com/agentculture/culture-nodes/internal/store"
	pgstore "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// DefaultCapBytes is the small-artifact size cap Driver uses when
// constructed with capBytes <= 0: 1 MiB.
const DefaultCapBytes int64 = 1 << 20

// Driver is the Postgres-backed internal/artifacts.Store implementation.
// It is safe for concurrent use: it carries no mutable state of its own,
// only a pooled *pgstore.Store connection and an immutable cap.
type Driver struct {
	store    *pgstore.Store
	capBytes int64
}

var _ artifacts.Store = (*Driver)(nil)

// New returns a Driver backed by pgStore, capping Put payloads at
// capBytes. capBytes <= 0 selects DefaultCapBytes.
func New(pgStore *pgstore.Store, capBytes int64) *Driver {
	if capBytes <= 0 {
		capBytes = DefaultCapBytes
	}
	return &Driver{store: pgStore, capBytes: capBytes}
}

// Put reads all of r, up to capBytes+1 bytes (enough to detect an oversize
// payload without ever trusting or requiring a caller-supplied length), and
// -- if it fits at or under the cap -- writes the bytes and their metadata
// row in one transaction: either both commit or neither does, because
// artifact_blobs.id has a foreign key on artifacts.id
// (migrations/0006_artifact_blobs.sql).
func (d *Driver) Put(ctx context.Context, meta artifacts.ArtifactMeta, r io.Reader) (artifacts.Ref, error) {
	data, err := io.ReadAll(io.LimitReader(r, d.capBytes+1))
	if err != nil {
		return "", fmt.Errorf("artifacts/postgres: Put: read: %w", err)
	}
	if int64(len(data)) > d.capBytes {
		return "", fmt.Errorf("artifacts/postgres: Put: payload is at least %d bytes, over the %d byte small-artifact cap: %w",
			len(data), d.capBytes, artifacts.ErrTooLarge)
	}

	sum := sha256.Sum256(data)
	digest := artifacts.DigestPrefix + hex.EncodeToString(sum[:])
	id := store.NewULID()
	ref := artifacts.NewRef(meta.NamespaceID, id)

	tx, err := d.store.Pool().Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("artifacts/postgres: Put: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	if _, err := pgstore.InsertArtifactTx(ctx, tx, pgstore.InsertArtifactInput{
		ID:             id,
		NamespaceID:    meta.NamespaceID,
		RunID:          meta.RunID,
		AttemptID:      meta.AttemptID,
		URI:            string(ref),
		MediaType:      meta.MediaType,
		SizeBytes:      int64(len(data)),
		Digest:         digest,
		StorageBackend: string(artifacts.BackendPostgres),
		Metadata:       artifacts.EncodeExtraMetadata(meta.Name),
	}); err != nil {
		return "", fmt.Errorf("artifacts/postgres: Put: insert metadata: %w", err)
	}

	if _, err := tx.Exec(ctx, `INSERT INTO artifact_blobs (id, data) VALUES ($1, $2)`, id, data); err != nil {
		return "", fmt.Errorf("artifacts/postgres: Put: insert blob: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("artifacts/postgres: Put: commit: %w", err)
	}

	return ref, nil
}

// Get resolves ref's metadata and, only if it is recorded as this driver's
// backend, its bytes -- returning artifacts.ErrNotFound for a ref this
// driver does not hold (internal/artifacts.Router never makes that mistake;
// this guards a driver reached directly instead of through Router). The
// returned ReadCloser verifies size and digest as it streams (see
// artifacts.NewVerifyingReadCloser).
func (d *Driver) Get(ctx context.Context, ref artifacts.Ref) (io.ReadCloser, artifacts.ArtifactMeta, error) {
	namespaceID, id, err := artifacts.ParseRef(ref)
	if err != nil {
		return nil, artifacts.ArtifactMeta{}, err
	}

	row, err := d.store.GetArtifactByURI(ctx, namespaceID, string(ref))
	if err != nil {
		if errors.Is(err, pgstore.ErrNotFound) {
			return nil, artifacts.ArtifactMeta{}, artifacts.ErrNotFound
		}
		return nil, artifacts.ArtifactMeta{}, fmt.Errorf("artifacts/postgres: Get: %w", err)
	}
	if row.StorageBackend != string(artifacts.BackendPostgres) {
		return nil, artifacts.ArtifactMeta{}, artifacts.ErrNotFound
	}
	if tomb, tombErr := d.store.GetArtifactTombstone(ctx, row.ID); tombErr == nil {
		meta := metaFromRecord(row)
		return nil, meta, &artifacts.ReapedError{Tombstone: tombstoneFromRecord(tomb, meta)}
	} else if !errors.Is(tombErr, pgstore.ErrNotFound) {
		return nil, artifacts.ArtifactMeta{}, fmt.Errorf("artifacts/postgres: Get: tombstone: %w", tombErr)
	}

	var data []byte
	if err := d.store.Pool().QueryRow(ctx, `SELECT data FROM artifact_blobs WHERE id = $1`, id).Scan(&data); err != nil {
		// Reap may have committed between the first tombstone lookup and this
		// content read. Resolve that race to the tombstone, never a bare miss.
		if tomb, tombErr := d.store.GetArtifactTombstone(ctx, row.ID); tombErr == nil {
			meta := metaFromRecord(row)
			return nil, meta, &artifacts.ReapedError{Tombstone: tombstoneFromRecord(tomb, meta)}
		}
		return nil, artifacts.ArtifactMeta{}, fmt.Errorf("artifacts/postgres: Get: blob: %w", err)
	}

	meta := metaFromRecord(row)
	rc := artifacts.NewVerifyingReadCloser(io.NopCloser(bytes.NewReader(data)), meta.SizeBytes, meta.Digest)
	return rc, meta, nil
}

// Stat returns ref's recorded metadata regardless of which backend it
// names -- see the internal/artifacts.Router doc comment for why Stat is
// backend-agnostic even though Get and Delete are not.
func (d *Driver) Stat(ctx context.Context, ref artifacts.Ref) (artifacts.ArtifactMeta, error) {
	namespaceID, _, err := artifacts.ParseRef(ref)
	if err != nil {
		return artifacts.ArtifactMeta{}, err
	}

	row, err := d.store.GetArtifactByURI(ctx, namespaceID, string(ref))
	if err != nil {
		if errors.Is(err, pgstore.ErrNotFound) {
			return artifacts.ArtifactMeta{}, artifacts.ErrNotFound
		}
		return artifacts.ArtifactMeta{}, fmt.Errorf("artifacts/postgres: Stat: %w", err)
	}
	return metaFromRecord(row), nil
}

// Delete refuses raw removal. Retention code must use Reap so ledger refs
// continue to resolve to an immutable tombstone.
func (d *Driver) Delete(ctx context.Context, ref artifacts.Ref) error {
	return artifacts.ErrDeleteForbidden
}

func (d *Driver) Reap(ctx context.Context, ref artifacts.Ref, reason string, reapedAt time.Time) (artifacts.Tombstone, error) {
	if strings.TrimSpace(reason) == "" || reapedAt.IsZero() {
		return artifacts.Tombstone{}, fmt.Errorf("artifacts/postgres: Reap: reason and reapedAt are required")
	}
	namespaceID, id, err := artifacts.ParseRef(ref)
	if err != nil {
		return artifacts.Tombstone{}, err
	}
	row, err := d.store.GetArtifactByURI(ctx, namespaceID, string(ref))
	if err != nil {
		if errors.Is(err, pgstore.ErrNotFound) {
			return artifacts.Tombstone{}, artifacts.ErrNotFound
		}
		return artifacts.Tombstone{}, err
	}
	if row.StorageBackend != string(artifacts.BackendPostgres) {
		return artifacts.Tombstone{}, artifacts.ErrNotFound
	}
	meta := metaFromRecord(row)
	tx, err := d.store.Pool().Begin(ctx)
	if err != nil {
		return artifacts.Tombstone{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT id FROM artifacts WHERE id=$1 FOR UPDATE`, id); err != nil {
		return artifacts.Tombstone{}, err
	}
	if existing, tombErr := pgstore.GetArtifactTombstoneTx(ctx, tx, id); tombErr == nil {
		return artifacts.Tombstone{}, &artifacts.ReapedError{Tombstone: tombstoneFromRecord(existing, meta)}
	} else if !errors.Is(tombErr, pgstore.ErrNotFound) {
		return artifacts.Tombstone{}, tombErr
	}
	rec, err := pgstore.InsertArtifactTombstoneTx(ctx, tx, pgstore.InsertArtifactTombstoneInput{
		ID: store.NewULID(), ArtifactID: id, ArtifactRef: string(ref), ReapedAt: reapedAt,
		Reason: reason, Name: meta.Name, MediaType: meta.MediaType, SizeBytes: meta.SizeBytes, Digest: meta.Digest,
	})
	if err != nil {
		return artifacts.Tombstone{}, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM artifact_blobs WHERE id=$1`, id); err != nil {
		return artifacts.Tombstone{}, fmt.Errorf("artifacts/postgres: Reap: content: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return artifacts.Tombstone{}, err
	}
	return tombstoneFromRecord(rec, meta), nil
}

func metaFromRecord(row pgstore.ArtifactRecord) artifacts.ArtifactMeta {
	return artifacts.ArtifactMeta{
		NamespaceID: row.NamespaceID,
		RunID:       row.RunID,
		AttemptID:   row.AttemptID,
		Name:        artifacts.DecodeExtraMetadataName(row.Metadata),
		MediaType:   row.MediaType,
		SizeBytes:   row.SizeBytes,
		Digest:      row.Digest,
		Backend:     artifacts.Backend(row.StorageBackend),
		CreatedAt:   row.CreatedAt,
	}
}

func tombstoneFromRecord(row pgstore.ArtifactTombstoneRecord, meta artifacts.ArtifactMeta) artifacts.Tombstone {
	meta.Name = row.Name
	meta.MediaType = row.MediaType
	meta.SizeBytes = row.SizeBytes
	meta.Digest = row.Digest
	return artifacts.Tombstone{
		ID: row.ID, Ref: artifacts.Ref(row.ArtifactRef), ReapedAt: row.ReapedAt,
		Reason: row.Reason, Meta: meta, Supersedes: row.Supersedes,
	}
}

// ListByAttempt implements artifacts.AttemptLister over the authoritative
// metadata table this driver shares with the object backend: the listing
// covers every artifact recorded for the attempt, whichever backend holds
// its bytes. Reaped artifacts keep their metadata row and appear in the
// listing; a Get on their ref reports the tombstone.
func (d *Driver) ListByAttempt(ctx context.Context, attemptID string) ([]artifacts.Listed, error) {
	rows, err := d.store.ListArtifactsByAttempt(ctx, attemptID)
	if err != nil {
		return nil, fmt.Errorf("artifacts/postgres: ListByAttempt: %w", err)
	}
	out := make([]artifacts.Listed, 0, len(rows))
	for _, row := range rows {
		out = append(out, artifacts.Listed{Ref: artifacts.Ref(row.URI), Meta: metaFromRecord(row)})
	}
	return out, nil
}

var _ artifacts.AttemptLister = (*Driver)(nil)
