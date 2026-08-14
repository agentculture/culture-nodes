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
`prod.env` unless `FORCE_PROD=1`, so re-deploys never silently rotate the
live database password, and each other lane has its own `FORCE_*` switch
(`FORCE_RUNNER`, `FORCE_CODEX`, `FORCE_NOTIFY`, `FORCE_HUMAN_INBOX`) so
authorizing one rotation cannot authorize another.

## prod.env is merged, never rewritten

`prod.env` holds two populations of keys: the six secrets
`install-secrets.sh` generates, and the ones that accrete afterwards —
`NODES_NAMESPACE_ID` and `THOR_IP` written by `deploy.sh`,
`NODES_ACTOR_CODEX_*_TOKEN` / `NODES_ACTOR_NOTIFY_TOKEN` written by later
lanes of the same script, and `NODES_ACTOR_CLAUDE_TOKEN` /
`DISCORD_WEBHOOK_URL` relayed in from outside.

Every write **merges key by key**: an existing key's line is replaced in
place, an unknown key is appended, and nothing else in the file is touched.
All three lanes that write the file share one `PROD_ENV_MERGE` definition,
because the copies had already drifted — only one of them normalised a
missing trailing newline, and appending to a hand-edited file without one
concatenated the new key onto the previous value.
The prod lane used to write the whole file from its generated block, so an
authorized rotation deleted the second population without saying so — a
`FORCE=1` rotation destroyed `NODES_ACTOR_CLAUDE_TOKEN` and the breakage
stayed latent for ~18 hours, because the running worker kept the token in
memory until its next restart (`company/developer` succeeded at 13:03, then
answered `policy_denied` / 401 at 06:42 the next morning). Merge semantics
are pinned by `tests/deploy/prodenvmerge_test.go`, which rotates for real
with an externally-issued key present and looks for it afterwards.

### Removing a key

Merging means no deploy lane can delete a line, so removal is its own
explicit act — otherwise `prod.env` could only ever grow, and a dead
credential would be indistinguishable from a live one:

```bash
./remove-secret.sh NODES_ACTOR_CLAUDE_TOKEN thor          # dry run: shows the
                                                          # line, value redacted
./remove-secret.sh NODES_ACTOR_CLAUDE_TOKEN --yes thor    # actually removes it
```

Hosts default to `thor orin`; `ENV_FILE=<name>` targets another file in
`~/.culture-nodes` (e.g. `codex-bridge.env`). It is a dry run until `--yes`,
it never prints a value, it refuses any key name that is not
`[A-Za-z_][A-Za-z0-9_]*` (a pattern here would delete lines nobody named),
and it writes no backup — a `.bak` beside `prod.env` would be a second
unmanaged copy of live credentials. Restart whatever reads the file
afterwards: a running container still holds the removed value in memory.

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

## Dispatch pacing (NODES_DISPATCH_RATE_*)

