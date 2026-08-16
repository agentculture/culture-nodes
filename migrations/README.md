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
  Coverage is honestly narrowed (issue #32, the attempts-evidence frame's
  h24): an attempt reports usage only when its bridge held a parseable
  terminal result at completion — carried on the §13.2 sync result, the
  `completed`/`failed` callback payloads (ADR 0008), and the sync 500 error
  body (task t5) alike. **Cancelled attempts and result-less crashes or
  timeouts stay unreported**: a SIGTERM'd session emits no terminal event,
  a bridge that crashed or timed out before its CLI produced a result holds
  nothing to report, and in every such case all four columns stay NULL —
  `attempts_not_reported` in the rollups (`internal/store/postgres/
  usage_rollup.go`) is where that burn shows up, and NULL means
  "unreported", never "free".
- `0013_run_metadata.sql` — expand-only: adds `runs.name`, `runs.description`,
  `runs.category` (task t3), all nullable with no default. `name`/
  `description` are operator-given at creation only (`internal/api/runs.go`'s
  `handleCreateRun`); `category` alone is retaggable afterward via
  `PATCH /v1alpha1/runs/{id}` (`handlePatchRun`) per frame decision q4.
- `0014_events_namespace_id_index.sql` — expand-only: adds
  `events_namespace_id_id_idx (namespace_id, id)`, serving the cross-run
  event stream's (task t17, `GET /v1alpha1/events`) bounded, namespace-scoped
  poll ordered by the events table's own ULID primary key.
- `0015_actor_invocations_actor_id.sql` — expand-only: nullable
  `actor_invocations.actor_id` (FK to `actors`), the resolved actor row id an
  async attempt's terminal callback commits into `attempts.actor_id` — the
  attribution per-actor stats read; without it every async attempt was
  invisible to `GET /v1alpha1/actors/{id}/stats` (found live by the t20
  success-signal run).
- `0017_attempt_usage_extended.sql` — expand-only: adds
  `attempts.usage_cached_input_tokens`, `usage_reasoning_tokens`,
  `usage_model`, `usage_thread_id`, and `termination_reason` (task t1 of the
  economy-discord-graphs plan), the extended §13.2 telemetry
  `docs/adr/0009-usage-telemetry-extension.md` amends the PRD with. All five
  are nullable with no default and nothing is backfilled, exactly as in
  `0012`. `usage_input_tokens IS NOT NULL` remains the "this attempt
  reported usage" sentinel — the four new `usage_*` columns are each
  independently nullable *within* a reported block, so none of them may
  stand in for it. NULL still means "not reported", never zero: an actor
  whose contract exposes no cache telemetry is honestly unmeasurable rather
  than measured at 0%. `termination_reason` carries no
  `usage_` prefix on purpose — a turn can know why it ended while holding no
  parseable usage at all, so it is a sibling of the usage block rather than
  a member of it (see the migration header and ADR 0009).
- `0018_attempt_continuation_ref.sql` — expand-only: adds nullable
  `attempts.continuation_ref` (task t4 of the economy-discord-graphs plan),
  the §13.2 handle an actor offers for continuing the conversation a turn
  had, which `docs/adr/0010-continuation-ref-on-request.md` also adds to
  §13.1's request and §13.4's `completed` event. The value is opaque —
  nothing parses it or derives from it — and NULL means "no handle
  reported", never "the session ended" and never `''` (an empty string is a
  value a bridge could mistake for a handle). It is deliberately NOT
  `usage_thread_id` from `0017`: that column is telemetry about where a
  turn's usage accrued, this one is the handle a later dispatch resumes
  with, and neither is derived from the other.
