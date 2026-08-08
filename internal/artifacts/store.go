package artifacts

import (
	"context"
	"io"
)

// Store is the artifact-store boundary every driver
// (internal/artifacts/postgres, internal/artifacts/s3) and Router
// implement. It is the only way anything in this codebase is meant to read
// or write artifact content -- implementations write to a shared,
// authoritative backing store (PostgreSQL rows, an S3-compatible bucket),
// never to a pod's local filesystem, so a Ref returned by one replica's Put
// resolves to byte-identical content on every other replica's Get (spec
// claim c38).
type Store interface {
	// Put reads r fully, stores its bytes, and records a metadata row
	// (namespace, size, sha256 digest, backend) before returning the Ref
	// that resolves it. meta.NamespaceID scopes the artifact and is
	// required; RunID, AttemptID, Name, and MediaType are optional and
	// descriptive/associative only. The SizeBytes/Digest/Backend/CreatedAt
	// fields of meta are ignored on input -- they are the Store's to set.
	Put(ctx context.Context, meta ArtifactMeta, r io.Reader) (Ref, error)

	// Get resolves ref back to its content and recorded metadata. The
	// returned ReadCloser streams the content while verifying it against
	// the size and digest recorded at Put time, surfacing ErrSizeMismatch
	// or ErrDigestMismatch -- instead of silently handing back corrupted
	// bytes -- once the stream has been read to completion. Callers must
	// Close it. Get returns ErrNotFound if ref does not resolve to an
	// artifact this Store holds.
	Get(ctx context.Context, ref Ref) (io.ReadCloser, ArtifactMeta, error)

	// Stat returns ref's recorded metadata without reading its content.
	// Stat returns ErrNotFound if ref does not resolve to an artifact this
	// Store holds.
	Stat(ctx context.Context, ref Ref) (ArtifactMeta, error)

	// Delete removes ref's content and its metadata row. Deleting a ref
	// that does not exist (or that this Store does not hold) returns
	// ErrNotFound.
	Delete(ctx context.Context, ref Ref) error
}
