# Restore runbook — thor's authoritative Postgres (plan t20)

The optional `backup` profile in `compose.thor.yml` writes a full
`pg_dump -Fc` of `NODES_DATABASE_URL`
to `~/.culture-nodes/backups/` on thor every `BACKUP_INTERVAL_SECONDS`
(default 6h), keeping the newest 7 and a `latest.dump` symlink. The
ledger is the authoritative evidence store; this file is how it comes
back.

## Restore on a replacement machine

```bash
# 1. Fetch the newest dump off thor (argv-only ssh):
scp thor:.culture-nodes/backups/latest.dump /tmp/nodes-restore.dump

# 2. Start a fresh Postgres and restore into it:
docker run -d --name nodes-restore -e POSTGRES_USER=nodes \
  -e POSTGRES_PASSWORD=restore -e POSTGRES_DB=nodes -p 15432:5432 postgres:17-alpine
until docker exec nodes-restore pg_isready -U nodes -d nodes; do sleep 1; done
docker cp /tmp/nodes-restore.dump nodes-restore:/r.dump
docker exec nodes-restore pg_restore -U nodes -d nodes --no-owner /r.dump

# 3. Verify the ledger came back digest-intact — compare every run's
#    ledger record ids + content digests against the source (or, post
#    incident, against the last known projection digests):
docker exec nodes-restore psql -U nodes -d nodes -Atc \
  "SELECT count(*), coalesce(sum(('x'||substr(md5(id||record_digest),1,8))::bit(32)::bigint),0) FROM ledger_records"

# 4. Point the control plane at the restored database (NODES_DATABASE_URL)
#    and run `nodes migrate` — a same-version restore reports no pending
#    migrations; an older dump replays forward under expand-contract.
```

The drill that proves this procedure (backup from live production,
restored on the dev machine, every ledger record id + digest byte-equal)
ran on 2026-08-09 and is recorded in the phase-2 delivery summary.

## Restoring when thor itself is what failed (#30)

Step 1 above says `scp thor:...`, which assumes thor is still there. If it
is not, the local dumps went with it. That is what the off-host copy is
for.

Set `BACKUP_S3_BUCKET` on the `backup` service and each dump is also
uploaded to `s3://$BACKUP_S3_BUCKET/backups/`. Unset, nothing changes and
the deployment stays local-only — every install without an AWS account
gets exactly the behaviour it had before.

```bash
# 1. List what survived, and take the newest:
export AWS_PROFILE=culture-nodes AWS_REGION=us-east-1
aws s3 ls "s3://culture-nodes-backups-435593604218/backups/"
aws s3 cp "s3://culture-nodes-backups-435593604218/backups/<newest>.dump" \
  /tmp/nodes-restore.dump

# 2..4. Continue from step 2 of the runbook above — identical from here.
```

### Two retentions, on purpose

| Copy | Retention | Enforced by | Answers |
|---|---|---|---|
| `~/.culture-nodes/backups` on thor | newest 7 dumps | the backup loop's own `ls -1t \| tail -n +8` | "restore quickly" |
| `s3://$BACKUP_S3_BUCKET/backups/` | bucket lifecycle rule | S3, not this project | "restore when the host is gone" |

The remote copy is deliberately **not** expired by the backup loop. A
backup process that can delete its own off-host copies is one bug away
from deleting the thing it exists to protect.

The upload also fails soft: an S3 error logs `backup <ts> s3 FAILED` and
the loop continues, because a broken off-host copy must not stop a local
dump that still works. That log line is the signal to investigate.

### The drill that proves this half

Run on spark on 2026-08-15, deliberately destroying every local copy so
the restore could only come from object storage:

```console
$ docker exec t14-src psql -U nodes -d nodes -tAc 'select note from restore_canary where id=1'
written before the dump at 18:26:28Z
$ aws s3 cp nodes-t14.dump s3://culture-nodes-backups-435593604218/backups/nodes-t14-drill.dump
upload: ... to s3://culture-nodes-backups-435593604218/backups/nodes-t14-drill.dump
$ rm -f nodes-t14.dump && docker rm -f t14-src        # local dump AND source database destroyed
$ aws s3 cp s3://culture-nodes-backups-435593604218/backups/nodes-t14-drill.dump fetched.dump
download: s3://... to fetched.dump
$ docker exec t14-restore pg_restore -U nodes -d nodes /tmp/fetched.dump
$ docker exec t14-restore psql -U nodes -d nodes -tAc 'select note from restore_canary where id=1'
written before the dump at 18:26:28Z
```

A restored dump that nothing runs against is not a proven restore, so the
drill ends by pointing a full stack at it — using the external-database
switch task t15 added, since a restored database is exactly an external
one:

```console
$ COMPOSE_ENV_FILE=t14-restore.env COMPOSE_PROJECT_NAME=t14restore ./smoke.sh
runs list: 200 OK, 1 run(s)
workflow_digest=sha256:4a892a8c... run_id=01M03AVC2KP6GVAGQ0Y5SYG5DC final_state=running node_runs=1
$ docker exec t14-restore psql -U nodes -d nodes -tAc 'select note from restore_canary where id=1'
written before the dump at 18:26:28Z
```

The stack migrated the restored database, published a workflow and created a
run against it — and the pre-dump canary row was still there afterwards.

### That the bucket is private and encrypted, measured rather than assumed

`s3:GetBucketPublicAccessBlock` and `s3:GetEncryptionConfiguration` are not
in the operator grant, so the obvious commands return `AccessDenied`. They
were added to `dev-operator-policy.json` for management, but the evidence
below does not depend on them — and is stronger, because it measures the
effect rather than reading the configuration that is supposed to produce it:

```console
$ aws s3api head-object --bucket culture-nodes-backups-435593604218 \
    --key backups/probe.txt --query ServerSideEncryption --output text
AES256
$ curl -s -o /dev/null -w '%{http_code}\n' \
    https://culture-nodes-backups-435593604218.s3.us-east-1.amazonaws.com/backups/probe.txt
403
```
