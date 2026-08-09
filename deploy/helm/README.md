# deploy/helm

The `culture-nodes` Helm chart (`deploy/helm/culture-nodes/`): the api,
scheduler, and worker Deployments of one control-plane image, plus
PostgreSQL, a migration Job, and (optionally) an Ingress for the actor
callback surface (PRD §19).

## Phase 1 trust boundary — read this first

> **WARNING.** Phase 1 runs **AUTHLESS** behind a private network: anyone
> with network reach to the api Deployment — including the actor callback
> route, `POST /v1/attempts/{id}/events` — has full control. There is no
> username/password, no API key, no mTLS anywhere in this chart. **Deploy
> only on a private cluster/VPC**, or behind an authenticating proxy you add
> yourself. OIDC lands in Phase 2.

This is spec decision c45 and `internal/api`'s own package doc ("Authless by
design"), not an oversight of this chart. `helm install`/`helm upgrade`
print the same warning via `templates/NOTES.txt`.

## What this chart deploys

The deployable set is exactly the control-plane Deployments plus Postgres —
nothing else:

| Resource | Component label | Purpose |
| --- | --- | --- |
| Deployment | `api` | Serves `nodes.culture.dev/v1alpha1` (`api/openapi/openapi.yaml`) and, when `callback.enabled`, the actor callback route |
| Deployment | `scheduler` | Fires durable timers, sweeps expired leases (PRD §12.7, §20.4) |
| Deployment | `worker` | Claims and dispatches ready work (PRD §12.1) |
| StatefulSet | `postgres` | Self-contained `postgres:17`, only when `postgresql.enabled` (default `true`) |
| Job | `migrate` | `nodes migrate`, run as a `pre-install,pre-upgrade` hook |
| Service | `api` | Always the in-cluster callback surface, regardless of Ingress |
| Ingress | `api` | Optional, `ingress.enabled` — the callback surface for actors *outside* the cluster |
| PodDisruptionBudget | `worker` | `minAvailable: 1`, when `worker.podDisruptionBudget.enabled` |

All three role Deployments and the migration Job run the **same image**
(`cmd/nodes`) in different modes — `serve`, `scheduler`, `worker`,
`migrate` — never three separate images.

## Per-role replicas

- **api** — `api.replicas` (default 1). Stateless; scale for throughput or
  availability like any HTTP service.