Task t10 (issue #48 item 2). A worker can hold itself to a declared
session rate so a backlog does not drain at dispatch speed into a
subscription window that resets on a fixed clock. The state is durable
and shared (`dispatch_rate_state`, migration 0022), so several workers
honour ONE rate between them rather than one rate each. Unset, there is
no pacing and nothing changes; a malformed value refuses to start the
worker rather than dispatching unpaced.

- `NODES_DISPATCH_RATE_LIMIT` — dispatches all of this namespace's
  workers together may start per window. This is the meter that matters
  when several actors draw on one subscription pool.
- `NODES_ACTOR_DISPATCH_RATE_LIMIT` — the default rate each actor key
  gets *separately*, on the same window.
- `NODES_ACTOR_DISPATCH_RATE_LIMITS` — per-actor overrides,
  `company/analyzer=4,company/reviewer=1`; a limit of `0` opts that actor
  out of the default entirely.
- `NODES_DISPATCH_RATE_WINDOW` — the session window's length as a Go
  duration; defaults to `5h`. It applies to every scope, because it
  describes the subscription's reset cycle, not one scope's allowance.
- `NODES_DISPATCH_RATE_ANCHOR` — the reset clock, an RFC 3339 instant at
  which a window boundary falls (`2026-08-13T00:00:00Z`). Windows tile
  off it in both directions; every worker must use the same one. Unset
  tiles from the Unix epoch, which puts round window lengths on round
  clock times.

The rate is spread across the REMAINING window, not applied as a flat
interval: a wave starting halfway through a window can place about half
as many sessions as the same wave starting at the reset. Read what is
actually being enforced — and what each scope has consumed this window —
with `curl -s $API/v1alpha1/dispatch-rates`; each actor's own rate also
rides on `GET /v1alpha1/actors`. A paced deferral is recorded per run as
`dev.culture.nodes.dispatch.paced`, so a stalled run explains itself.

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

### Unprivileged user namespaces (issue #63)

**A fresh Ubuntu 24.04 host cannot run a codex actor until this is
changed.** Codex sandboxes every shell command it runs inside a user
namespace, built with bubblewrap. Ubuntu 24.04 ships
`kernel.apparmor_restrict_unprivileged_userns=1`, which blocks unprivileged
user-namespace creation outright — so the actor registers, dispatches,
accepts work, and then fails every command it tries to run, after the turn
is already spent. Nothing about the error says "host provisioning": it
surfaces as a bridge or runner fault and is neither.

This fleet takes the blunt option, deliberately and with its cost stated:

```bash
echo 'kernel.apparmor_restrict_unprivileged_userns = 0' \
  | sudo tee /etc/sysctl.d/60-culture-nodes-userns.conf
sudo sysctl --system
```

Applied and persisted on spark, thor and orin. The cost is real: this
restores pre-24.04 behaviour for *every* local process, re-exposing a
kernel attack surface that has historically carried local-root CVEs. On
these single-tenant LAN machines that is a smaller cost than on a shared
host, but it is not zero. The better option — a scoped AppArmor profile
granting `userns` to `bwrap` alone — stays open; none is installed today.
Disabling the codex sandbox instead (`--sandbox danger-full-access`) is
rejected: it widens the agent's blast radius to everything the invoking
user can touch, to work around a sandbox bug.

**Verify by capability, never by reading the sysctl back.** The value says
what was configured; only the probe says what works:

```bash
bwrap --unshare-user --unshare-net --ro-bind / / /bin/true && echo "bwrap userns: OK"
```

`codex-preflight.sh` runs exactly this probe as its check 7, and the
bridge unit runs the preflight as `ExecStartPre` — so a host in this state
fails to start its bridge instead of accepting work it cannot do.

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
./examples/codex-smoke-pair/run-smoke.sh   # manual, billable, live-only — never CI
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
4. Installs the Python `nodes` query CLI on each host from PyPI
   (`uv tool install culture-nodes` -> `~/.local/bin/nodes`) — the Go
   binary has no query verbs (deviation d1), and neither host has a Go
   toolchain (c19, h17); contrast the build-locally-then-scp fallback
   `deploy.sh` still uses for
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
already tight running its own worker (measured at the phase-2 rollout) —
treat orin as the binding constraint.
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
FORCE_CODEX=1 ./install-secrets.sh   # fresh bridge tokens; refuses without it
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
| Both hosts pass preflight on the authenticated explicit `~/.local/bin/codex` (0.147.0 at rollout; the preflight records the measured version rather than pinning one) | c5, h5, c7 |
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

## nodes-notifier (Discord webhook daemon, thor — task t34)

`cmd/nodes-notifier` (economy-discord-graphs task t14) consumes the
control plane's cross-run SSE feed and posts run-lifecycle events to a
Discord (or generic) webhook via `internal/notify`. It runs as a compose
service, `notifier`, in `compose.thor.yml` — thor only, since the SSE
feed it consumes is thor's own API service; a second copy on orin would
just be a redundant consumer of the same events.

**Image**: the same `culture-nodes:prod` image every other role container
runs. The Dockerfile's build stage now compiles `cmd/nodes-notifier`
alongside `cmd/nodes` and the final distroless stage ships both binaries
(`/nodes` and `/nodes-notifier`); the `notifier` service selects the
second one with an `entrypoint: ["/nodes-notifier"]` override. This is the
smallest change that avoids a second Dockerfile, a second build context,
or a second image to build natively on thor — see the Dockerfile's own
header comment for the full reasoning.

**Cursor durability**: `NODES_NOTIFIER_CURSOR_FILE` points at
`/var/lib/nodes-notifier/cursor.json` inside the container, mounted from
the `notifiercursor` **named volume** (top-level `volumes:` entry, same
treatment `pgdata`/`miniodata` get) — never a bind mount, and never left
on the container's own writable layer. This is load-bearing: the cursor
is the daemon's entire exactly-once-across-restarts guarantee
(`internal/notifier/cursor.go`), so it must survive a `docker compose ...
up -d` container recreate, not just an in-process restart.

**Secrets**: `CULTURE_NODES_WEBHOOK_URL` (checked first) or
`DISCORD_WEBHOOK_URL` (fallback) — read by `internal/notify.ResolveWebhook`
inside the container, never passed as a flag (the URL embeds a bearer
token). Neither is fabricated by `install-secrets.sh`: it only relays a
value the operator already exported into *its own* environment before
running the script:

```bash
CULTURE_NODES_WEBHOOK_URL='https://discord.com/api/webhooks/…' ./install-secrets.sh
```

Left unset, `nodes-notifier` still starts and runs — every lifecycle
event is journaled as delivery-disabled rather than posted (fail-open,
per `internal/notify`'s own doc comment) — until a later re-run of
`install-secrets.sh` installs the URL and `deploy.sh thor` restarts the
service.

**Verify**:

```bash
ssh thor 'docker compose --env-file ~/.culture-nodes/prod.env \
  -f culture-nodes-prod/deploy/prod/compose.thor.yml ps notifier'
ssh thor 'docker compose --env-file ~/.culture-nodes/prod.env \
  -f culture-nodes-prod/deploy/prod/compose.thor.yml logs --tail 50 notifier'
```

A healthy startup line reads `nodes-notifier consuming http://api:8080
(cursor /var/lib/nodes-notifier/cursor.json, runs every active run
(default), webhook enabled)` — `webhook disabled` means the secret above
is not yet installed.

## human-inbox actor bridge + merge tracker (task t34; host derivation, t10)

`adapters/human-inbox` (task t16) is a `kind=human` actor-protocol bridge:
culture-nodes invocations park as durable inbox tasks until a person (or
the sibling GitHub merge tracker) submits a result. Reference config
surface: `adapters/human-inbox/README.md`. Deployed as **two host systemd
user units, always together, on the host serving `company/human-ops`** —
one logical human actor, unlike codex's per-host actors, so a second copy
anywhere would race the same GitHub PRs and the same inbox tasks against
the same actor row.

### Which host they go to (issue #72)

**Derived from the actor's registration, never declared.** `deploy.sh`
and `install-secrets.sh` both source `actor-placement.sh`, which reads
`GET /v1alpha1/actors`, takes `company/human-ops`'s newest revision, and
uses that row's `endpoint_ref` for the host to deploy to, the port the
bridge binds, and the `actors(id)` the bridge stamps as
`origin.actor_id`. One registry read, so those values cannot come from
different revisions.

This lane used to say *thor only*, in a comment, in three files, while
the actor was registered at another machine's address. The engine
dispatches to the registration — so human tasks parked on the bridge
there while the tracker on the declared host watched its own empty state
directory and logged `pending=0` for as long as anyone left it running.
Nothing failed; two config values that had to agree were agreeing only by
luck.

Two mechanisms now hold the pairing, and both are needed:

- **Deploy time.** `assert_human_inbox_colocated` runs after the env
  files are written and before either unit is installed. It reads back
  what was actually written on the host and refuses the deploy — loudly,
  naming both sides — if the host does not answer on the registered
  address, if the bridge port is not the registered port, if the tracker
  points at another bridge or another state directory, if the bridge's
  and tracker's `HUMAN_INBOX_BRIDGE_ACTOR_ID` values are swapped, or if
  the tracker's startup check is left disarmed.
- **Runtime.** The tracker resolves the same registration at startup and
  exits non-zero when its bridge is not the actor's bridge (task t8,
  `verify_bridge_serves_actor`). `deploy.sh` writes
  `HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL` precisely so this check is
  armed; unset, it degrades to a warning and the split runs silently
  again.

A wrong deploy that never starts is still a wrong deploy, and a right
deploy that nothing rechecks drifts on the next endpoint move.

If the actor is not registered yet, both scripts install **nothing** and
say so: a pair deployed to a guessed host is the defect, a pair not
deployed is a reported gap. `HUMAN_INBOX_HOST=<address>` overrides the
lookup in `install-secrets.sh` for bootstrapping a host before its actor
row exists; `NODES_API_URL` (default `http://thor:18080`) points both
scripts at the control plane.

`deploy/prod/actor-placement.sh` also runs its command locally rather
than over ssh when the resolved address belongs to the machine running
the deploy — a normal arrangement (an actor served by the operator's own
box), and one where ssh'ing to yourself may not be configured at all.

### Architecture

- `human-inbox-bridge.service` — the bridge server (`human-inbox-bridge
  serve`), always installed and started.
- `human-inbox-tracker.service` — the GitHub merge tracker
  (`human-inbox-tracker`), installed and started **unconditionally**
  beside the bridge. `GITHUB_TOKEN` selects the authenticated polling
  lane; without one the tracker polls public repositories anonymously.
- Both units are **persistent, `Restart=always` services**, not a
  `--once` timer: the tracker's own module docstring documents both a
  continuous mode and a one-shot `--once` probe, and the continuous mode
  already implements its own bounded poll loop
  (`HUMAN_INBOX_TRACKER_POLL_SECONDS`, default 60s) with a per-cycle
  GitHub request budget — wrapping `--once` in a systemd timer would just
  reimplement that same interval as a second, redundant schedule.
- Both exec console scripts installed by `uv tool install`, which copies
  the package into its own venv — so the units keep serving after the
  next deploy's `rm -rf` removes the tree they were built from. They used
  to `uv run --directory` against the `~/git/culture-nodes-agent` codex
  agent workspace, a checkout the codex-bridge lane fast-forwards to
  main: deploying a branch installed units whose code was not in the
  directory they ran from, and the tracker crash-looped 6272 times over
  nine hours on `No module named human_inbox_bridge.tracker`. An agent
  workspace and a deployment artifact source are different things.

### Install, deploy, verify

```bash
# Operator-supplied, externally issued credentials — never generated by
# this repo:
CULTURE_NODES_WEBHOOK_URL='https://discord.com/api/webhooks/…' \
GITHUB_TOKEN='ghp_…' \
  ./install-secrets.sh   # + human-inbox lane: HUMAN_INBOX_BRIDGE_AUTH_TOKEN
                          # generated locally like the codex-bridge tokens;
                          # GITHUB_TOKEN relayed as-is if set; both land in
                          # ~/.culture-nodes/human-inbox.env (0600) on the
                          # host serving company/human-ops
./deploy.sh thor          # + human-inbox lane: bridge AND tracker, together,
                           # on that same derived host — which may not be the
                           # host named on this command line

# verify (on the host the lane reported deploying to):
ssh <human-ops-host> 'systemctl --user status human-inbox-bridge'
ssh <human-ops-host> 'systemctl --user status human-inbox-tracker'
ssh <human-ops-host> 'journalctl --user -u human-inbox-tracker -n 50'
```

A healthy tracker start logs `bridge identity confirmed: <url> serves
actor company/human-ops (revision N, registered <endpoint>)`. A refusal
names both endpoints and exits non-zero — see
`adapters/human-inbox/README.md`'s "Startup identity check".

### Registering the `company/human-ops` actor

An operator/DB step, per `adapters/human-inbox/README.md`'s own
"Registering a `kind=human` actor" section — but note that it is what
DECIDES the deployment, not a formality after it. `register-actor.sh`
appends a revision the same way it does for codex actors, naming
`HUMAN_INBOX_BRIDGE_AUTH_TOKEN` as the `metadata.auth_token_env` and the
bridge's bound address (`http://<lan-ip>:<port>`) as `endpoint_ref`.

Moving the bridge to another machine is therefore a re-registration
followed by a deploy — never an edit to a host name in this repo. The
next `deploy.sh` follows the new endpoint, and any pair left behind on
the old host refuses to start rather than double-serving the actor.

### Secrets this task adds to install-secrets.sh

| Env var | Installed as | Source | Host(s) |
| --- | --- | --- | --- |
| `CULTURE_NODES_WEBHOOK_URL` / `DISCORD_WEBHOOK_URL` | a line in `prod.env` | relayed from the operator's own shell environment when set; never fabricated | thor only |
| `HUMAN_INBOX_BRIDGE_AUTH_TOKEN` | `~/.culture-nodes/human-inbox.env` (0600) | generated locally (`openssl rand -base64 32`), like the codex-bridge tokens | the host serving `company/human-ops` |
| `GITHUB_TOKEN` | `~/.culture-nodes/human-inbox.env` (0600) | relayed from the operator's own shell environment when set; never fabricated | the host serving `company/human-ops` |

All three follow the same discipline as every other secret in this file:
stdin over ssh, never argv; that lane's own `FORCE_*` switch required to
overwrite an existing value; nothing committed to this repo.
