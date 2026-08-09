# ADR 0002: PostgreSQL migration policy — expand-contract and N-1 binary compatibility

- Status: accepted
- Date: 2026-08-09
- Task: t6 (Postgres store: pgx schema, migrations with expand-contract and N-1 binary compatibility)

## Context

PostgreSQL is the authoritative store for the control plane
(`docs/initial-design/culture-nodes-prd-spec.md` §12.2, §14). Task t22
(Helm chart) runs `nodes migrate` as a pre-install/pre-upgrade Job ahead of
new pods receiving traffic, and production runs `api`, `scheduler`, and
`worker` as separately-scaled Deployments (§19.3). That combination means,
during every rollout, there is a window where:

- the schema has already been migrated to version N (the migration Job runs
  first and blocks pod readiness until it succeeds), while
- some pods are still running the previous binary, version N-1, until the
  rolling update finishes replacing them.

A migration that a version-N-1 binary cannot tolerate — a renamed column, a
dropped table, a `NOT NULL` added without a backfill — breaks every pod that
has not yet rolled over. This ADR fixes the rule that prevents that: schema
changes are additive-first, and every migration that ships must still let
the immediately preceding binary version run correctly against it.

## Decision

### Expand-contract, not in-place breaking change

Every migration is one of two kinds:

1. **Expand** — purely additive: new table, new nullable or
   default-bearing column, new index, new constraint that only tightens
   rows the old binary never writes. An expand migration is always safe to
   ship alongside the previous binary version, because the previous binary
   simply does not know the new shape exists and keeps working against the
   columns/tables it already understands.
2. **Contract** — destructive: drop column, drop table, rename anything,
   tighten a constraint the old binary's writes would violate, or change a
   column's type/meaning. A contract migration may only ship at least one
   full release **after** the last binary version whose code still reads or
   writes the old shape. Concretely: if binary version N stops using a
   column, the migration that drops that column ships no earlier than N+1.

In practice this means every schema change starts as an expand migration.
When a contract migration becomes appropriate, it lands in its own
migration file, sequenced after the release that stopped needing the old
shape, never bundled into the same migration as the code change that could
still be running against it.

Renames are always modeled as expand-then-contract: add the new
column/table, migrate/dual-write from application code, cut reads over, and
only then (≥ N+1) drop the old one. There is no single-migration "rename"
in this schema.

### N-1 binary compatibility promise

At every point after a migration ships, the immediately preceding tagged
binary version must still start up and serve traffic correctly against the
post-migration schema. This is what makes a rolling upgrade — where old and
new pods run against the same database simultaneously — safe. It is also
what makes rollback safe: rolling back to N-1 after a schema migration to N
has already applied must not require an emergency schema rollback.

This promise does **not** extend to N-2 or earlier: once a contract
migration ships, only the immediately preceding version is guaranteed
compatible, and only for the duration of that one rollout.

`scripts/n1-compat.sh` (below) is the enforcement mechanism: it is meant to
run in CI on every PR that touches `migrations/`, checking out the binary
built from the previous git tag and smoke-testing it against a freshly
migrated (current) schema.

### Migrate-before-rollout via a k8s Job

`nodes migrate` (`cmd/nodes/migrate.go`, backed by
`internal/store/postgres.Store.Migrate`) is a standalone subcommand that
takes a `NODES_DATABASE_URL` (or `--database-url`), applies every migration
in `migrations/` that is not yet recorded in the `schema_migrations`
bookkeeping table, and exits. It has no other side effects and does not
start the API/scheduler/worker roles.

Task t22's Helm chart runs this as a `pre-install`/`pre-upgrade` hook Job:
Kubernetes blocks the new Deployment rollout until the Job completes
successfully, so the schema is always at version N before any pod running
binary N starts, and — because migrations are additive-first — the
still-running binary N-1 pods keep working unaffected against the same
now-migrated schema until the rolling update replaces them too.

`Migrate` is idempotent: it re-checks `schema_migrations` before applying
each file, so re-running the Job (retried pod, re-triggered hook, or a
manual `nodes migrate` for local development) after a partial or complete
success is always safe and a no-op past the first successful application of
each file (`TestMigrateIsIdempotent`,
`internal/store/postgres/migrate_test.go`).

### Immutability is not exempt from this policy

`ledger_records` (`migrations/0003_ledger.sql`) has no `UPDATE`/`DELETE`
path at all, enforced by a trigger, not just by convention
(`prd-spec` §10.3). That is a stronger guarantee than expand-contract, not
a different one: the table itself can still gain new nullable columns
(expand) without breaking the immutability guarantee, but no migration may
ever add an `UPDATE`/`DELETE` path back in, and no migration may alter the
meaning of an existing column's already-written values.

## Consequences

- Every table in `migrations/` carries `namespace_id` from the very first
  migration (`0001_namespaces_and_identity.sql`), specifically so that
  namespace scoping never needs a contract migration to retrofit.
- Application code that reads a column must tolerate that column being
  absent from rows written by a binary running against a schema one
  expand-migration behind (rare, since expand migrations are additive by
  definition — but new nullable columns do mean `NULL`/zero-value handling
  is a real code-review question, not a formality).
- A destructive change always costs at least one extra release cycle
  (ship the expand step, wait for every N-1 binary in production to roll
  past the version that still needs the old shape, then ship the contract
  step). There is no fast path for "just drop the column now."
- CI (task t1's `go.yml`, extended by t22's kind-cluster CI) is where
  `scripts/n1-compat.sh` is meant to run automatically; wiring that trigger
  into the workflow file is left to whichever task next touches CI
  composition, so as not to widen this task's diff into shared CI files.

## Alternatives considered

- **Blue/green schema per release** — a full second schema/database per
  version, cut over atomically. Rejected: PostgreSQL is meant to stay a
  single small, understandable authoritative store for the MVP
  (`prd-spec` §17.6); a second schema doubles operational surface for a
  problem expand-contract already solves.
- **Feature-flag every schema-dependent code path** instead of relying on
  N-1 compatibility — technically stronger (N-2, N-3 compatibility too) but
  a large ongoing tax on every change for a guarantee this product doesn't
  need yet. Revisit if a future requirement (e.g. multi-region gradual
  rollout) needs more than one version of headroom.
