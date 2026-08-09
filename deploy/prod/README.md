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

## The runner registry (NODES_RUNNER_SERVICES_FILE)

A worker only dispatches code over the network when
`NODES_RUNNER_SERVICES_FILE` points at a registry file — absent or empty,
it keeps the in-process CodeRunner path (network dispatch is a deployment
decision, never a default). The file is a JSON array; each entry binds a
registry name to one runner service:

```json
[
  {
    "name": "hello-workflow/build",
    "endpoint": "http://thor:17070",
    "image_digest": "sha256:…",
    "secret_file": "/home/nodes/.culture-nodes/runner-secret.thor",
    "allow_insecure_transport": true,
    "description": "thor's host runner"
  }
]
```

- `name` — the registry key: either `<workflow>/<node>` (place one node
  of one workflow) or a node's `uses` reference (place every node that
  uses it).
- `endpoint` — the runner service's base URL.
- `image_digest` — the runner image the operation document is pinned to.
- `secret_file` — path to the entry's bearer secret; the material stays
  out of the registry JSON so the registry stays loggable.
- `allow_insecure_transport` — opt-in for plaintext HTTP off-loopback,
  per the LAN trust boundary below.
- `description` — free text for humans reading the registry.

Name resolution is most-specific-first: for a code node the registry
tries `<workflow>/<node>` before the node's `uses` reference, so one
entry can re-place a single node while another places everything else
(`cmd/nodes/runnerservices.go`, `internal/worker/runnerasync.go`).

Each entry's `secret_file` contents are loaded once at worker start and
registered under the symbolic reference `runner-secret:<name>`; the
registry refuses paths as references by design, so a file path can never
be mistaken for secret material downstream.

The worker envs that complete the code-dispatch wiring:

- `NODES_CODE_RUNNER_NAME` — the logical runner name stamped on every
  code operation; any code dispatch refuses without one.
- `NODES_CODE_RUNNER_REVISION` — the runner build revision stamped on
  the operation; the runner service validates the operation against it.
- `NODES_CODE_RUNNER_ACTOR_ID` — the registered actors-table row the
  runner's observed evidence is attributed to.
- `NODES_RUNNER_SERVICES_FILE` — the path to this registry file; unset
  or an empty array keeps the worker in-process only.

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
