# Artifact retention and production sizing

Artifact content has a 30-day retention window. The retention worker is the
only legitimate caller of `Store.Reap`; it supplies the policy identifier
`retention/30-days` and its trusted clock time. `Store.Delete` is retained as
a fail-closed compatibility surface and always refuses raw deletion. A reap
appends an immutable tombstone before removing bytes. The artifact metadata
row and tombstone remain indefinitely, so every ledger `artifact_ref` resolves
either to digest-checked content or to an explicit record of what was reaped,
when, and why. Tombstone corrections append with `supersedes`, matching the
ledger authority model rather than rewriting history.

## Capacity envelope

The production write route accepts at most 64 MiB per request. Capacity is
planned at 20 attempts per run, 10 runs per day, one published artifact per
attempt, and 30 retained days: 6,000 live artifacts. These are operational
planning assumptions, not fabricated observations.

The router keeps artifacts through 1 MiB in `prod-postgres-1` and sends larger
ones to `prod-minio-1`. Conservatively sizing each backend as though every live
artifact landed there gives:

- `prod-postgres-1`: 6,000 × 1 MiB = 6,000 MiB (5.86 GiB), plus table/index,
  WAL, backup and vacuum overhead. Allocate at least 12 GiB for artifact data.
- `prod-minio-1`: 6,000 × 64 MiB = 384,000 MiB (375 GiB), before MinIO
  filesystem/erasure and backup overhead. Allocate at least 500 GiB.
- Immutable metadata and tombstones grow after content retention expires.
  At a deliberately padded 4 KiB per artifact+tombstone pair, the same write
  rate adds about 800 KiB/day, 285 MiB/year to Postgres.

The HTTP route currently limits bytes per artifact but not artifact count per
attempt. Therefore 5.86/375 GiB are the expected-workload envelope, not a hard
adversarial quota. Alert on 70%/85% capacity and on artifact counts exceeding
200/day; exceeding either assumption requires rate/quota work or more storage
before the 30-day window can be honored.
