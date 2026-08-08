package artifacts

import "time"

// Backend names which physical store holds an artifact's bytes. It is
// recorded on the artifacts metadata row at Put time (in Postgres, via
// internal/store/postgres.InsertArtifact's StorageBackend column) and is
// what Router uses to send Get/Delete to the right driver.
type Backend string

const (
	// BackendPostgres marks an artifact stored by internal/artifacts/postgres:
	// bytes live in the artifact_blobs table
	// (migrations/0006_artifact_blobs.sql).
	BackendPostgres Backend = "postgres"
	// BackendS3 marks an artifact stored by internal/artifacts/s3: bytes
	// live in an S3-compatible bucket (MinIO in dev, AWS S3 in production).
	BackendS3 Backend = "s3"
)

// ArtifactMeta describes one artifact. NamespaceID, RunID, AttemptID, Name,
// and MediaType are the caller-supplied Put input (RunID, AttemptID, Name,
// and MediaType may all be left "" when not applicable); SizeBytes, Digest,
// Backend, and CreatedAt are filled in by the Store and are only meaningful
// on a value returned from Put, Get, or Stat.
type ArtifactMeta struct {
	// NamespaceID scopes the artifact to a tenant/installation (prd-spec
	// §14). Required.
	NamespaceID string
	// RunID optionally associates the artifact with the run that produced
	// it.
	RunID string
	// AttemptID optionally associates the artifact with the specific
	// dispatch attempt that produced it.
	AttemptID string
	// Name is a caller-chosen descriptive label (e.g. "stdout", "diff.json")
	// -- purely informational, not part of Ref.
	Name string
	// MediaType is the artifact's content type (e.g. "application/json",
	// "text/plain"), used as the object's Content-Type by the S3 driver.
	MediaType string

	// SizeBytes is the exact byte count Put measured while storing the
	// content.
	SizeBytes int64
	// Digest is "sha256:<hex>" of the stored content, in the same format
	// as DigestPrefix.
	Digest string
	// Backend names which driver's Store actually holds this artifact's
	// bytes.
	Backend Backend
	// CreatedAt is when the metadata row was written.
	CreatedAt time.Time
}
