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

