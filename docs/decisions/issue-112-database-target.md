# Owner decision: database target for this deployment (#112)

Status: proposed decision brief, 2026-08-15.

## Decision requested

Which already-configurable PostgreSQL target should **this thor/orin
deployment** point at: its bundled PostgreSQL, a credential-authenticated
managed provider after #110, or RDS?

This is no longer a decision about what database support to build.
`deploy/prod/compose.thor.yml` and `deploy/compose/docker-compose.yml` put the
bundled database behind the `bundled-postgres` profile and require
`NODES_DATABASE_URL`; `deploy/helm/culture-nodes/values.yaml` provides
`postgresql.enabled` plus `postgresql.external.url`. The work-package grounding
records that a stack was brought up against external PostgreSQL; this brief
does not elevate that supplied fact into an independently observed claim.

## Options and cost

### A — Keep this deployment bundled (recommended)

Keep `NODES_DATABASE_URL` pointed at thor's bundled PostgreSQL and close the
selection. Incremental database service cost: no new provider bill. The
resource baseline in `docs/aws-rds-lane-closeout.md` is 69 MB for PostgreSQL
and 246 MB for the whole control plane on a host with 122 GB total; those are
historical measurements made by the commands printed there, not current
capacity guarantees. Operational cost remains owning backup, restore, host
failure, patching, and credentials; the repository contains no staff-time
figure for it.

### B — Select a credential-authenticated managed provider

Select this only after #110 returns an acceptable measured verdict. Provider
price, engineering time, latency, connection limits, cold-start penalty, and
restore characteristics are **unknown** until that trial. The cheapest
experiment that produces them is the bounded trial specified in
`docs/decisions/issue-110-managed-postgres-trial.md`; do not invent a free-tier
price or promote it before that result.

### C — Select RDS

Enable the optional AWS overlay and provision/cut over only after the RDS
preflight. Current dollar cost and engineering time are **unknown**; obtain a
current AWS estimate for the selected region and instance/storage/backup
settings before approval. The cheapest latency evidence already available is
the historical ladder in `docs/aws-rds-lane-closeout.md`: 13 ms
`il-central-1`, 54 ms `eu-south-1`, 68 ms `eu-central-1`, 155 ms
`us-east-1`, 161 ms `us-east-2`, and 224 ms `us-west-2`. Re-run its documented
probe before cutover.

RDS permissions are not in the base policy
(`deploy/aws/dev-operator-policy.json`). The optional
`deploy/aws/rds-optional-policy.json` grants project-scoped instance,
snapshot, subnet-group and parameter-group mutations, plus the described
unscoped reads and tightly conditioned security-group operations.

## Dependencies

Bundled depends on the local backup/restore lane and thor availability.
Managed depends on an affirmative #110 verdict, provider credentials, TLS,
and an explicit backup owner. RDS depends on an owner-selected AWS account,
region and network path, the optional policy overlay, a current quote, and
`./deploy/aws/preflight.py --db-target rds` passing before and after
provisioning, as required by `docs/aws-rds-lane-closeout.md`. The same file
states that the RDS-specific c67/c68/c70 obligations bind only if RDS is
selected.

## Consequence of “no”

**No to moving this deployment off thor means select `local`, close #112 with
the bundled target pinned for thor/orin, and do not keep an abstract database
choice open. Any later move must open a new cutover issue naming the target,
current quote, measured latency, rollback, and maintenance window.**
