# migrations

Numbered SQL migrations for the authoritative PostgreSQL store (prd-spec
§14, persistence model). Applied in filename order by `nodes migrate`
(`internal/store/postgres.Store.Migrate`), which embeds this directory via
`migrations.FS` (see `migrations.go`) and records applied versions in a
`schema_migrations` bookkeeping table it creates on first connect.

## Files

- `0001_namespaces_and_identity.sql` — `namespaces`, `owners`, `actors`,
  `workflow_drafts`, `workflow_versions`.
- `0002_runtime_execution.sql` — `runs`, `tokens`, `node_runs`, `attempts`,
  `runner_operations`, `work_items`, `timers`, `human_tasks`.
- `0003_ledger.sql` — `ledger_records` (immutable; UPDATE/DELETE blocked by
  trigger), `ledger_reviews`, `ledger_projection_versions`.
- `0004_observability.sql` — `artifacts`, `events`, `outbox`,
  `idempotency_keys`.
- `0005_queue_signals.sql` — `queue_signals` (disposable delivery storage
  for the Postgres queue driver, `internal/queue/postgres`; not
  authoritative state -- see the file's header comment).
- `0006_artifact_blobs.sql` — `artifact_blobs` (small-artifact BYTEA store)
  plus the `artifacts(namespace_id, uri)` unique index.
- `0007_ledger_origin_actor_revision.sql` — expand-only: adds
  `ledger_records.origin_actor_revision`, the one prd-spec §10.3 envelope
  field `0003` had no column for.
- `0008_run_output.sql` — expand-only: adds `runs.output`, the workflow
  result a completed run produces, which `0002` had no column for.
- `0009_actor_invocations.sql` — `actor_invocations` (durable async
  invocation tracking) plus the `(state, updated_at)` partial index over
  the `waiting_external` hot set.
- `0010_run_updated_at_indexes.sql` — expand-only: adds
  `runs_namespace_updated_at_idx (namespace_id, updated_at)` and
  `node_runs_namespace_updated_at_idx (namespace_id, updated_at)`, serving
  the `updated_since`/`updated_until` run listing and the cross-run
  node-runs listing (tasks t4/t11/t15).
- `0011_runner_invocations.sql` — expand-only: adds `runner_invocations`
  (durable tracking for runner-protocol operations parked as
  `waiting_external`, task t9) plus the `(namespace_id, next_poll_at)`
  partial index the status sampler claims due operations on and the
  `deadline_timer_id` reverse-lookup index the scheduler's deadline effect
  uses. It is `0009`'s `actor_invocations` for the outbound-sampling
  boundary, and is deliberately distinct from `0002`'s `runner_operations`:
  the two are opposite halves of one life cycle — in-flight tracking while
  no outcome is known, versus the recorded operation and result once one is.
- `0012_attempt_usage.sql` — expand-only: adds `attempts.usage_input_tokens`,
  `usage_output_tokens`, `usage_cost`, `usage_currency` (task t1), the
  per-attempt prd-spec §13.2 telemetry block both completion seams
  (`internal/worker/dispatch.go`'s sync path, `internal/actors/callback.go`'s
  async path) now persist. All four are nullable with no default; an
  attempt that reported no usage stays NULL, not a fabricated zero.
- `0013_run_metadata.sql` — expand-only: adds `runs.name`, `runs.description`,
  `runs.category` (task t3), all nullable with no default. `name`/
  `description` are operator-given at creation only (`internal/api/runs.go`'s
  `handleCreateRun`); `category` alone is retaggable afterward via
  `PATCH /v1alpha1/runs/{id}` (`handlePatchRun`) per frame decision q4.

## Policy

Migrations are additive-first (expand-contract). See
`docs/adr/0002-migration-policy.md` for the full policy, the N-1 binary
compatibility promise, and the k8s Job migrate-before-rollout pattern.

## Adding a migration

1. Add `NNNN_description.sql` with the next number. Never edit a file that
   has shipped.
2. Prefer additive changes (new table, new nullable/defaulted column, new
   index `CONCURRENTLY` where practical). A destructive change (drop column,
   drop table, tighten a constraint) may only ship at least one release
   (N+1) after the last binary version that still reads/writes the old
   shape — see the ADR.
3. Run `nodes migrate` against a scratch database and add/extend a Go test
   in `internal/store/postgres` covering the new shape.
