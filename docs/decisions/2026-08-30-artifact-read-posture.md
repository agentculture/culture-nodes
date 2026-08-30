# Artifact read posture (#171)

Status: decided 2026-08-30 (cycle ticket `SCRUM-6`, task t4).
Disposition: artifact reads remain deliberately unauthenticated and bounded by
opaque identifiers, retention, and safe-download headers. This record and the
publish-to-consume example close issue #171.

## Decision

Keep attempt artifact listing and content GETs unauthenticated, consistent with
the run and ledger read surfaces that already expose run outputs. Keep artifact
publication authenticated with the attempt-scoped callback token. An artifact
reference is an identifier, not an authorization credential.

The posture has these limits:

- Readers must know a store-minted attempt ID and artifact name; arbitrary
  filesystem paths and caller-supplied `artifact://` references do not resolve.
- Reads expose only artifacts already associated with an attempt. They do not
  grant listing across namespaces, mutation, deletion, or publication.
- Retention still applies. Reaped content returns not found; an unauthenticated
  read is not a promise of permanent availability.
- Publisher-controlled media types are always served as downloads with MIME
  sniffing disabled and a sandbox content-security policy, so the API origin is
  not an active-content host.
- This is suitable only while the deployment accepts run outputs as readable
  to anyone who can reach the API and knows their identifiers. Secrets must not
  be published as artifacts. A deployment requiring private run outputs needs
  a broader read-authentication decision, not a special artifact-only token.

## Evidence

- `internal/api/artifacts_test.go:232-252` pins unauthenticated listing at 200,
  refuses deletion, and proves that a reference or filesystem path is not
  authorization.
- `internal/api/artifacts.go:171-181` documents and implements the
  unauthenticated attempt listing.
- `internal/api/artifacts.go:220-269` streams a named artifact and applies the
  attachment, `nosniff`, and sandbox response headers.
- `internal/api/artifacts.go:48-72` requires and verifies an attempt-scoped
  bearer token before publication.

## Publish to consume

`examples/artifact-publish-consume/workflow.yaml` is the runnable boundary
example. Its `publish` code node prints its bound payload; the runner service
POSTs that captured stdout as the attempt's `stdout` artifact using the
callback token held at the runner boundary. The node output carries its
operation ID, which is the attempt ID. The next `consume` code node receives
that ID through an ordinary binding and performs an unauthenticated GET of
`/v1alpha1/attempts/{attemptID}/artifacts/stdout`, then compares the bytes with
the original payload.

The credential is intentionally not exposed to either container:
`internal/runners/contextenv.go:54-92` forwards correlation IDs and resolved
input, while `cmd/nodes-runner/main.go:367-381` gives the runner-owned artifact
client custody of publication. The example therefore demonstrates the actual
runner boundary rather than granting a workflow a callback credential.
