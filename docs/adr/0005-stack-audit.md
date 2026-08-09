# ADR 0005: Phase-1 stack audit against PRD §1

- **Status:** accepted
- **Date:** 2026-08-09
- **Task:** t27 (Phase-1 vertical slice: assemble and prove)
- **Audited against:** `docs/initial-design/culture-nodes-prd-spec.md` §1's
  "Recommended stack" table
- **Audited artifacts:** `go.mod` (module `github.com/agentculture/culture-nodes`,
  `go 1.26.5`), `web/package.json` (`culture-nodes-web`), `api/openapi/openapi.yaml`,
  `deploy/`

## Context

The PRD's §1 table is a set of technology decisions the build was supposed
to honour. Phase 1 is now assembled, so this ADR is the point at which each
row is checked against what is actually in the repository — not against what
anyone remembers deciding. Every row below names the file that proves its
verdict.

A deviation recorded here is a deviation the project owns. A deviation
nobody wrote down is drift, and this repo's CLAUDE.md says so outright:
"Record deviations from the PRD explicitly, don't drift silently."

## Audit

| PRD §1 row | Decision | What is in the repo | Verdict |
| --- | --- | --- | --- |
| Control-plane runtime | Go | `go.mod`: `go 1.26.5`; one binary at `cmd/nodes` | **match** |
| Web application | TypeScript, React, Vite | `web/package.json`: `react` ^18.3.1, `vite` ^6.4.3, `typescript` ~5.6.3 | **match** |
| Graph canvas | React Flow, using the shared Culture/AgentCulture graph components | `@xyflow/react` ^12.11.2; the shared layer is a pinned point-in-time extraction under `web/src/culture-design/` (ADR 0001) | **match, with ADR 0001's extraction caveat** |
| Live execution | Node/edge state overlays from committed runtime events | `internal/api/events.go` streams the `events` table; `web/src/hooks` consumes it | **match** |
| UI system | Culture/AgentCulture tokens and components; Tailwind and Radix **only where the shared system uses or permits them** | No `tailwindcss`, no `@radix-ui/*` in `web/package.json`; styling is `web/src/culture-design/tokens.css` plus hand-written CSS | **match** — the shared system uses neither, so neither is present. The PRD permits them conditionally; the condition is not met, so their absence is compliance, not a gap. |
| Automatic graph layout | ELK.js | `elkjs` ^0.9.3 | **match** |
| API | REST/JSON described by OpenAPI 3.1 | `api/openapi/openapi.yaml`: `openapi: 3.1.0`; `internal/api/contract_test.go` holds the handler surface to it | **match** |
| Live UI updates | Server-Sent Events initially | `internal/api/events.go`'s `text/event-stream` handler; exercised end to end by `tests/e2e/slice_test.go` | **match** |
| Authoring | YAML or JSON | `internal/compiler`'s `FormatYAML`/`FormatJSON`; `sigs.k8s.io/yaml` v1.4.0 | **match** |
| Canonical definition | Normalized JSON IR, SHA-256 addressed | `internal/compiler/normalize.go` + `internal/contracts/canonical.go` | **match** |
| Contracts | JSON Schema Draft 2020-12 | `github.com/santhosh-tekuri/jsonschema/v6` v6.0.3; `schemas/**` declare `$schema: .../draft/2020-12/schema` | **match** |
| Conditions | CEL | `github.com/google/cel-go` v0.28.0 | **match** |
| Durable state | PostgreSQL | `github.com/jackc/pgx/v5` v5.10.0; `migrations/` | **match** |
| Local work queue | PostgreSQL work table, leases and `SKIP LOCKED` | `internal/store/postgres/claiming.go`'s `claimWorkSQL` | **match** |
| AWS queue | SQS as a delivery signal; PostgreSQL authoritative | `aws-sdk-go-v2/service/sqs` v1.46.4; `internal/queue/sqs` | **match** |
| Large artifacts | S3-compatible object storage | `github.com/minio/minio-go/v7` v7.2.1 in `internal/artifacts/s3` — **not** `aws-sdk-go-v2/service/s3` | **deviation (accepted)** — see below |
| Work ledger | Append-only typed JSON with provenance, authority, evidence | `internal/ledger`, `migrations/0003_ledger.sql` | **match** |
| Code runner | headspace-cli Docker adapter | `internal/runners/headspace` (real subprocess bridge; live-proved by `tests/e2e/live_test.go`) | **match** |
| Events | CloudEvents 1.0-compatible envelopes | `internal/events/envelope.go` stamps `specversion: "1.0"` | **match** |
| Telemetry | OpenTelemetry | `internal/telemetry` is a package doc stub; **no** `go.opentelemetry.io/*` dependency anywhere in `go.mod` | **absent — deferred to Phase 2** |
| Local packaging | One binary plus PostgreSQL; Docker Compose | `cmd/nodes` (modes `serve`/`scheduler`/`worker`/`all`/…); `deploy/compose/docker-compose.yml` | **match** |
| AWS deployment | ECS/Fargate first, EKS-compatible | `deploy/helm/culture-nodes` (Kubernetes, kind-smoked) and `deploy/aws/worker-iam-policy.json`; **no** ECS task definitions or Fargate service manifests | **deviation (partial)** — see below |
| Arbitrary code execution | External isolated runner; never inside the control-plane process | `internal/runners`' document-in/document-out `Runner` interface; `tests/deploy/compose_test.go` and `tests/deploy/helm_test.go` assert no Docker socket is mounted into any control-plane container | **match** |

