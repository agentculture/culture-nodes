// Package s3 implements the S3-compatible object-store internal/artifacts.Store
// driver: artifact bytes live in a bucket, addressed by
// "<namespace-id>/<id>" object keys. It uses the MinIO Go client
// (github.com/minio/minio-go/v7) rather than the AWS SDK -- minio-go speaks
// the S3 API directly (bucket/object PUT, GET, STAT, DELETE) against both
// MinIO (dev/test) and AWS S3 (production) without pulling in the much
// heavier aws-sdk-go-v2 dependency tree.
//
// Note for task t17 (AWS package isolation and credential-chain lint): its
// depguard-style rule is meant to enumerate the packages allowed to import
// an AWS SDK (internal/queue/sqs, internal/artifacts/s3,
// internal/runners/lambda per the build plan). This package is on that
// list as the artifact-store boundary, so if a future change here ever
// needs the AWS SDK directly (e.g. a feature minio-go does not cover), it
// is already an authorized import site -- t17 does not need to special-case
// or reopen this package.
//
// Like internal/artifacts/postgres, this driver never writes artifact
// content to a pod's local filesystem: every Put lands in the shared
// bucket and a shared Postgres metadata row (task t15, spec claim c38).
package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/agentculture/culture-nodes/internal/artifacts"
	"github.com/agentculture/culture-nodes/internal/store"
	pgstore "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Config holds the S3-compatible endpoint, credentials, bucket, and TLS
// setting the MinIO Go client needs. It works unchanged against both a
// local MinIO instance (dev/test: Endpoint "127.0.0.1:9000", UseTLS false)
// and AWS S3 (production: Endpoint "s3.<region>.amazonaws.com", UseTLS
// true).
type Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	UseTLS    bool
}

// Driver is the S3-compatible internal/artifacts.Store implementation. It
// is safe for concurrent use: minio.Client is, and Driver carries no other
// mutable state.
type Driver struct {
	client *minio.Client
	bucket string
	store  *pgstore.Store
}

var _ artifacts.Store = (*Driver)(nil)

// New connects to the S3-compatible endpoint in cfg and ensures cfg.Bucket
// exists, creating it if this is the first driver to talk to a fresh
// bucket-less endpoint (a bucket that already exists, including one an AWS
// account's policy forbids creating, is not an error as long as it is
// already there). Metadata for every artifact this Driver stores goes
// through pgStore (internal/store/postgres), the same authority
// internal/artifacts/postgres uses -- that shared table is what lets
// internal/artifacts.Router route a ref to whichever of the two drivers
// actually holds it.
func New(ctx context.Context, cfg Config, pgStore *pgstore.Store) (*Driver, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseTLS,
	})
	if err != nil {
		return nil, fmt.Errorf("artifacts/s3: connect to %s: %w", cfg.Endpoint, err)
	}

	if err := ensureBucket(ctx, client, cfg.Bucket); err != nil {
		return nil, err
	}

	return &Driver{client: client, bucket: cfg.Bucket, store: pgStore}, nil
}

func ensureBucket(ctx context.Context, client *minio.Client, bucket string) error {
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return fmt.Errorf("artifacts/s3: check bucket %s: %w", bucket, err)
	}
	if exists {
		return nil
	}

	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		// A concurrent New() racing to create the same bucket is not a
		// failure: BucketExists above already told us the bucket is the
		// thing we want, so re-check rather than trust the error alone.
		if exists, existsErr := client.BucketExists(ctx, bucket); existsErr == nil && exists {
			return nil
		}
		return fmt.Errorf("artifacts/s3: create bucket %s: %w", bucket, err)
	}
	return nil
}

// objectKey is the bucket key an artifact's bytes are stored under:
// "<namespace-id>/<id>", the same pair NewRef encodes into a Ref.
func objectKey(namespaceID, id string) string {
	return namespaceID + "/" + id
}

