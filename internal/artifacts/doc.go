// Package artifacts defines the pod-agnostic artifact-store boundary (task
// t15; spec claim c38) and the primitives every driver and caller shares:
// the Store interface, the Ref/Backend/ArtifactMeta types, verification
// errors, a streaming digest/size-verifying reader, and Router, a composite
// Store that dispatches by payload size and recorded backend.
//
// Concrete drivers live in subpackages, never here, so this package stays
// free of any storage-backend dependency:
//
//   - internal/artifacts/postgres -- small artifacts, bytes in Postgres
//     itself, capped by size (default 1 MiB).
//   - internal/artifacts/s3 -- large artifacts, bytes in an S3-compatible
//     bucket (MinIO in dev, AWS S3 in production).
//
// Every driver writes to a shared, authoritative backing store (PostgreSQL
// rows for metadata; PostgreSQL BYTEA or an S3-compatible bucket for
// content) and never to a pod's local filesystem. That is the whole of the
// pod-agnostic guarantee: a Ref returned by one replica's Put resolves to
// byte-identical, digest-verified content on every other replica's Get,
// because there is no per-pod state involved at any point. A ref never
// carries or implies a filesystem path -- it is always
// "artifact://<namespace-id>/<id>" (see ref.go), resolved only through a
// Store.
package artifacts
