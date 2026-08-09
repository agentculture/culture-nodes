# Culture Nodes — setup and tour

The shortest honest path from `git clone` to watching a workflow run, and
what each screen is telling you once it does.

> **Every node has a contract. Every result has evidence.**

## 1. Run it

### Docker Compose (the one-command path)

Everything local: API + scheduler + worker + PostgreSQL + MinIO, with the
web UI embedded in the same binary.

```bash
cd deploy/compose
cp .env.example .env       # required: the compose file ships no password literals
docker compose up --build
```

Open <http://localhost:8080> — the UI is served by the control plane itself
(no separate frontend container). `POST`-able API lives under
`/v1alpha1`; `GET /v1alpha1/healthz` says `{"status":"ok"}` when it's up.

### Kubernetes (Helm)

```bash
helm install nodes deploy/helm/culture-nodes
```

The chart deploys per-role Deployments (api / scheduler / worker) of one
image, applies migrations via a pre-install Job, defaults the worker to
`replicas: 2` — safe by construction: leases, fencing tokens, and
`SKIP LOCKED` claiming mean identical pods never double-work — and exposes
the API (which is also the actor-callback surface) through an optional
Ingress.

> **Trust boundary:** Phase 1 runs **authless behind a private network**.
> Anyone with network reach has full control. Private cluster/VPC only;
> OIDC lands in Phase 2.

### Dev mode (no containers)

```bash
export NODES_DATABASE_URL=postgres://...   # any Postgres 15+
go run ./cmd/nodes migrate
go run ./cmd/nodes all                     # serve + scheduler + worker in one process
```

## 2. Give it work

Publish the reference workflow (the PRD's delivery loop: intake → plan →
build → test → verify, with `test.failed` and `verify.changes_required`
looping back to build) and create a run:

```bash
export NODES_API_URL=http://localhost:8080
uv run nodes workflow publish examples/delivery-loop/workflow.yaml
# → digest: sha256:…  (immutable, content-addressed; runs pin it forever)

uv run nodes run create --workflow sha256:… --input examples/delivery-loop/input.json
uv run nodes run events <run-id>           # live SSE stream, one line per committed event
```

Agent nodes need an actor to talk to: an external process — on another
machine or container, never inside culture-nodes — speaking the actor
protocol. Register its endpoint in the `actors` table and the worker
dispatches to it; `adapters/colleague/` is the working reference (its
`README.md` covers config, and the actor's `origin.actor_id` must be its
*registered* actor row id). Code nodes go through the runner boundary —
AWS Lambda in the cloud, the headspace-cli bridge on a dev host.

## 3. What you're looking at

![The runs list — one line per run: state, workflow digest, created](assets/runs-list.png)

`/runs` — every run, each pinned to an immutable workflow digest; nothing
here ever resolves "latest".

![The Run view — the delivery-loop graph live, first node ready](assets/run-view-light.png)

`/runs/:id` — the run as a live graph: solid edges have been walked, dashed
edges are still possibilities, and the loop edges back to `build` are
declared domain outcomes, not failures.

![The same run in dark mode](assets/run-view-dark.png)

Dark mode follows `prefers-color-scheme`, using the design tokens pinned
from agentculture.org (`web/src/culture-design/`, ADR 0001).

![The node detail panel — contract digest, owner, attempts, ledger delta](assets/node-detail.png)

Click a node (or Tab to it and press Enter — the canvas is fully
keyboard-operable): its contract digest, owner, every attempt with fencing
token, and the ledger records that attempt appended.

![The Ledger view — records with authority chips](assets/ledger-view.png)

The work ledger is the run's meaning: agents can only *propose* (dashed
chip), humans confirm, trusted runners observe what they measured,
validators derive — so a completion claim that nothing verified stays
visibly unverified.

## 4. Poke at it from the CLI

Everything the UI renders is the same API the CLI fronts:

```bash
uv run nodes run get <run-id>                      # run + tokens + node runs + attempts
uv run nodes ledger records <run-id>               # the append-only record stream
uv run nodes ledger projection <run-id> ready_tasks
uv run nodes review create <run-id> --records id1,id2 --ledger-version N
uv run nodes review commit <review-id> --confirm id1 --ledger-version N
```

Add `--json` to any verb for the raw API payload, byte-exact.

## 5. Tear it down

```bash
docker compose down -v        # compose profile
helm uninstall nodes          # k8s
```

## Going deeper

- [`docs/initial-design/culture-nodes-prd-spec.md`](initial-design/culture-nodes-prd-spec.md) — the full PRD.
- [`docs/acceptance.md`](acceptance.md) — the Phase-1 checklist mapped to test evidence.
- [`docs/benchmarks.md`](benchmarks.md) — recorded idle-memory and throughput numbers.
- [`docs/adr/`](adr/) — the design decisions, including the pinned org design revision and the Lambda IAM model.
- [`adapters/colleague/README.md`](../adapters/colleague/README.md) — running a real external agent against a workflow.
