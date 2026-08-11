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

## codex actor bridges (thor + orin)

A second production actor backend, per
`docs/specs/2026-08-11-codex-bridges-thor-orin.md` (the converged
codex-bridges-thor-orin frame; claim ids below cite that frame's `c`/`h`
records, `s` cites its scope-exploration entries). Reference config surface:
`adapters/codex/README.md`.

### Architecture

- One `codex-bridge` systemd user unit per host, running beside the
  containerized control-plane/worker stack — never inside a container —
  the same host-service lane `nodes-runner.service` already proves:
  `~/.culture-nodes`, an `ExecStartPre` preflight, `Restart=always` (c3).
- Two actors, `company/codex-thor` and `company/codex-orin`, registered
  **append-only** in thor's actors table (c4, c8). Each actor row's
  `metadata.auth_token_env` names only its **own** token env var — the
  thor row names `NODES_ACTOR_CODEX_THOR_TOKEN`, the orin row names
  `NODES_ACTOR_CODEX_ORIN_TOKEN` (c4).
- Both `compose.thor.yml` and `compose.orin.yml` worker env blocks carry
  **both** `NODES_ACTOR_CODEX_THOR_TOKEN` and `NODES_ACTOR_CODEX_ORIN_TOKEN`
  beside the existing `NODES_ACTOR_CLAUDE_TOKEN` — either worker may
  dispatch either actor, so both need both credentials. A missing env
  fails resolution loudly in `internal/worker/registry.go`, never
  silently (c4, h4).
- Actor `endpoint_ref`s are numeric LAN IPs, never hostnames: worker
  *containers* don't inherit `/etc/hosts`, so a `thor:8086`-style
  endpoint would fail resolution from inside the container the way
  `THOR_IP` resolution already exists to avoid above (c20, h18).

### Install, deploy, verify

Extends the one-time setup above with a bridge lane:

```bash
./install-secrets.sh   # + bridge-token lane: writes codex-bridge.env on the
                        # owning host, appends both NODES_ACTOR_CODEX_*_TOKEN
                        # envs to prod.env on BOTH hosts, over ssh stdin
./deploy.sh thor        # + codex-bridge: uv-tool install, agent checkout,
                         # unit + nodes CLI ship
./deploy.sh orin         # same, for orin

# register both actors (numeric LAN IPs only — c20):
./register-actor.sh company/codex-thor http://<thor-lan-ip>:8086 \
  NODES_ACTOR_CODEX_THOR_TOKEN
./register-actor.sh company/codex-orin http://<orin-lan-ip>:8086 \
  NODES_ACTOR_CODEX_ORIN_TOKEN

# verify:
ssh thor 'systemctl --user status codex-bridge'
ssh orin 'systemctl --user status codex-bridge'
./examples/codex-smoke-pair/run.sh   # manual, billable, live-only — never CI
```

`deploy.sh`'s bridge lane does four things per host:

1. `uv tool install` the `adapters/codex` package into `~/.local` —
   **archive-independent**, so `deploy.sh`'s normal per-run
   `rm -rf culture-nodes-prod` never takes the running bridge down with
   it; a full re-deploy still leaves the bridge active afterward (c21,
   h19).
2. Provisions `~/git/culture-nodes-agent`: clones
   `github.com/agentculture/culture-nodes` if absent; on a later run,
   fast-forwards **only if the checkout is clean**, and otherwise
   refuses — leaving the checkout untouched, with a clear message — when
   it is dirty or diverged (c6, h6). `codex exec` refuses to run outside
   a git repository and this bridge never passes
   `--skip-git-repo-check` (`adapters/codex/README.md`), so a real
   checkout is load-bearing, not cosmetic.
3. Installs the unit, env file, and config; `daemon-reload`s, restarts,
   enables — mirroring the runner block above.
4. Builds the Go `nodes` CLI locally and ships it to each host (scp) —
   neither host has a Go toolchain (c19, h17), the same
   build-locally-then-scp fallback `deploy.sh` already uses for
   `cmd/nodes-runner`.

`register-actor.sh` is idempotent and append-only: it reads the latest
revision for an actor key, no-ops on an unchanged endpoint+metadata, and
appends a new revision row on a changed one — no code path ever `UPDATE`s
or `DELETE`s an actor row (c8, h7). It refuses a hostname endpoint
outright; only a numeric LAN IP is accepted (c20).

### Unbounded concurrency — placement is the containment

The bridge's async runner spawns one thread + one `codex exec` subprocess
per invocation with **no concurrency cap**
(`adapters/codex/src/codex_bridge/async_runner.py`). Adding a cap would be
a change under `adapters/codex/src`, which this cycle's zero-src-change
rule puts out of scope (c23) — so **workflow placement is the only
containment that exists today**. Orin has roughly 8 GiB RAM free and is
already tight running its own worker (per this repo's
machines-dev-prod-topology memory) — treat orin as the binding constraint.
Prefer thor for heavy or concurrent codex placements; avoid stacking
concurrent codex sessions on orin. This is a documented, parked plan risk
(h21), not a solved problem — no concurrency-cap code lands this cycle.