- **scheduler** — `scheduler.replicas` (default 1). Every instance runs the
  identical loop and contends for one Postgres advisory lock
  (`internal/scheduler`'s package doc); exactly one is ever *active*, the
  rest are standbys that take over automatically when the active instance's
  session ends. More replicas buys availability, never throughput, and
  there is no "which one is active" setting to get wrong — it is a runtime
  fact, not a deployment decision.
- **worker** — `worker.replicas` (**default 2**). Multi-pod safety is built
  into the claiming path itself: leases with fencing tokens
  (`internal/store/postgres/claiming.go`'s §12.4 invariants), proven under
  real process-level concurrency and real kills by `tests/fault`. 2 is the
  supported default, not a ceiling — running more is the normal way to get
  throughput and availability.

## Probes, and the roles that deliberately have none

- **api** has real HTTP probes: `livenessProbe` on `GET /v1alpha1/healthz`
  (pure liveness, never touches the database) and `readinessProbe` on
  `GET /v1alpha1/readyz` (pings the database) — `internal/api/health.go`'s
  two documented endpoints.
- **scheduler** and **worker** have **no** liveness/readiness probe. Neither
  serves HTTP or any other port, and the base image (`gcr.io/distroless/
  static-debian12:nonroot`) has no shell to exec a command probe with
  either. A TCP probe against a port nothing listens on, or an
  always-succeeding exec probe, would claim a certainty this chart does not
  have — so there is no probe rather than a fake one. Instead:
  - the scheduler's own single-active advisory lock, and the worker's own
    lease + fencing-token claiming path, are what actually recover from a
    wedged or killed process — not a kubelet restart triggered by a probe;
  - `restartPolicy: Always` (the only value a Deployment's pod template
    accepts) still restarts the container if the process itself exits or
    crashes.

## Draining, not a preStop hook

There is no `preStop` hook anywhere in this chart — the distroless image has
no shell to exec one in. The drain story is entirely the binary's own
SIGTERM handling:

- **api** (`cmd/nodes/serve.go`): `signal.NotifyContext` + `http.Server.
  Shutdown`, bounded by its own 15s internal `shutdownTimeout`.
- **scheduler** / **worker** (`cmd/nodes/scheduler.go`, `cmd/nodes/
  worker.go`): `signal.NotifyContext` cancels the run loop's context; each
  returns once its current unit of work finishes.

`terminationGracePeriodSeconds` defaults to 30s — comfortably longer than
the api's own 15s ceiling, and enough room for the scheduler/worker to
finish one in-flight tick/claim before Kubernetes would ever escalate to
SIGKILL.

## The migration Job

`templates/migration-job.yaml` runs `nodes migrate`
(`docs/adr/0002-migration-policy.md`) as a `helm.sh/hook:
pre-install,pre-upgrade` Job, so every pending migration
(`migrations/*.sql`, embedded into the binary) applies before any
new-revision api/scheduler/worker pod can receive traffic or claim work.

Ordering matters here and is worth stating explicitly: Helm creates
pre-install/pre-upgrade hooks *before* any plain (non-hook) resource in the
release — so the migration Job's own database Secret, and (when
`postgresql.enabled`) the Postgres Secret/StatefulSet/Service, are
themselves hooks at an earlier weight (`-5`, vs. the migration Job's `0`).
Helm does not block-wait for a freshly-created StatefulSet's pod to become
Ready before moving to the next weight tier (only Job/Pod-kind hooks get
that treatment) — the migration Job's `backoffLimit: 6` and
`activeDeadlineSeconds: 240` are its own safety net for that remaining
window, since `nodes migrate` fails fast on an unreachable database rather
than retrying internally.

## Secret-ref env config

Every Deployment and the migration Job read secret-bearing configuration —
`NODES_DATABASE_URL`, and `NODES_CALLBACK_TOKEN_SECRET` when
`callback.enabled` — out of Kubernetes Secrets via
`env[].valueFrom.secretKeyRef`. No pod spec in this chart carries a database
URL, password, or callback secret in plaintext.

`NODES_DATABASE_URL` is always sourced from one Secret
(`<release>-culture-nodes-db`), whether it is built from the in-chart
Postgres (`postgresql.enabled: true`, the default) or passed straight
through from `postgresql.external.url` (`postgresql.enabled: false`, bring
your own — RDS, Cloud SQL, a shared cluster, ...).

## The actor callback route and the Ingress (c42, h37)

Asynchronous actors report back through `POST /v1/attempts/{id}/events`
(`internal/actors/protocol.go`'s `CallbackEventsPathFormat` — unversioned,
*not* under `/v1alpha1`, since it is a separate runner-agnostic wire
contract from the `nodes.culture.dev/v1alpha1` product API). It is
authenticated only by a short-lived, attempt-scoped bearer token
(`internal/actors/token.go`) — never by network identity, an ingress
annotation, or anything else — which is the Phase-1 trust boundary's whole
point: reachability is not authorization.

- The **api Service** is always the callback surface for an actor reachable
  from inside the cluster.
- The **Ingress** (`ingress.enabled`, default `false`) is what makes the
  same route reachable from *outside* the cluster. This chart only renders
  the `Ingress` resource — it does not install an ingress controller. A
  real end-to-end round trip needs one (`deploy/helm/ci-smoke.sh` installs
  `ingress-nginx` for the kind-cluster CI smoke).
- `callback.enabled` (default `true`) controls whether the route exists at
  all: `internal/api/server.go`'s `WithCallbackSigner` only mounts it when a
  signer is configured, so a `false` here means the route 404s rather than
  existing-but-always-failing. The same `NODES_CALLBACK_TOKEN_SECRET` reaches
  both api (verifies) and worker (mints) from one Secret
  (`<release>-culture-nodes-callback`).

## Namespace resolution: why worker takes a slug, not an id

A namespace id is a database-generated ULID
(`internal/store/postgres.Store.CreateNamespace`) — nothing outside the
database can predict it ahead of a Helm install. The worker Deployment sets
`NODES_NAMESPACE_SLUG=default` rather than a raw id; `cmd/nodes/worker.go`
resolves (and creates, if it is the first process to ask) that slug through
the identical idempotent lookup `cmd/nodes/serve.go` already uses for the
api Deployment's own namespace (`internal/api.EnsureNamespace` — its
concurrent-caller idempotency is exercised directly by
`internal/api/namespace_test.go`). Whichever Deployment's pod starts first
creates the row; every process after that — regardless of start order —
resolves the same id.

## Values reference (selected)

See `values.yaml` for the full, commented set. The load-bearing ones:

| Key | Default | Notes |
| --- | --- | --- |
| `image.repository` | `ghcr.io/agentculture/culture-nodes` | |
| `image.tag` | `""` (falls back to `.Chart.AppVersion`) | Ignored when `image.digest` is set |
| `image.digest` | `""` | **Wins over `image.tag` when set** — pin this in production |
| `api.replicas` | `1` | |
| `api.service.port` | `8080` | |
| `scheduler.replicas` | `1` | Single-active; extras are warm standbys |
| `worker.replicas` | `2` | Multi-pod safe by design; this is the default, not a ceiling |
| `worker.podDisruptionBudget.minAvailable` | `1` | |
| `callback.enabled` | `true` | `false` leaves the callback route unmounted (404) |
| `callback.tokenSecret` | `""` (generated) | Shared by api + worker |
| `postgresql.enabled` | `true` | Self-contained `postgres:17` StatefulSet+Service — **not** a Bitnami subchart dependency |
| `postgresql.external.url` | `""` | Required when `postgresql.enabled: false` |
| `ingress.enabled` | `false` | The callback surface for actors outside the cluster |

## Local install (kind)

```console
kind create cluster --name nodes-ci
docker build -t culture-nodes:kindtest .
kind load docker-image culture-nodes:kindtest --name nodes-ci
helm install nodes deploy/helm/culture-nodes \
  --set image.repository=culture-nodes --set image.tag=kindtest \
  --set worker.replicas=2 \
  --wait --timeout 5m

# Health/workflow/pod smoke checks (also run by .github/workflows/deploy.yml):
WORKER_REPLICAS=2 ./deploy/helm/ci-smoke.sh

kind delete cluster --name nodes-ci
```

`deploy/helm/ci-smoke.sh` is shared verbatim between a local run and
`.github/workflows/deploy.yml` — see that script's own doc comment for
exactly what it checks and its full env-var configuration.

To additionally exercise the h37 callback-through-Ingress round trip
(install a real ingress controller, enable the chart's Ingress, hit the
callback route from outside the api Service, and confirm a bogus
attempt-scoped token is refused with a structured 401):

```console
kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.14.0/deploy/static/provider/kind/deploy.yaml
kubectl wait --namespace ingress-nginx --for=condition=ready pod \
  --selector=app.kubernetes.io/component=controller --timeout=180s

helm upgrade nodes deploy/helm/culture-nodes \
  --set image.repository=culture-nodes --set image.tag=kindtest \
  --set worker.replicas=2 \
  --set ingress.enabled=true --set ingress.className=nginx --set ingress.host=nodes.kind.test \
  --wait --timeout 5m

WORKER_REPLICAS=2 SMOKE_INGRESS=1 INGRESS_HOST=nodes.kind.test \
  KIND_NODE=nodes-ci-control-plane ./deploy/helm/ci-smoke.sh
```

`ci-smoke.sh` resolves the kind node's container IP and ingress-nginx's
NodePort itself (`docker inspect` + `kubectl get svc`), so this needs no
`extraPortMappings` in the kind cluster config.

## Images

The control-plane binary (`cmd/nodes`) is published as a multi-arch
(linux/amd64, linux/arm64) OCI image to `ghcr.io/agentculture/culture-nodes`
by the `release.yml` workflow on every `v*` tag, alongside the existing PyPI
lane for the Python mesh-agent surface. Images are pushed **by digest** —
the `<tag>` and `latest` tags both point at that digest. Production values
for this chart should pin `image.digest` (`sha256:<digest>`), not a mutable
tag, so every role Deployment (api/scheduler/worker) and the migration Job
— sharing the one image — run the exact artifact `release.yml`'s smoke job
already verified.