## Deviations

### D1 — minio-go instead of the AWS SDK for S3

**What:** `internal/artifacts/s3` drives object storage through
`github.com/minio/minio-go/v7`, not `aws-sdk-go-v2/service/s3`.

**Why it is not drift:** the PRD row says "S3-compatible object storage",
not "the AWS SDK". The artifact boundary has to work against MinIO in
`docker compose`, against a MinIO container in `internal/artifacts`' own
integration tests, and against real S3 in a deployment — one client that
speaks the protocol serves all three, while the AWS SDK would make the local
and test paths the special case. ADR 0004 already treats
`internal/artifacts/s3` as a boundary package under the same isolation lint
as the two SDK-using packages, so the credential-handling discipline is
identical either way.

**Cost:** IRSA/OIDC credentials reach minio-go through
`internal/awsauth`'s chain rather than through the SDK's own resolver, which
is one more piece of code to keep correct (it has its own tests in
`internal/awsauth`).

### D2 — Kubernetes/Helm shipped, ECS/Fargate not

**What:** §1 says "ECS/Fargate first, EKS-compatible". What exists is the
EKS-compatible half: a Helm chart with per-role Deployments, a migration
Job, and a callback Ingress, smoke-tested against `kind` (task t22). There
are no ECS task definitions.

**Why:** the Phase-0/1 implementation issue's Milestone 1 asks for "a
complete Docker Compose profile" and says nothing about ECS; the deployment
target the build plan actually funded was Compose for local and Helm for a
cluster. Writing ECS manifests nobody has deployed would be shipping
untested YAML as evidence.

**Consequence:** "ECS/Fargate first" is **not met**. It is recorded as such
in `docs/acceptance.md` rather than counted as delivered.

### D3 — OpenTelemetry absent

**What:** `internal/telemetry` exists as a package doc and nothing else. No
tracer, no meter, no exporter, no dependency.

**Why:** §1 names OpenTelemetry, but neither Milestone 0 nor Milestone 1 of
the implementation issue lists an observability deliverable, and no
acceptance criterion mentions traces or metrics. Adding an instrumentation
layer nothing asserts on would have been unproven surface area.

**Consequence:** the §1 telemetry row is **not met** and is Phase-2 work.
What Phase 1 does have instead is the durable `events` table — every
committed transition emits one, with a per-run monotonic sequence — which is
an audit trail, not telemetry, and does not substitute for one.

## Things checked that the PRD does not require

- **TanStack Query is absent.** `web/package.json` lists no `@tanstack/*`
  package; data fetching is hand-written hooks under `web/src/hooks` against
  `web/src/api`. §1 names no data-fetching library, so this is a free choice,
  not a deviation — recorded because "is TanStack Query there?" is a
  reasonable question to ask of a React app and the answer should not require
  reading `package.json`.
- **No Docker socket anywhere in the control plane.** Asserted, not assumed:
  `tests/deploy/compose_test.go` and `tests/deploy/helm_test.go` both grep the
  rendered deployment for socket mounts.

## Decision

Accept D1 permanently (it is the better engineering choice for the stated
requirement). Accept D2 and D3 as **Phase-1 scope boundaries**, not as
technology reversals: ECS manifests and OpenTelemetry instrumentation remain
the intended targets and are recorded as unmet in `docs/acceptance.md`.
Revisit each with its own ADR when it is built.
