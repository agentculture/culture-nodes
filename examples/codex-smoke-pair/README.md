# codex-smoke-pair

Two-node smoke workflow proving both production codex actors —
`company/codex-thor` and `company/codex-orin` — can complete a real node
through culture-nodes' normal engine dispatch path, each producing its own
`proposed` ledger claim. This is the acceptance check for issue #14
(codex-bridges-thor-orin) task t8.

## What this proves

- `smoke.workflow.yaml` compiles offline (`nodes validate`, or the
  `tests/deploy/codexsmoke_test.go` Go test — see below).
- One read-only `codex exec` session completes on `codex-thor`, then one on
  `codex-orin` (the graph is sequential — see the workflow file's own header
  comment for why: this schema has no fan-out/parallel node kind yet).
- The engine's ledger records exactly two `proposed`, `authority: proposed`
  `claim` records, attributed to `company/codex-thor` and
  `company/codex-orin` respectively (`origin.actor_id`) — proof that each
  node was dispatched to the actor it was placed on, not a bypass endpoint.

Both nodes declare `sandbox: read-only` in their input (the bridge passes
this straight to `codex exec --sandbox`, see
`adapters/codex/README.md`'s "Invocation input fields") and an explicit
`policy.timeout: 15m`, so a smoke run can never mutate either agent checkout
and a stuck bridge cannot hang the run indefinitely.

## Billable warning

`run-smoke.sh` dispatches **two real, billable `codex exec` sessions** — one
on thor, one on orin. Codex has no offline mock engine (unlike
`adapters/colleague`'s `COLLEAGUE_ENGINE=mock`), so there is no way to
exercise this workflow for real without spending ChatGPT/API quota. This is
a **manual, live-only verification lane** — it is never invoked from any
`.github/workflows/*` file, and it must never be run unattended
(`run-smoke.sh` refuses to run unless `CONFIRM_BILLABLE=yes` is set).

## Prerequisites

- Both codex bridges deployed and running as `codex-bridge` systemd user
  units on thor and orin (`deploy/prod/deploy.sh`'s bridge lane).
- `company/codex-thor` and `company/codex-orin` registered as actors in the
  production `actors` table (`deploy/prod/register-actor.sh`), each with a
  numeric LAN IP `endpoint_ref` reachable from both worker containers.
- A git checkout at `~/git/culture-nodes-agent` on each host — `codex exec`
  refuses to run outside a git repo, and the bridge never passes
  `--skip-git-repo-check`.
- The culture-nodes API reachable at `http://thor:18080` (or wherever
  `NODES_API_URL` points).
- `curl` and `jq` on the machine running `run-smoke.sh`.

## How to run

```bash
CONFIRM_BILLABLE=yes ./run-smoke.sh
```

Override the target API or repo paths as needed:

```bash
CONFIRM_BILLABLE=yes \
  NODES_API_URL=http://thor:18080 \
  THOR_REPO=/home/thor/git/culture-nodes-agent \
  ORIN_REPO=/home/orin/git/culture-nodes-agent \
  ./run-smoke.sh
```

The script validates the workflow against the API, publishes it, creates a
run, polls `GET /v1alpha1/runs/{id}` until the run reaches a terminal state,
prints both nodes' outcomes, then fetches
`GET /v1alpha1/runs/{id}/ledger` and prints the two `proposed` `claim`
records with their actor attributions. It exits non-zero if the run does not
complete cleanly or fewer than two proposed claims land.

## Offline validation

`smoke.workflow.yaml` compiles clean with zero errors and zero warnings
through `internal/compiler.Compile` — no network, no codex binary required:

```bash
go run ./cmd/nodes validate examples/codex-smoke-pair/smoke.workflow.yaml
```

`tests/deploy/codexsmoke_test.go` (package `deploytest`) runs the same
compiler check as part of `go test ./...`, so a change that breaks the
workflow's shape is caught in CI without ever dispatching anything billable.
