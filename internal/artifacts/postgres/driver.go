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

	var data []byte
	if err := d.store.Pool().QueryRow(ctx, `SELECT data FROM artifact_blobs WHERE id = $1`, id).Scan(&data); err != nil {
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

// Delete removes ref's metadata row and (via artifact_blobs.id's ON DELETE
// CASCADE foreign key, migrations/0006_artifact_blobs.sql) its bytes in the
// same statement. Like Get, it refuses -- with artifacts.ErrNotFound -- a
// ref recorded under a different backend, so calling Delete on the wrong
// driver directly can never silently delete an S3-held artifact's metadata
// while leaving its bytes orphaned in the bucket.
func (d *Driver) Delete(ctx context.Context, ref artifacts.Ref) error {
	namespaceID, _, err := artifacts.ParseRef(ref)
	if err != nil {
		return err
	}

	row, err := d.store.GetArtifactByURI(ctx, namespaceID, string(ref))
	if err != nil {
		if errors.Is(err, pgstore.ErrNotFound) {
			return artifacts.ErrNotFound
		}
		return fmt.Errorf("artifacts/postgres: Delete: %w", err)
	}
	if row.StorageBackend != string(artifacts.BackendPostgres) {
		return artifacts.ErrNotFound
	}

	if err := d.store.DeleteArtifactByURI(ctx, namespaceID, string(ref)); err != nil {
		if errors.Is(err, pgstore.ErrNotFound) {
			return artifacts.ErrNotFound
		}
		return fmt.Errorf("artifacts/postgres: Delete: %w", err)
	}
	return nil
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
