# deploy/compose

The Docker Compose local development profile (PRD §19.1, task t23):
`docker compose up --build` starts the complete local system — PostgreSQL,
MinIO, and the control-plane binary running as four separate role
processes (migrate, api, scheduler, worker) — in one command.

## Quickstart

```bash
cd deploy/compose
cp .env.example .env   # dev-only defaults; edit if you like
docker compose up --build
```

This builds `culture-nodes:local` from the repo root's `Dockerfile` once
and starts it under four commands (`migrate`, `serve`, `scheduler`,
`worker`), alongside `postgres:17-alpine` and `minio/minio`. `migrate`
applies the schema and exits; `api`, `scheduler`, and `worker` wait for
that and for Postgres to report healthy before starting
(`depends_on: condition: service_completed_successfully` /
`service_healthy`).

Once it is up:

- API: `http://localhost:8080` — `curl http://localhost:8080/v1alpha1/healthz`
  and `.../readyz`; every operation `api/openapi/openapi.yaml` documents
  (workflows, runs, ledger, reviews, SSE) is served from here.
- MinIO console: `http://localhost:9001` (login from `.env`'s
  `MINIO_ROOT_USER`/`MINIO_ROOT_PASSWORD`); S3 API on `:9000`.
- Postgres: `localhost:5432` (login from `.env`'s `POSTGRES_USER`/
  `POSTGRES_PASSWORD`/`POSTGRES_DB`) if you want to inspect the schema
  directly.

