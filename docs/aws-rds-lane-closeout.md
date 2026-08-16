# AWS RDS lane closeout

Status: proposed record, 2026-08-15. No AWS cutover or provisioning is part of
this record.

## Decision: RDS remains opt-in

RDS is not granted by the base operator policy. The reviewable base policy,
`deploy/aws/dev-operator-policy.json`, contains no `rds:*` action. The RDS
actions live in `deploy/aws/rds-optional-policy.json`, an overlay that an
account administrator attaches with:

```bash
./deploy/aws/bootstrap-operator.sh enable-rds
```

The administrator removes the overlay with:

```bash
./deploy/aws/bootstrap-operator.sh disable-rds
```

This is deliberate least privilege: the current deployment keeps PostgreSQL
on thor and ships backups to S3, so no decided work exercises RDS. The overlay
preserves a reviewed path if issue #112 selects `rds` as the external database
target, without leaving a standing RDS grant on deployments that select a
different target.

## Measurements behind the closeout

The control-plane containers on thor measured **246 MB total**: PostgreSQL 69
MB, MinIO 95 MB, API 32 MB, worker 20 MB, scheduler 15 MB, notifier 14 MB, and
backup 2 MB (component figures are rounded, so they sum to 247 MB). The host
had 122 GB total and 56 GB free; the comparison workload, model-gear's vLLM
worker, used 16.2 GB. These were produced on thor with:

```bash
free -h
docker stats --no-stream --format '{{.Name}}\t{{.MemUsage}}'
```

The region TCP-connect latency ladder measured from thor was:

| Region | TCP connect |
|---|---:|
| `il-central-1` | 13 ms |
| `eu-south-1` | 54 ms |
| `eu-central-1` | 68 ms |
| `us-east-1` | 155 ms |
| `us-east-2` | 161 ms |
| `us-west-2` | 224 ms |

It was produced with the same endpoint probe used by preflight:

```bash
for r in il-central-1 eu-south-1 eu-central-1 us-east-1 us-east-2 us-west-2; do
  printf '%s ' "$r"
  curl -o /dev/null -sS -w '%{time_connect}\n' "https://rds.${r}.amazonaws.com/"
done
```

These are historical measurements, not a claim about today's route. Re-run
the commands from the control-plane host before a future cutover.

## Conditional requirements

Claims c67, c68, and c70 in the close-the-backlog frame are **binding only if
issue #112 selects the `rds` target**. They cover the RDS-specific prerequisite,
region/latency, and cutover obligations. With no RDS selection and no cutover,
they are not gates for the bundled PostgreSQL or another external PostgreSQL
target.

If #112 does select RDS, `./deploy/aws/preflight.py --db-target rds` must exit
0 before the first provisioning command. The same command must exit 0 again
after provisioning. This checkout has no AWS credentials, so neither live
preflight result can be produced here.
