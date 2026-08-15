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