**UI**: `http://localhost:8080` also serves the web UI — the Dockerfile
builds `web/` (Vite) and embeds it into the `nodes` binary (`-tags
embedweb`), so there is no separate frontend container. It renders the Runs
list, the Run view, and the Ledger view against the same `/v1alpha1` API the
CLI uses; see the root [`README.md`](../../README.md#see-it-running) for
what each looks like. If you are iterating on `web/` itself and want hot
reload instead of rebuilding the image on every change, point a
locally-run Vite dev server at this same API:

```bash
cd web
npm install
VITE_API_URL=http://localhost:8080 npm run dev
```

Bring the stack down (and drop its volumes, for a clean-slate restart):

```bash
docker compose down -v
```

See `deploy/compose/smoke.sh` for a scripted end-to-end exercise of this
same stack (validate → publish → create a run → inspect it), used as this
profile's manifest-adjacent live check.

## Service map

| Service | Image | Command | Role |
|---|---|---|---|
| `postgres` | `postgres:17-alpine` | — | The authoritative store (PRD's "Runtime" ground rule). |
| `minio` | `minio/minio:latest` | `server /data --console-address :9001` | S3-compatible artifact storage. |
| `migrate` | `culture-nodes:local` | `migrate` | One-shot: applies pending schema migrations, then exits. |
| `api` | `culture-nodes:local` | `serve` | `nodes serve` — the HTTP API on `:8080`. |
| `scheduler` | `culture-nodes:local` | `scheduler` | `nodes scheduler` — durable timers + expired-lease sweep. |
| `worker` | `culture-nodes:local` | `worker` | `nodes worker` — claims ready work and dispatches it to actors. |
| `colleague-bridge` *(profile `agents`)* | `culture-nodes-colleague-bridge:local` | — | Example EXTERNAL agent host (see below). Off by default. |

`api`, `scheduler`, `migrate`, and `worker` all run the **same** image
(`culture-nodes:local`, built once from the repo root `Dockerfile`) under
different commands — one role per container, mirroring the per-role
topology `deploy/helm` (t22) uses in a cluster. None of the four mounts a
volume, and none is `privileged` or adds capabilities;
`tests/deploy/compose_test.go` asserts both as a manifest test, not just a
doc promise.

## The code-runner boundary

**No service in this file mounts a Docker socket into a control-plane
container, and none is going to** (spec claim c4, honesty condition h4).
`migrate`, `api`, `scheduler`, and `worker` never shell out, never touch
`/var/run/docker.sock`, and never execute repository code directly — the
only code-execution path the product allows is through the runner
boundary (the headspace-cli bridge, AWS Lambda, or a runner-protocol
service — options below), and that boundary is always a *separate process*,
never something a container in this file reaches into.

Concretely, for local dev, a `code` node has two honest options today:

1. **A host-run worker with the headspace bridge.** Run a second worker
   directly on your machine (outside Docker, where `headspace` and Docker
   itself are actually installed) pointed at the same database:

   ```bash
   NODES_DATABASE_URL=postgres://nodes:nodes-dev-only@localhost:5432/nodes?sslmode=disable \
   NODES_NAMESPACE_ID=<the real default namespace id> \
     nodes worker
   ```

   This worker is not a container in this compose file and is not
   subject to the no-Docker-socket rule above, because it is not a
   control-plane container — it is a host process that happens to
   have Docker (for headspace's `--provider docker`) and `headspace`
   installed, exactly the way a human developer's machine does. See
   task t18's headspace bridge (`internal/runners/headspace`) for what
   it drives.

2. **The Lambda runner.** Point `code` nodes at real AWS Lambda
   functions (`internal/runners/lambda`) and accept that code execution
   happens in the cloud even during local development — no local
   runner needed at all.

3. **A [runner-protocol](../../api/runner-protocol/README.md) service —
   `cmd/nodes-runner`, or your own.** Run the reference runner service
   (which wraps the same headspace bridge as option 1, but behind the HTTP
   contract and as its own process, with mandatory bearer auth) on any
   machine that can reach this compose stack's Postgres and that this
   `worker` can reach over the network:

   ```bash
   NODES_RUNNER_SECRET=replace-me \
   NODES_RUNNER_STATE_DIR=/var/lib/nodes-runner \
   NODES_RUNNER_HEADSPACE_PROVIDER=docker \
   NODES_RUNNER_HEADSPACE_PROFILES=sha256:<pinned-digest>=python3.12 \
     go run ./cmd/nodes-runner --listen :8090
   ```

   Then register a `ServiceIdentity` for it (endpoint + the same pinned
   digest + a `secret_ref`) in `internal/runners.FunctionRegistry` — see the
   protocol doc's "How a node reaches a runner" section. This is the same
   contract a **hand-rolled runner in any language** implements to become a
   valid execution target: nothing about it is specific to headspace or to
   this reference binary. `tests/runnerconformance` is the acceptance bar —
   an implementation that passes it unmodified is a conformant runner,
   whether it is `cmd/nodes-runner` or something you wrote yourself.

What this compose file's `worker` service does NOT do: it does not run
`code` nodes. It has no Docker socket and no `headspace` binary — by
design, not by omission. It still claims and dispatches `agent`/
`action.http` nodes to actors reachable over the network (real ones, or
the `colleague-bridge` example below), and it still runs the engine's
completion path for whatever it claims — it is simply never the thing
that runs untrusted code.

### Why the fixture run in `smoke.sh` does not fully complete

`docker compose up`'s `worker` has no actor registered for
`deploy/compose/testdata/smoke.workflow.yaml`'s `uses: actor://...`
reference, and its `NODES_NAMESPACE_ID` is a placeholder that will not
match the random id `nodes serve` generates for the "default" namespace on
first boot (there is no namespace-discovery endpoint yet — PRD §26). So the
smoke run either sits `running`/`created` with its entry node `ready`
(the worker never claims it), or — if you register a real actor and fix
`NODES_NAMESPACE_ID` — the worker claims it, fails to resolve the actor,
and cleanly marks the attempt failed (`policy_denied`, not retried). Either
outcome proves the same thing: the engine, the database, and the queue
work end to end inside these containers. Making a full run *complete*
locally needs a real actor listening somewhere the worker can reach —
`colleague-bridge` below, [your own actor
implementation](../../api/actor-protocol/README.md), or a workflow whose
actors point at `http://host.docker.internal:<port>` for something running
on your host.

### Why `api`, `scheduler`, and `worker` have no container healthcheck

The `culture-nodes:local` image is built `FROM
gcr.io/distroless/static-debian12:nonroot` — no shell, no `curl`, no
`wget`, nothing a Docker `healthcheck.test` could invoke from inside the
container (`docker run --entrypoint sh gcr.io/distroless/static-
debian12:nonroot` fails with *"exec: sh: executable file not found in
$PATH"*). `nodes doctor` doesn't probe HTTP by default, so it isn't a
substitute either. Adding a shell or a probe binary to the runtime image
just to satisfy a healthcheck would reopen the exact attack surface the
distroless base exists to close, so this profile deliberately omits a
container-level healthcheck for these three services and relies on
`depends_on` (`migrate` completing, `postgres`/`minio` reporting healthy)
for startup ordering. Actual readiness after that point is `GET
/v1alpha1/readyz` — poll it from outside the container, the way
`deploy/compose/smoke.sh` does.

## colleague-bridge (agents profile)

The `colleague-bridge` service is off by default. It ships as its **own**
image (`adapters/colleague/Dockerfile`, not the control-plane image) and
exists here to demonstrate, locally, what a real deployment does on a
**separate machine**: the control plane reaches an external agent host
over the network like any other actor (spec claims c24/c31), and that
host — never the control plane — is the thing that shells out to an agent
backend (`colleague work`, here).

```bash
docker compose --profile agents up --build
```

With `COLLEAGUE_ENGINE=mock` (the `.env.example` default) it runs a
deterministic offline actor useful for a smoke test, no real model
required. To register it as an actor for a workflow node's `uses:`
reference, insert a row in the `actors` table pointing at
`http://colleague-bridge:8085` (there is no actor-registration HTTP
endpoint yet — see `internal/worker/registry.go`'s `DBRegistry`), then
point `NODES_NAMESPACE_ID`/a workflow's `uses:` at it accordingly. See
`adapters/colleague/README.md` for the full actor-protocol mapping, the
`repo_allowlist` requirement (empty by default — the bridge refuses every
invocation until you set `COLLEAGUE_BRIDGE_REPO_ALLOWLIST` and mount the
repo it names), and the colleague contract v1 pin.

`colleague-bridge` is the one adapter this compose file wires a profile
for; its two siblings — [`adapters/claude-code`](../../adapters/claude-code/README.md)
(headless `claude -p`) and [`adapters/codex`](../../adapters/codex/README.md)
(headless `codex exec`) — follow the identical actor-protocol shape and are
run the same way (their own README's "Running it" section), just not yet
wired into this particular manifest. Any of the three, or a fourth you write
yourself, is registered the same way: a base URL in the `actors` table.

## Images

The control-plane binary (`cmd/nodes`) is published as a multi-arch
(linux/amd64, linux/arm64) OCI image to `ghcr.io/agentculture/culture-nodes`
by the `release.yml` workflow on every `v*` tag, alongside the existing PyPI
lane for the Python mesh-agent surface. Images are pushed **by digest** —
the `<tag>` and `latest` tags both point at that digest, and the digest
itself (not a mutable tag) is what any compose profile added here should
pin, e.g. `ghcr.io/agentculture/culture-nodes@sha256:<digest>`.

This compose file builds `culture-nodes:local` from source
(`build: {context: ../.., dockerfile: Dockerfile}`) instead, so
`docker compose up --build` works standalone against a checkout with no
registry access required — the tradeoff local development wants. Swap the
`build:` block for `image: ghcr.io/agentculture/culture-nodes@sha256:<digest>`
(dropping `--build`) if you would rather run a published image against
this same profile.