- `0020_actor_availability.sql` — expand-only: adds the mutable
  `actor_availability` table (task t9 of the economy-discord-graphs plan,
  issue #48 item 1), the capacity circuit breaker's durable paused-until
  state. It is a NEW table rather than a column on `actors` because actor
  identity is append-only by contract (`0001`, restated in
  `internal/store/postgres/actorstats.go`): a pause is mutable and
  short-lived, and recording one as a new identity revision would be a lie
  about the actor's registration. It is keyed by `(namespace_id,
  actor_key)`, not by `actors.id`, because provider capacity belongs to the
  identity rather than to one revision of its registration — and because the
  dispatch site always has the actor key while the actors-table row id is
  best-effort. `paused_until` in the past means "history", never "paused":
  every read compares against `now()` rather than treating row presence as a
  pause. `retry_after_seconds` is NULL when the provider named no
  Retry-After — never `0`, which would read as "retry immediately".
  Concurrent trips are an idempotent upsert that keeps the LATER
  `paused_until`, so a race may extend a pause and never shorten one.
- `0023_run_sessions.sql` — expand-only: adds the `run_sessions` table (task
  t11 of the economy-discord-graphs plan, issue #48 item 5), the session
  ledger `budget.maxSessions` is spent against. One row per NEW provider
  session a run opened — a COLD START — and deliberately not one per
  dispatch: a dispatch carrying a prior `continuation_ref` (`0018`)
  continues a conversation already paid for and writes nothing, because a
  warm workstream of N turns that counted N would always exhaust the budget
  it was designed to conserve. It is a new table rather than a column on
  `attempts` because the cold/warm fact is known by the WORKER at dispatch
  time while an attempts row is written by the engine at completion time
  (and, for an async invocation, in another process entirely), and because
  `attempts` holds rows for kinds that never touch a provider at all. Keyed
  by the §13.1 protocol attempt id, so a re-entered dispatch charges once.
  The row is written immediately BEFORE the invocation and therefore
  over-counts a dispatch that dies in transport — the conservative
  direction, since a budget that under-counts spends money the author
  forbade.
- `0027_artifact_tombstones.sql` — append-only artifact retention records.
  Reaped refs preserve the original name, media type, size, digest, reaping
  time and policy; immutable corrections append through `supersedes`.
- `0028_attempt_supersedes.sql` — expand-only: `attempts.supersedes`, the
  nullable self-FK a late-callback reconciliation uses to name the attempt
  record it corrects (task t11,
  `docs/adr/0012-late-callback-supersession.md`), plus a partial UNIQUE
  index over it. The index carries ADR 0012 §2's invariant — one record is
  corrected at most once, so corrections chain rather than fan out. Every
  aggregate over `attempts` drops a row some other row supersedes (§3);
  listings deliberately keep both.
- `0029_attempt_late_callback_delivery.sql` — expand-only:
  `attempts.late_callback_attempt_id` / `late_callback_event_id`, the
  delivery identity (§13.1 protocol attempt id + §13.4 callback event id) of
  the late report a reconciliation row records, with a partial UNIQUE index
  over the pair and a `NOT VALID` CHECK keeping the two columns written
  together. It closes ADR 0012 §5's gap: 0028's index is partial on
  `supersedes IS NOT NULL`, so the reconciliation that corrects nothing was
  unguarded and an at-least-once redelivery could persist one session's
  report twice. Both columns are NULL on every ordinary dispatch, so the
  index constrains only rows a binary that knows about it writes — an N-1
  binary's INSERT cannot start failing under it.
- `0030_signal_event_watermarks.sql` — expand-only: durable per-source
  cursors for external emitters. Cursor comparison and advancement run in
  `DeliverSignalEvent`'s existing `signal_events` transaction, so a repeated
  discovery returns its original fact without firing handlers again.
- `0031_inbound_authentication.sql` — the deliberately temporary (#111)
  inbound verifier record. It keys parties by actor key or host name, rejects
  address-shaped keys, and stores only a SHA-256 verifier or an environment
  variable name so database dumps cannot become credential archives.

- `0022_dispatch_rate_state.sql` — expand-only: adds the mutable
  `dispatch_rate_state` table (task t10 of the economy-discord-graphs plan,
  issue #48 item 2), the dispatch pacing control's durable rate state. It is
  in the database rather than in a worker's memory because workers scale
  horizontally ("several processes may run one each",
  `internal/worker/worker.go`) and N in-process limiters enforce N times the
  declared rate. Keyed by `(namespace_id, scope, scope_key)` — `global` with
  an empty key for the installation's own session rate, `actor` plus an
  `actor_key` for one actor's — following `0020`'s reasoning that a rate
  belongs to the actor identity rather than to one append-only registration
  revision. `window_started_at` says which session window `dispatched`
  counts, so a window roll is a comparison and never a sweep: a row from an
  older window has consumed nothing in this one. `window_anchor`,
  `window_seconds` and `limit_per_window` are recorded for the operator read
  surface (`GET /v1alpha1/dispatch-rates`), not for the next decision — the
  deciding worker carries its own configuration
  (`NODES_DISPATCH_RATE_*`, `cmd/nodes/pacing.go`). Every decision happens
  inside one transaction that row-locks each scope it consults and writes all
  of them or none; a refusal writes nothing at all.

- `0024_plan_imports.sql` — expand-only: adds three new tables,
  `plan_imports`, `plan_import_tasks`, `plan_import_deviations` (task t22 of
  the economy-discord-graphs plan; issue #45, spec claims c10/c15, honesty
  h7/h11), the durable home for an imported external plan's per-task status,
  REAL dependency edges, computed wave layering, and deviations (with their
  origin — issue #45's "system knows" llm vs "user reports" user split).
  Deliberately NOT `ledger_records`: an import is not scoped to a run the
  way `ledger_records.run_id` requires, and it is not the evidentiary work
  ledger the PRD's immutable-by-trigger guarantee governs — see the
  migration file's own header for the full reasoning. Every import is its
  own row, inserted once (no UPDATE path is exposed at all); re-importing
  the same plan slug is a new row, read back by
  `(namespace_id, slug) ORDER BY imported_at DESC`, never an overwrite.
  `wave_index` is computed locally at import time from the real dependency
  edges (topological layering, `internal/devague`'s `planTaskWaves`) —
  devague's own `plan show --json` does not emit it; it is NULL for a
  rejected task, which occupies no wave. `source_status` columns carry the
  source system's own status verbatim and are never translated into a
  ledger authority value (see `internal/devague/deviations.go`'s
  `MapDeviations` doc comment for that reasoning in full).
- `0025_attempt_preserve_branch.sql` — expand-only: adds nullable
  `attempts.preserve_branch`, `preserve_pushed`, `preserve_remote` (task t26
  of the economy-discord-graphs plan, issue #49, spec claim c32 / honesty
  h21), carrying task t25's bridge-minted preserve-on-failure branch past
  the worker process that first reads it — t25 stopped at the bridge,
  attaching the outcome only to the failed event/error body. `preserve_branch`
  is written only when the bridge's `preserve.PreserveResult.committed` is
  true (a minted-but-never-committed name names nothing that exists in any
  repository); `preserve_pushed` distinguishes a branch that reached the
  configured remote from one that exists only in the bridge host's local
  object database (the expected common case today — bridge-host push
  credentials are unverified); `preserve_remote` is informational only and
  is never combined with the branch to fabricate a forge URL — a clickable
  link on the run detail page may only come from operator-set configuration
  (`web/README.md`'s `VITE_PRESERVE_BRANCH_URL_TEMPLATE`). All three are
  written together or not at all (`InsertAttempt`), so `preserve_pushed`/
  `preserve_remote` are never non-NULL while `preserve_branch` is NULL.
- `0026_dispatch_preflights.sql` — expand-only: adds `dispatch_preflights`
  (the clarify-then-commit gate's durable state, issue #67 / task t14) plus
  the `actors_preflight_gate_requires_surface` CHECK constraint. The gate's
  *evidence* is two ledger records — a derived `dispatch_preflight` and a
  proposed `dispatch_acknowledgement` — but ledger records are immutable,
  and the protocol this generalizes (`deploy/prod/install-secrets.sh`'s
  windowed confirmation file) is **single-use**: this table is where
  "consumed" becomes a transactional fact, so exactly one dispatch can ride
  one acknowledgement. Rows are keyed per **node run**, not per attempt,
  because the gate runs before any attempt exists. The CHECK constraint is
  the third door on acceptance criterion 3: `internal/preflight`'s
  `CheckConfiguration` refuses a gate-without-surface registration at the
  API handler and at `RegisterActor`, and this covers raw SQL. It is `NOT
  VALID` (no rescan of existing rows — none can violate it, since this
  migration introduces the concept) and deliberately narrower than the Go
  check: only a literal JSON `true` under `metadata.preflight_gate.enabled`
  enables the gate, matching `preflight.ParseGate`, so the two never
  disagree about which rows they are talking about. An N-1 binary ignores
  both changes safely — it never inserts a preflight row, and it never
  writes a `preflight_gate` block for the constraint to refuse.

## Policy

- `0033_inbound_transport.sql` — address-free actor presence and a durable
  reverse-transport mailbox. PostgreSQL owns work and responses; long polls
  are disposable signals and store no peer address.

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