### Runbooks

**codex re-login (auth expiry).** Codex's ChatGPT auth can expire while
the systemd unit keeps running — the preflight only re-checks at unit
start/restart, so a mid-life expiry surfaces as invocation failures, not
a unit crash. Symptom: bridge invocations fail where `codex login status`
would report unauthenticated. Fix:

```bash
ssh <host>                  # thor or orin
~/.local/bin/codex login
systemctl --user restart codex-bridge
```

**checkout harvest / reset (after a write task).** Production codex
nodes default to `sandbox: read-only`; only an explicit write task dirties
`~/git/culture-nodes-agent`. Because `deploy.sh` refuses to fast-forward
(or deploy over) a dirty/diverged checkout (c6, h6), an operator closes
the loop before the next deploy rather than leaving it blocked:

```bash
ssh <host>
cd ~/git/culture-nodes-agent
git status                  # review what a write task changed
git diff                    # inspect it
# commit what's approved through the normal PR lane (push a branch, open
# a PR — never push directly from the host), then:
git reset --hard && git clean -fd
```

Skipping the reset does not corrupt anything — it blocks the *next*
`deploy.sh <host>` run with a clear refusal message, checkout untouched
(c6, h6).

**token rotation.**

```bash
FORCE=1 ./install-secrets.sh   # generates fresh tokens; refuses without FORCE=1
./deploy.sh thor                # picks up the refreshed prod.env
./deploy.sh orin
ssh thor 'systemctl --user restart codex-bridge'
ssh orin 'systemctl --user restart codex-bridge'
# restart both workers — both carry both NODES_ACTOR_CODEX_*_TOKEN envs (c4):
ssh thor 'docker compose --env-file ~/.culture-nodes/prod.env \
  -f culture-nodes-prod/deploy/prod/compose.thor.yml restart worker'
ssh orin 'docker compose --env-file ~/.culture-nodes/prod.env \
  -f culture-nodes-prod/deploy/prod/compose.orin.yml restart worker'
```

Restart both workers even when only one bridge's token rotated — each
worker resolves both actors, so each needs the refreshed `prod.env`.

### issue #14 acceptance mapping

| issue #14 checkbox | frame claim(s) |
| --- | --- |
| `codex-bridge` enabled, active, survives restart on both hosts | c3, h3, c21, h19 |
| Both hosts pass preflight on authenticated `~/.local/bin/codex` 0.147.0; thor's stale binary can't be selected accidentally | c5, h5, c7 |
| Thor + orin workers reach and authenticate to both bridge endpoints | c4, h4, c20, h18 |
| Thor Postgres carries append-only registrations for both actors, each naming only its own token env | c4, c8, h7 |
| A workflow run completes one node per host and writes two `proposed` ledger claims to the correct actor ids | c1, h1, c18, h8 |
| The supported CLI reports runs, node runs, ledger state, and pending human tasks from a codex session | c14, h13, c19, h17 |
| Existing claude registrations/services and historical runs are unchanged | c9, h9 |
| An unavailable host fails its selected actor honestly; no undocumented failover/active-active claim | c10, h10 |
| Adapter/deployment tests cover preflight failures, token wiring/redaction, idempotent registration, and service definitions | c5/h5 (preflight), c12/h11 (token wiring/redaction), c8/h7 (idempotent registration), c3/c12 (service definitions) |

### measured before-state (2026-08-11)

Probes recorded in the spec's scope entries `s1`–`s9` (h15) — the
*measured* state, not the issue text, two claims of which were already
stale by the time this cycle ran:

- No `codex-bridge` unit existed on either host; only
  `nodes-runner.service` was active (`s5`, `s6`).
- No codex actor rows existed in thor's actors table (`s7`).
- No `~/git/culture-nodes-agent` checkout existed on either host
  (`s5`, `s6`).
- Both hosts had codex-cli 0.147.0 authenticated (ChatGPT) at
  `~/.local/bin/codex` — itself a symlink into the self-updating
  standalone install
  (`~/.codex/packages/standalone/current/bin/codex`), a soft pin rather
  than a fixed version (`s5`, `s6`).
- thor's issue-flagged stale `/usr/local/bin/codex` 0.55.0 was already
  gone (`ls` failed against the path) — the first of the two stale issue
  claims (`s5`, c7).
- spark's four registered `claude` actor rows
  (`192.168.1.157:8086`-`8089`) were dark: no listeners on `8085`-`8089`
  and no bridge processes running, despite the issue's "existing Claude
  setup is healthy" line — the second stale issue claim, and itself
  evidence that ad-hoc bridges without systemd management do not survive
  (`s8`).