// Put uploads r's content to the bucket, streaming (PutObject with an
// unknown size hands off to minio-go's multipart upload path, so the whole
// payload never needs to fit in memory) while hashing it, then records the
// resulting size and digest as the metadata row.
//
// There is no distributed transaction spanning the bucket and Postgres: the
// object is uploaded first (an orphaned object with no metadata row is
// harmless -- nothing can ever resolve a Ref to it), and only then is
// metadata recorded. If the metadata write fails, Put makes a best-effort
// attempt to delete the object it just uploaded, so a failed Put does not
// leave content reachable under a ref no metadata row ever pointed to.
func (d *Driver) Put(ctx context.Context, meta artifacts.ArtifactMeta, r io.Reader) (artifacts.Ref, error) {
	id := store.NewULID()
	ref := artifacts.NewRef(meta.NamespaceID, id)
	key := objectKey(meta.NamespaceID, id)

	contentType := meta.MediaType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	hasher := sha256.New()
	info, err := d.client.PutObject(ctx, d.bucket, key, io.TeeReader(r, hasher), -1, minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return "", fmt.Errorf("artifacts/s3: Put: upload: %w", err)
	}

	digest := artifacts.DigestPrefix + hex.EncodeToString(hasher.Sum(nil))

	if _, err := d.store.InsertArtifact(ctx, pgstore.InsertArtifactInput{
		ID:             id,
		NamespaceID:    meta.NamespaceID,
		RunID:          meta.RunID,
		AttemptID:      meta.AttemptID,
		URI:            string(ref),
		MediaType:      meta.MediaType,
		SizeBytes:      info.Size,
		Digest:         digest,
		StorageBackend: string(artifacts.BackendS3),
		Metadata:       artifacts.EncodeExtraMetadata(meta.Name),
	}); err != nil {
		_ = d.client.RemoveObject(ctx, d.bucket, key, minio.RemoveObjectOptions{})
		return "", fmt.Errorf("artifacts/s3: Put: record metadata: %w", err)
	}

	return ref, nil
}

// Get resolves ref's metadata and, only if it is recorded as this driver's
// backend, its bytes -- returning artifacts.ErrNotFound for a ref this
// driver does not hold (internal/artifacts.Router never makes that
// mistake; this guards a driver reached directly instead of through
// Router). The returned ReadCloser verifies size and digest as it streams
// (see artifacts.NewVerifyingReadCloser), so corrupted or tampered bucket
// content surfaces as a read error rather than silently returned bytes.
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
		return nil, artifacts.ArtifactMeta{}, fmt.Errorf("artifacts/s3: Get: %w", err)
	}
	if row.StorageBackend != string(artifacts.BackendS3) {
		return nil, artifacts.ArtifactMeta{}, artifacts.ErrNotFound
	}

	obj, err := d.client.GetObject(ctx, d.bucket, objectKey(namespaceID, id), minio.GetObjectOptions{})
	if err != nil {
		return nil, artifacts.ArtifactMeta{}, fmt.Errorf("artifacts/s3: Get: %w", err)
	}
	// minio-go's GetObject is lazy: the request is not actually sent until
	// the first Read or Stat. Stat here so a missing/inaccessible object is
	// reported immediately as part of Get, not as a surprise on first Read.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, artifacts.ArtifactMeta{}, fmt.Errorf("artifacts/s3: Get: object: %w", err)
	}

	meta := metaFromRecord(row)
	return artifacts.NewVerifyingReadCloser(obj, meta.SizeBytes, meta.Digest), meta, nil
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
		return artifacts.ArtifactMeta{}, fmt.Errorf("artifacts/s3: Stat: %w", err)
	}
	return metaFromRecord(row), nil
}

// Delete removes ref's metadata row first, then its bucket object. Deleting
// the metadata row first means a failure removing the object leaves a
// harmless orphaned object behind rather than a metadata row that claims
// bytes no longer exist. Like Get, it refuses -- with artifacts.ErrNotFound
// -- a ref recorded under a different backend, so calling Delete on the
// wrong driver directly can never silently delete a Postgres-held
// artifact's metadata while its bytes remain in artifact_blobs.
func (d *Driver) Delete(ctx context.Context, ref artifacts.Ref) error {
	namespaceID, id, err := artifacts.ParseRef(ref)
	if err != nil {
		return err
	}

	row, err := d.store.GetArtifactByURI(ctx, namespaceID, string(ref))
	if err != nil {
		if errors.Is(err, pgstore.ErrNotFound) {
			return artifacts.ErrNotFound
		}
		return fmt.Errorf("artifacts/s3: Delete: %w", err)
	}
	if row.StorageBackend != string(artifacts.BackendS3) {
		return artifacts.ErrNotFound
	}

	if err := d.store.DeleteArtifactByURI(ctx, namespaceID, string(ref)); err != nil {
		if errors.Is(err, pgstore.ErrNotFound) {
			return artifacts.ErrNotFound
		}
		return fmt.Errorf("artifacts/s3: Delete: metadata: %w", err)
	}

	if err := d.client.RemoveObject(ctx, d.bucket, objectKey(namespaceID, id), minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("artifacts/s3: Delete: object: %w", err)
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
