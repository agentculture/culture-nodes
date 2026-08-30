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

## Bundled or external PostgreSQL

`install-secrets.sh` preserves the current topology explicitly: thor's
`COMPOSE_PROFILES=bundled-postgres,backup` starts the bundled database and
backup loop, while both thor and orin receive a host-appropriate
`NODES_DATABASE_URL`. `DATABASE_SSLMODE` is the TLS-mode input **used when
the URL is first composed**; the LAN default is `disable` under the network
trust decision below.

Read that "first composed" literally. Nothing else reads `DATABASE_SSLMODE` —
no compose service and no Go code — because the settings lane resolves it and
writes the mode into `NODES_DATABASE_URL` as a literal (see below for why a
`${DATABASE_SSLMODE}` placeholder is unsafe there). The URL is then
add-if-absent, so on a host that already has one, **editing `DATABASE_SSLMODE`
changes nothing and reports nothing**. To change the TLS mode of a provisioned
host, change the URL itself: `remove-secret.sh NODES_DATABASE_URL --yes <host>`
with the new `DATABASE_SSLMODE` in place, then re-run `install-secrets.sh` — or
edit the URL by hand, which the external-database path already expects.

To use an external database, edit `prod.env` on each host: set the same
provider URL in `NODES_DATABASE_URL` (using the provider-required sslmode),
remove `bundled-postgres` from `COMPOSE_PROFILES`, and keep `backup` only if
that external database should be dumped to thor's configured backup
directory. The backup service runs `pg_dump "$NODES_DATABASE_URL"`, so it
cannot silently continue dumping an unused local database. Removing
`backup` disables the loop explicitly. No Go/Python source or image changes
are involved.

An absent `NODES_DATABASE_URL` is a configuration error: Compose exits
during variable interpolation with `set the bundled or external PostgreSQL
URL in prod.env` before it starts any control-plane service.

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

### Deployment settings arrive by re-run, not by rotation

The `FORCE_PROD=1` guard above is what stops a re-deploy from rotating the
live database password — and for a while it was also what stopped an
already-provisioned host from receiving anything at all. The guard returns
*before* the key-by-key merge runs, so on any host that already had a
`prod.env`, the prod lane was a no-op. When `NODES_DATABASE_URL` later
became a required key, there was no way to deliver that one non-secret line
through the script except `FORCE_PROD=1`, which would have rotated every
generated secret in that host's block to do it. Both hosts were fixed by hand instead — thor mid-deploy,
and then orin, whose deploy never got that far and aborted outright at
compose interpolation. Two operator hand-turns for a value the script
already knew how to compose.

The non-secret population therefore has its own lane, which runs
**unguarded**. The answer to *compose says a variable is missing on a host
I already installed* is to re-run the script, with nothing set:

```bash
./install-secrets.sh            # delivers newly-required settings;
                                # rotates no secret, needs no FORCE_PROD
```

That lane is **add-if-absent**: it writes a key that `prod.env` does not
have and never touches one it does. The asymmetry is deliberate, and the
reason is a few sections up — "Bundled or external PostgreSQL" tells an
operator to point the stack at an external database by editing
`NODES_DATABASE_URL` and `COMPOSE_PROFILES` on the host. A lane that
re-asserted its own values every run would quietly revert that documented
choice on the next deploy, and the stack would come back up against the
bundled database having reported nothing.

The cost of that guarantee is worth stating plainly: **correcting a wrong
value is not a re-run.** A key that is present is a key the lane leaves
alone, however wrong it is. Remove it explicitly with `remove-secret.sh`
above, then re-run:

```bash
./remove-secret.sh NODES_DATABASE_URL --yes thor
./install-secrets.sh
```

Two properties of the URL that lane writes are invisible in the result and
easy to undo by accident:

- It is composed **on the host**, from that host's own `POSTGRES_PASSWORD`
  line in `prod.env` — the same discipline as every other secret here: the
  password crosses no wire and enters no argv. A host whose `prod.env`
  carries no `POSTGRES_PASSWORD` is refused by name, rather than handed a
  URL with an empty password in it.
