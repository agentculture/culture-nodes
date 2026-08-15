# Owner decision: run a managed Postgres trial (#110)

Status: proposed decision brief, 2026-08-15.

## Decision requested

Should the operator spend a bounded experiment on one credential-authenticated
managed PostgreSQL provider now, or decline the trial and close the managed
target as unmeasured?

This is a trial, not a production migration. `.owner-issues/issue-110.md`
defines the measurements: migrations and suite, thor latency, connection
capacity, lease/fencing behaviour, cold start, and restore.

## Options and cost

### A — Authorize one bounded trial

Choose exactly one provider and one region, create a disposable database,
point `NODES_DATABASE_URL` at it, and record every measurement named in the
issue body. Delete the trial instance after the report. The repository does
not contain a current provider price or a measured staff-time estimate, and
this checkout has no network access, so monetary and engineering costs are
**unknown** rather than assumed to be zero.

The cheapest experiment that produces the missing cost and viability facts is
one provider's smallest disposable tier for one test window: run migrations,
the full suite and `tests/fault`, take the same three-sample TCP probe described
in `.owner-issues/issue-110.md`, idle it once to measure resume, record peak
connections, and restore one dump. Before creation, the owner must read the
provider's current price/limits and set both a spend ceiling and deletion time;
those figures cannot be supplied from this offline checkout.

The benefit must be weighed against the measured baseline, not an invented
savings claim: `docs/aws-rds-lane-closeout.md` records the entire thor control
plane at 246 MB and records the historical thor latency ladder (13 ms for
`il-central-1` through 224 ms for `us-west-2`), with the commands that produced
both.

### B — Decline the trial (recommended until a deployment move is desired)

Do not create a provider account or instance. Cost: zero trial spend and zero
trial engineering time; the managed target remains unsupported and cannot be
selected for this deployment. This does not claim the target is unsuitable;
it records that the owner chose not to buy the measurement.

## Dependencies

Option A needs an owner-selected provider/region, account and credential,
current terms/limits, a spend ceiling, a deletion owner, TLS settings, and an
approved window for the suite and fault tests. The configuration path already
exists: `deploy/compose/docker-compose.yml` and
`deploy/prod/compose.thor.yml` accept an external `NODES_DATABASE_URL`, while
`deploy/helm/culture-nodes/values.yaml` exposes `postgresql.external.url`.
Backup/restore must be assigned explicitly because
`deploy/prod/compose.thor.yml` makes its backup service a separate profile.

Option B depends only on issue #112 selecting a target other than managed
rather than claiming managed PostgreSQL is supported.

## Consequence of “no”

**No means close #110 as won't-do and remove `managed` from the selectable
targets for this deployment; it remains explicitly unmeasured. A future trial
requires a new issue naming one provider, region, spend ceiling, deletion time,
and the trigger that justifies rerunning this decision.**
