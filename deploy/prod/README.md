# deploy/prod — the thor + orin production pair

The production topology from the phase-2 spec (`docs/specs/2026-08-09-self-hosted-phase-2-cycle.md`):
**thor** hosts the shared authoritative Postgres, MinIO, and the full
control plane (`compose.thor.yml`); **orin** joins as a second worker over
the LAN (`compose.orin.yml`). Two machines against one database imitate
the k8s multi-pod topology. The dev machine deploys with argv-only ssh —
no push, no registry: `deploy.sh` ships a `git archive` of HEAD and builds
natively on each aarch64 target.

## One-time setup

```bash
./install-secrets.sh            # generate + install ~/.culture-nodes/prod.env on both
./deploy.sh thor                # control plane + worker + runner unit
./deploy.sh orin                # second worker + runner unit
```

`install-secrets.sh` never passes a secret through argv — everything rides
ssh stdin into mode-0600 files (the credential discipline cited from
reachy-mini-cli's ssh module). It refuses to overwrite an existing
`prod.env` unless `FORCE=1`, so re-deploys never silently rotate the live
database password.

## The runner is a host process (deviation d2)

`cmd/nodes-runner` runs under a **systemd user unit**
(`nodes-runner.service`), not in compose: the code-execution boundary
needs the host's Docker and the headspace CLI, and the control-plane
containers are deliberately socketless (`tests/deploy` enforces it).
`deploy.sh` installs the headspace CLI (`uv tool install headspace-cli`),
the binary, the env file, and the unit. The runner's bearer secret lives
in `~/.culture-nodes/runner.secret` on each machine, mirrored to the
operator's `~/.culture-nodes/runner-secret.<host>` for registry entries.

## Network trust

Postgres (5432), MinIO (9000), the API (18080), and the runners (17070)
listen on the LAN with secret/password auth and no TLS — the LAN is the
trust boundary, per the cycle's boundary decision (OIDC/workload auth is
parked as issue #6; the runner protocol's `AllowInsecureTransport` opt-in
is what permits plaintext HTTP off-loopback). Do not port-forward any of
these beyond the LAN.

## After first boot

`nodes serve` mints the namespace id on first boot; `deploy.sh thor`
reads it back from Postgres, writes `NODES_NAMESPACE_ID` into
`prod.env`, and restarts the worker with it. `deploy.sh orin` copies the
same id and resolves `THOR_IP` (containers don't inherit `/etc/hosts`).

## Verifying the pair

```bash
ssh thor 'curl -fsS http://localhost:18080/v1alpha1/healthz'
ssh thor 'docker compose --env-file ~/.culture-nodes/prod.env \
  -f culture-nodes-prod/deploy/prod/compose.thor.yml ps'
ssh orin 'docker compose --env-file ~/.culture-nodes/prod.env \
  -f culture-nodes-prod/deploy/prod/compose.orin.yml ps'
# Both worker ids appear once real work runs:
ssh thor 'docker compose --env-file ~/.culture-nodes/prod.env \
  -f culture-nodes-prod/deploy/prod/compose.thor.yml exec -T postgres \
  psql -U nodes -d nodes -Atc "SELECT DISTINCT lease_owner FROM work_items"'
```