- Its `sslmode` is written as a **literal value, never a
  `${DATABASE_SSLMODE}` placeholder**. Compose interpolates env-file values
  recursively, but only backwards: a placeholder resolves only when the key
  it names appears *earlier* in the file. In the other order compose
  resolves `sslmode=` to the empty string and reports no error at all —
  libpq then falls back to its own default, the stack connects, and nobody
  learns the TLS mode was never applied.

### The post-deploy credential audit

Merging fixed the mechanism that ate `NODES_ACTOR_CLAUDE_TOKEN`.
`audit-credentials.sh` is the **detector** for whatever eats a key next — a
hand edit, a restore from an older copy, a lane that was never taught to
install a key on this host. `deploy.sh` runs it **last**, after the stack is
up, on both lanes:

```bash
./audit-credentials.sh thor      # 0 = complete, 1 = a required key is gone
```

It compares the env keys **this host's compose file declares** against what
`~/.culture-nodes/prod.env` on that host contains, and puts every key in one
of three classes:

| class | meaning | audit behaviour |
|---|---|---|
| `required` | the service cannot work without it | **fails the audit** when absent or empty |
| `optional` | absence is a legitimate choice that *closes a feature* rather than breaking one (`DISCORD_WEBHOOK_URL`, the closed-by-default bearer secrets, the runner-service placement keys) | reported, never a failure |
| `unknown` | present in `prod.env`, declared by no compose file (`NODES_RUNNER_SECRET` is one on both hosts) | reported and **left alone** — `prod.env` legitimately carries keys compose never mentions; `remove-secret.sh` is the deliberate removal path |
| `forbidden` | a key that must **not** be here at all: any `*_DIAL_TOKEN`, because a dial-in credential has exactly one custody point and it is not this file | **fails the audit**, naming the key (see [Dial-in credentials](#dial-in-credentials-issue-111)) |

The declared set is **read from `compose.thor.yml` and `compose.orin.yml`**,
never from a list in the script, so it cannot drift from what compose
actually substitutes (`$${VAR}` is compose's escape for the container's own
shell and is correctly ignored). Compose also decides most of the
classification by itself: `${KEY:?…}` is required by construction and
`${KEY:-value}` works without the key by construction. The hand-classified
half is only the keys compose says nothing about — `${KEY:-}`, the shape
every credential has, including the one the incident destroyed — and it
lives in exactly one place, `audit_classification()` in
`audit-credentials.sh`, one entry per key with the reason it is where it is.
A compose-declared key with an open default that nobody classified is
reported as unclassified and treated as required until someone writes down
which it is.

Values never leave the host: the remote command emits `KEY<TAB>set|empty`,
so the audit reports key **names** only and no credential reaches an argv or
a log line. `tests/deploy/credentialaudit_test.go` runs the real script
against a stub `ssh` under a per-host `HOME` and pins all of it, including
the fixture that is missing one required key.

Known gap this surfaced on its first run: **`NODES_ACTOR_CLAUDE_TOKEN` is
not installed on orin.** `install-secrets.sh`'s relay lane targets `$THOR`
only, while `compose.orin.yml` declares the variable — so orin's worker
401s on any claude node run it claims. The audit reports it; installing it
is a change to `install-secrets.sh` that has not been made.

## Dial-in credentials (issue #111)

A bridge no longer waits at an address for the control plane to call it: it
**dials out** and identifies itself with a token **the control plane issued**
(`POST /v1alpha1/inbound/credentials`, issue #111's dial-in half). One
command issues and delivers one bridge's credential:

```bash
./issue-dialin-credential.sh company/codex-thor            # issue + deliver
./issue-dialin-credential.sh --revoke company/codex-thor   # end its authority
```

### One plaintext, one digest

This is the first bridge credential here with **one** plaintext custody
point. Every other one has two — the bridge holds the token and `prod.env`
holds the same token for the control plane to *present* when it dispatches
outbound — and `install-secrets.sh` has a lane per credential whose whole job
is keeping each pair in step.

Dial-in reverses the direction, so the control plane never presents the
value; it only verifies one, and keeps a SHA-256 verifier
(`inbound_authentication`, migrations 0031/0037). What is left is:

| copy | where | written by |
|---|---|---|
| the plaintext | the bridge's own per-bridge file | `issue-dialin-credential.sh`, and nothing else |
| the digest | `inbound_authentication.verifier_sha256` | `POST /v1alpha1/inbound/credentials`, and nothing else |

There is no lane that writes either one alone. Minting **replaces the
verifier and reveals the plaintext in the same request**, this script has no
mode that mints without delivering, and there is no mode that registers a
value an operator invented — the control plane chooses it with `crypto/rand`
and nothing can read it back. So a rotation replaces both copies or neither,
and issue #133's failure shape (one copy updated, another left stale, in
silence) has nowhere to occur.

What two machines cannot do is commit atomically, so the remaining case is
made **impossible to miss** rather than impossible:

- delivery is prepare-then-replace, so a failed write leaves the bridge's
  previous credential byte-intact;
- the deliverer recomputes the SHA-256 of what it received and refuses to
  write unless it equals the digest the control plane stored;
- a delivery that fails *after* a successful mint exits non-zero and names
  the party, which copy is ahead, and the repair (re-run — re-issuing
  replaces the verifier again, so there is nothing to reconcile by hand);
- `audit-credentials.sh` fails by name if a `*_DIAL_TOKEN` ever appears in a
  `prod.env`, since for a single-copy credential the only inconsistency
  `prod.env` can express is holding one at all — and it is not harmless:
  `notify-bridge.service` lists `prod.env` as an `EnvironmentFile`, so such a
  key would really be read.

### Nothing is relayed, nothing is local

The bearer that authorises minting
(`NODES_INBOUND_ISSUANCE_TOKEN_SECRET`) is **read on the control plane host
from its own `prod.env`** by the command that mints, and is never exported by
the operator, never put on an argv (`curl` takes its whole configuration on
stdin), and never returned. The credential itself flows from `curl` on the
control plane host straight into the delivering command on the bridge host
through a shell pipeline: it is never assigned to a variable in the script,
never written locally, never printed. The operator's process handles an actor
key, a host name, a URL and a digest.

`install-secrets.sh` installs the issuance bearer on thor **add-if-absent**,
outside the `FORCE_PROD` block, for both of that block's known problems —
issue #124 (a key added to the guarded block cannot reach an
already-provisioned host without rotating every secret beside it) and
issue #133. `FORCE_ISSUANCE=1` rotates it through the same protocol as
every other rotation here; doing so invalidates no issued credential, because
admission reads each party's stored verifier and not this bearer.

### Where a bridge reads it

The destination is **per bridge**, not per backend: spark runs four
claude-code bridges that share one prefix and one systemd `EnvironmentFile`,
so a per-backend destination would give all four the same identity
(issue #147). `dialin_bridges()` in the script carries one row per party, and
a row's destination is either:

- `env:<path>` — a single-purpose mode-0600 `EnvironmentFile` with the three
  settings `dialin.configured()` requires together. This is what the shipped
  bridge code reads today (`os.environ` only), and it is the default;
- `json:<path>` — the per-bridge JSON config, where every other per-bridge
  setting already lives.

Which one a bridge *reads* from is issue #147's decision (plan task t8); this
lane can already write either, so that decision does not need the script
rewritten. Add `EnvironmentFile=-%h/<path>` to the bridge's unit for an `env`
destination — the command prints the exact line.

## The runner is a host process (deviation d2)

`cmd/nodes-runner` runs under a **systemd user unit**
(`nodes-runner.service`), not in compose: the code-execution boundary
needs the host's Docker and the headspace CLI, and the control-plane
containers are deliberately socketless (`tests/deploy` enforces it).
`deploy.sh` installs the headspace CLI (`uv tool install headspace-cli`),
the binary, the env file, and the unit. The runner's bearer secret lives
in `~/.culture-nodes/runner.secret` on each machine, mirrored to the
operator's `~/.culture-nodes/runner-secret.<host>` for registry entries.

### Granted environment values (`environment_refs`)

A code operation can *name* an environment value it needs; the runner
resolves the name from its **own** process environment and refuses the
operation by name when it is unset. Values therefore live in
`~/.culture-nodes/runner.env` on the runner host — and because `deploy.sh`
rewrites that file every deploy, it also re-grants them, reading them from
the deploying operator's environment:

```bash
PR_UPKEEP_SWEEP_SOURCE_URL=https://…/sweep.py \
PR_UPKEEP_SWEEP_SOURCE_SHA256=$(sha256sum examples/pr-upkeep/sweep.py | cut -d' ' -f1) \
PR_UPKEEP_SWEEP_JIRA_SOURCE_URL=https://…/pr_upkeep_jira.py \
PR_UPKEEP_SWEEP_JIRA_SOURCE_SHA256=$(sha256sum examples/pr-upkeep/pr_upkeep_jira.py | cut -d' ' -f1) \
  deploy/prod/deploy.sh thor
```

Those four values are `examples/pr-upkeep`'s sweep script and Jira module
sources plus their expected digests (task t16): the workflow names *that it needs code*, this
deployment decides *whose*. Leave them unset on a host that does not run the
pr-upkeep loop — the sweep is then refused there by name, which is the
correct answer and not a silent fallback to someone else's code.

### Runner grants: what lives where, and how to put it back

Five grants keep the pr-upkeep loop running, and they live in **two files**
on each runner host, for one reason: `deploy.sh` rewrites `runner.env` on
every deploy, so anything that must survive a deploy without being retyped
belongs in the other file.

| Grant | File | Who writes it |
| --- | --- | --- |
| `PR_UPKEEP_SWEEP_SOURCE_URL` / `_SHA256`, `PR_UPKEEP_SWEEP_JIRA_SOURCE_URL` / `_SHA256`, `PR_UPKEEP_REPOSITORIES` | `~/.culture-nodes/runner.env` | `deploy.sh` (`lanes/runner-env-write.sh`), every deploy, from the deploying shell or by retaining the existing line |
| `JIRA_ACCOUNT_EMAIL` + `JIRA_API_TOKEN` | `~/.culture-nodes/runner-secrets.env` | `install-secrets.sh`'s Jira lane, **merged** — it replaces these two keys and no other |
| `GITHUB_TOKEN` | `~/.culture-nodes/runner-secrets.env` | **by hand.** No lane in this repo writes it |
| `SONAR_TOKEN` | `~/.culture-nodes/runner-secrets.env` | **by hand.** No lane in this repo writes it |
| `NODES_EVENT_TOKEN` | `~/.culture-nodes/runner-secrets.env` | **by hand.** No lane in this repo writes it |

The three hand-granted ones are why the Jira lane merges. It used to `cat >`
the whole file, so the 2026-08-29 cutover deploy — run by a shell holding no
Jira pair — reduced `runner-secrets.env` to 36 bytes of empty grants and took
the other three with it. 183 of the next 275 sweep runs were refused with
`rejected_input: environment_refs names GITHUB_TOKEN, SONAR_TOKEN,
NODES_EVENT_TOKEN, not set in this worker process's own environment`, over
sixteen hours (issue #253). Today the lane refuses to rewrite an existing
`runner-secrets.env` when the pair is unset, and says so by name; leaving the
pair unset is not a way to clear it.

Add a hand-granted value the same way, without going through a lane:

```bash
ssh thor 'umask 077; printf "SONAR_TOKEN=%s\n" "$TOKEN" >> ~/.culture-nodes/runner-secrets.env'
ssh thor 'systemctl --user restart nodes-runner'   # the unit reads it at start
```

**The deploy checks this before it ships anything.** `lanes/grant-check.sh`
runs in preflight: it reads the latest version of every `workflow_key` the
control plane can start today (one with a trigger, or one an enabled schedule
fires) and diffs the `environmentRefs` those versions declare against the key
*names* present in `runner.env` + `runner-secrets.env` on the host. A missing
grant fails the deploy, naming the key and the workflow that declares it,
while nothing on the host has been touched. It prints key names only — never a
value, on any path — so the refusal can be pasted into an issue as-is. When
the control plane cannot be reached it prints a `WARNING` and proceeds: an
unreachable control plane is a state a deploy is often the fix for.

**Rollback.** Every lane that rewrites either file copies the previous bytes
aside first, as `<file>.bak-<UTC timestamp>` in the same directory, mode 600,
and prints the restore command in the deploy log — for example:

```text
==> backed up runner-secrets.env on thor to /home/ori/.culture-nodes/runner-secrets.env.bak-20260830T172214Z
==> restore it with: ssh thor 'cp /home/ori/.culture-nodes/runner-secrets.env.bak-20260830T172214Z ~/.culture-nodes/runner-secrets.env'
```

Restore, then `ssh thor 'systemctl --user restart nodes-runner'` — the unit
reads both files at start, so an unrestarted runner keeps serving the grants
it booted with. The ten most recent backups of each file are kept and older
ones removed: each one is a second copy of live credentials, so the trail is
bounded on purpose.

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

### Handover evidence (task t10, issue #13)

When a dispatch hands its changes over as a git ref (`input.handover`, see
each bridge's README), the control plane can fetch that ref and record what
it measured — ref, commit sha, changed paths — as an `observed` ledger
record beside the agent's own proposed claim. It is **off unless
configured**, and both variables must be set together or the process
refuses to start:

- `NODES_HANDOVER_REMOTE` — the git remote **this host** fetches handover
  refs from. Deliberately the operator's configuration and never the remote
  a bridge reported: fetching from an agent-supplied url would let a session
  choose the repository its own work is measured against.
- `NODES_HANDOVER_ACTOR_ID` — the **registered** actors-table row the
  observation is attributed to (`ledger_records.origin_actor_id` is a
  foreign key to `actors(id)`), the same registration obligation
  `NODES_CODE_RUNNER_ACTOR_ID` carries.
- `NODES_HANDOVER_ACTOR_REVISION` — optional revision pin for that producer.
- `NODES_HANDOVER_OBJECT_DIR` — optional persistent bare repo fetches reuse
  objects from; unset means a fresh temp dir per fetch.

Set them on **both** `api` (the async `completed` callback lands there) and
`worker` (the synchronous 200 lands there), with the same values.

### Automated feature-branch merge custody

The Jira-driven shipped loop has one concrete merge custody point: the
`company/codex-thor` actor on host **thor**, in checkout
`/home/thor/git/culture-nodes-agent`. Its merge operation runs `nodes-merge`
after the gate-report response says `gates_passed` for the exact candidate
commit. The command rechecks that digest, requires the candidate to be the
two-parent commit produced by `merge --no-ff`, atomically advances the feature
branch from the candidate's first parent, and pushes that same commit.

The push credential is `GITHUB_TOKEN_WORKER`, the #90 seam already installed
as mode-0600 `~/.culture-nodes/bridge-push.env` and loaded by
`codex-bridge.service`. The value is environment-only. `nodes-merge` accepts
no credential flag, supplies the value to Git through a temporary
`GIT_ASKPASS` helper, disables terminal prompting, removes the helper after
the push, and redacts the credential even if a remote echoes it in an error.
Consequently this step needs no operator token handoff and places no token in
argv, logs, workflow content, or the checkout.

With neither set, nothing is fetched and no record is written — which is the
honest default: a control plane that cannot look must not write a record
saying it did. A ref that is claimed but not fetchable also writes nothing;
the reason goes to the process's diagnostic stream, never to the ledger.

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

### Ticket page links are LAN addresses (task t16)

`NODES_UI_BASE_URL` is the origin culture-nodes puts in front of every ticket
page link it posts on a Jira issue. Without it the page-link comment read
`/tickets/SCRUM-N` — a path with no origin, which Jira renders as plain text.
`install-secrets.sh` now writes the key into both hosts' `prod.env`, and both
compose files declare it for every service that can mint a run (api, scheduler
and worker on thor; the worker on orin — the comment is rendered by whichever
process claimed the work, and the two machines share one namespace).

**The link it produces is reachable from the LAN or tailscale only, and that is
the accepted state until the OAuth cycle.** The default is thor's API origin —
the control-plane host you invoked the script with, so
`./install-secrets.sh 192.168.1.146 orin` produces
`http://192.168.1.146:18080/tickets/SCRUM-N`. A reader looking at that comment
in Jira or Discord from off the network sees a link they cannot open. Nothing
about the ticket is hidden from them — the Jira issue itself is where the
decision is recorded — but the *page* is not public, and no part of this
deployment pretends otherwise (see "Network trust" above: none of these ports
should be forwarded beyond the LAN to make the link work).

Both hosts get **thor's** origin, not their own: orin serves no API, so a link
to orin would 404 for every reader.

To point the links at any other origin — a reverse proxy, a tailscale name, or
whatever the OAuth cycle lands on — export it and re-run:

```bash
NODES_UI_BASE_URL=https://nodes.example.net ./install-secrets.sh
```

The install log says which of the two it used (`exported for this run` versus
`defaulted to the control-plane API origin`), because the two produce
identically-shaped `prod.env` lines and only one of them is reachable from
outside. And because this lane is add-if-absent (above), changing an origin a
host already carries is `remove-secret.sh NODES_UI_BASE_URL --yes <host>`
followed by a re-run, not a re-run alone.

## Telemetry (telemetry profile, issue #5)

Off by default, twice over: the `otel-collector` service sits behind the
`telemetry` profile, and the control plane builds no exporter at all unless
`OTEL_EXPORTER_OTLP_ENDPOINT` is set (`internal/telemetry.New` returns
`NoOp()` — no exporter, no goroutine, no dial). Turning it on is one line in
`~/.culture-nodes/prod.env`:

```text
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317
```

plus starting the collector: `COMPOSE_PROFILES=telemetry docker compose
--env-file ~/.culture-nodes/prod.env -f compose.thor.yml up -d`. orin's
worker points at thor's (`http://thor:4317`, port 4317 published on the LAN
for exactly that, per Network trust above); leaving it unset there is a
supported state.

`api`, `scheduler` and both workers carry the variable, because all three
seams `internal/telemetry` instruments live in those processes. Pointing at
a different collector — Jaeger, Tempo, a vendor endpoint — is that
variable's value and nothing else.

Read `docs/operations/telemetry.md` before believing a trace: the three
seams share a `run_id`, not a trace id, because this control plane
propagates no W3C trace context across the actor boundary.

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
  Since #243 the unit is installed into the **`culture-codex` account's**
  `systemd --user` instance, not the login user's: `deploy.sh` reaches it
  over `ssh culture-codex@<host>`, `%h` resolves to
  `/home/culture-codex`, and the bridge process — and every `codex exec`
  it spawns — runs as that account. The unit file is unchanged; what
  changed is whose instance it lands in. `nodes-runner`, compose, the
  backups and `prod.env` stay under the login user, which is the only
  account on the host in `docker`. The same shape holds on spark, where
  the four claude bridges run under `culture-claude` and `qwen-developer`
  under `culture-qwen`, installed by `deploy.sh spark` (bridge lanes only,
  through `ssh culture-<engine>@localhost`). Spark's bridges are thereby
  stamped `uv tool install` **copies**, no longer editable installs from
  the operator checkout — so a bridge fix on spark takes effect only after
  `deploy.sh spark`, and a stale copy is indistinguishable from a refusal
  until it is redeployed (issue #120). What the account does and does not
  fence is recorded in `docs/deviations/2026-08-29-agents-as-os-users.md`.
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

### Unprivileged user namespaces (issue #63) — optional since #243

**Since #243 this section is advisory.** The codex session runs as the
`culture-codex` account, and POSIX ownership — no `sudo`, no `docker`
group, a 0750 home, an unreadable operator checkout — is the boundary.
The bubblewrap sandbox codex still wraps around its commands is a second,
optional layer: `codex-preflight.sh` check 7 (the `bwrap` probe below)
now **warns** instead of refusing the deploy, the bridge no longer blocks
dispatch when `sandbox_modes_unavailable` is set, and what refuses instead
is an account gate — the account exists, is in neither `sudo` nor
`docker`, and owns the allowlisted checkout. The probe, `nodes doctor`'s
`unprivileged_userns` check and the capability surface's confinement prose
all stay, so a host that *can* still nest a sandbox says so. The sysctl
below is therefore something a host may keep or drop; a fresh host no
longer needs it to run a codex actor. The paragraphs that follow describe
the posture before #243 and remain accurate for a host that keeps it.

Before #243, **a fresh Ubuntu 24.04 host could not run a codex actor until
this was changed.** Codex sandboxes every shell command it runs inside a user
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
bridge unit runs the preflight as `ExecStartPre` — before #243 a host in
this state failed to start its bridge instead of accepting work it could
not do; since #243 the same probe reports, and the account gate decides.

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

`--os-user NAME` is sugar for `--metadata os_user=NAME` — a first-class
metadata key (issue #204) that records the dedicated Unix account a bridge
runs as (`culture-codex`, `culture-claude`, `culture-qwen`), so the registry
can be read as a lane tag. `NAME` must match `^[a-z_][a-z0-9_-]*$`; an
invalid name is refused with a `hint:` before any Postgres access. Like
every other `--metadata` key, `os_user` is merged into the previous
revision's metadata, never replaced — a later registration that only asks
for a different key still carries `os_user` forward.

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
ssh culture-codex@<host>    # thor or orin — the account the bridge runs as
~/.local/bin/codex login
systemctl --user restart codex-bridge
```

**checkout harvest / reset (after a write task).** Production codex
nodes default to `sandbox: read-only`; only an explicit write task dirties
the agent checkout. Since #243 that checkout belongs to the engine
account, so the harvest is an ssh *into the account*, not into the host:
`ssh culture-codex@<host>` on thor and orin (`@localhost` on spark), and
the tree is `/home/culture-codex/git/culture-nodes-agent` on thor/orin,
`/home/culture-claude/git/culture-nodes-<role>` (one clone per claude role:
developer, planner, verifier, intake) or
`/home/culture-qwen/git/culture-nodes-qwen-developer` on spark. Because
`deploy.sh` refuses to fast-forward (or deploy over) a dirty/diverged
checkout (c6, h6), an operator closes the loop before the next deploy
rather than leaving it blocked:

```bash
ssh culture-codex@<host>    # thor or orin; culture-claude@localhost on spark
cd ~/git/culture-nodes-agent    # or ~/git/culture-nodes-<role> on spark
git status                  # review what a write task changed
git diff                    # inspect it
# commit what's approved through the normal PR lane (push a branch, open
# a PR — never push directly to main from the host; the Protect main
# ruleset refuses it anyway), then:
git reset --hard && git clean -fd
```

A networked account can push its own handover ref with the Contents-only
`GITHUB_TOKEN_WORKER` in its `bridge-push.env`, so a package that pushed
needs no harvest at all — fetch the ref from origin. The runbook above is
for the packages that did not.

The **old trees** under `/home/<login>/git/culture-nodes-agent` on thor and
orin stay in place: the unix-user lane never touches `/home/<login>/git`,
their unpushed commits and unmerged branches are exactly where they were,
and they remain harvestable exactly as before — `ssh <host>`, `cd
~/git/culture-nodes-agent`, the same `git status` / `git diff` / fetch
over `<host>:git/culture-nodes-agent <branch>` — until they are emptied.
Nothing migrates them; the account clones fresh from origin.

Skipping the reset does not corrupt anything — it blocks the *next*
`deploy.sh <host>` run with a clear refusal message, checkout untouched
(c6, h6).

**rollback to the login-user posture (no deploy).** The cutover disables
the old login-user bridge units but leaves their files, configs and env
in place, so restoring the prior posture is one command pair per host,
run as the two users in that order:

```bash
ssh culture-codex@<host> 'systemctl --user stop codex-bridge'   # as the account
ssh <host> 'systemctl --user start codex-bridge'                # as the login user
```

The unix-user lane prints this pair in its deploy summary. On spark the
same pair applies per unit (`culture-nodes-claude-<role>` under
`culture-claude@localhost`, `culture-nodes-qwen-developer` under
`culture-qwen@localhost`, then the login user). Re-running `deploy.sh
<host>` moves the bridge back into the account.

**first deploy of a host (`FIRST_DEPLOY=1`).** The preflight runs `nodes
doctor` on every host the lane is about to modify *before* anything is
shipped or stopped, and it fails closed: a host with no
`~/git/culture-nodes-agent` checkout or no `~/.local/bin/nodes` cannot be
doctored, so the deploy is refused. The one exception is a host that has
never been deployed, and it is declared, never inferred from a missing file:

```bash
FIRST_DEPLOY=1 ./deploy.sh orin     # operator's shell; the lane carries it to the host
```

The lane passes the declaration inside the remote command as a normalised
`0`/`1` — ssh forwards none of the operator's environment, and only the
literal value `1` counts. The post-deploy doctor still gates that deploy
once the codex lane has installed the checkout and the CLI.

The declaration names one host: the one you are deploying. The thor lane
doctors orin too (it stops and restarts orin's worker), and an orin that has
a worker stack has been deployed — so `FIRST_DEPLOY=1 ./deploy.sh thor` does
not exempt it. If orin's checkout or CLI is missing then, that is a
restore-the-checkout state and the deploy is refused, as without the flag.

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
- **Runtime.** The tracker exits non-zero at startup when its bridge is
  not the actor's bridge (task t8, `verify_bridge_serves_actor`). Since
  task t7 it establishes that without any address: it asks the bridge on
  `GET /identity` for the `store_id` of the state directory that bridge
  owns and compares it against the one it reads off the local filesystem
  — proof that the process it submits to is the process whose task store
  it is emptying — then reads `GET /v1alpha1/dial-in-presence` to check
  that the actor's work is dispatched to a bridge that dials in as this
  one does. `deploy.sh` writes `HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL`
  precisely so that second half is armed; unset, the co-location proof
  still runs and only the dispatch half degrades to a warning.

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

## Human-task fan-out and merged-PR expiry (task t11, spec c6)

A pending human decision used to be visible on exactly two pages a person has
to go and look at, `/inbox` and `/decisions`, and nobody is paged to either. On
2026-08-30 that left 26 pending `human-merges-pr` approvals on prod whose pull
requests had all already merged.

The control plane now queues a fan-out in the same transaction that creates the
task (`human_task_fanout_outbox`, migration 0051) and the scheduler drains it
(`internal/humanfanout`). Nothing is delivered from the run transaction, and
`UNIQUE (human_task_id, channel)` means one task can never announce itself
twice.

### What each host must have for it to work

| What | Where | Why |
| --- | --- | --- |
| `NODES_UI_BASE_URL` | `~/.culture-nodes/prod.env` on every host that mints runs | the comment and the Discord post carry the decision page link; unset renders a bare path, which Jira shows as text |
| `JIRA_TRANSITION_TARGET` includes `Pending` | the jira bridge's own environment | the bridge's allowlist is the enforcement point for `transition_issue`. Since t11 it is a **comma-separated list** and a single value still works unchanged: `JIRA_TRANSITION_TARGET=Done,Pending` |
| `company/notify-discord` registered | `actors` table | the Discord half of every fan-out is dispatched through the same bridge `examples/notify-message` uses; an unregistered key fails that one row and leaves the queue alone |
| `human_task_expiry` registered | `actors` table | `deploy/prod/register-actor.sh --engine human_task_expiry`. An expiry appends one `derived` ledger record and `ledger_records.origin_actor_id` is a foreign key to `actors(id)`, so without it every expiry refuses. Override the id with `NODES_HUMAN_TASK_EXPIRY_ACTOR_ID` |

### Clearing the stale ones

The periodic consumer only expires tasks the control plane holds a delivered
`pr.merged` fact for, and the sweep only emits that fact for a pull request
whose branch or body carries a correlatable Jira key. The approvals that
accumulated carry none, so they need the backfill, which is a **dry run until
`--apply`**:

```bash
nodes expire-approvals --namespace "$NS"                       # what would happen
nodes expire-approvals --namespace "$NS" \
  --pr agentculture/culture-nodes#236 \
  --pr agentculture/culture-nodes#238 --apply
```

`--pr` makes the OPERATOR the source of the merge fact, and the recorded expiry
detail says so — that is a weaker provenance than a delivered fact and it reads
as such in the ledger afterwards, deliberately.

### What a PR-sourced run does NOT get

A GitHub PR comment. No actor registered here advertises a verb that writes to
a pull-request thread: the bridge that reads a PR thread writes only to its own
submit surface, and the agent bridges expose no GitHub write capability at all.
A PR-sourced task therefore fans out to Discord only. Widening it is a new
migration (0051's `channel` CHECK) plus a branch in
`engine.PlanHumanTaskFanOut`, once a bridge advertises the capability.
