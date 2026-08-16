# Agent-host toolchain baseline (issue #96, task t19)

Captured 2026-08-15 on spark. Machine-readable form:
`docs/baselines/toolchains/{thor,orin,spark}.json`, written and re-checked by
`scripts/toolchain-baseline.sh`.

## What this baseline is for

The capability surface each bridge advertises now reports **toolchains** and
**dispatch grants** (`adapters/*/src/*/preflight.py`,
`api/actor-protocol/README.md`). Half of that is measured live by the bridge
every time an operator reads `/v1/capabilities`. The other half — what a
given dispatch posture actually grants — comes from four dispatched probe
runs, and those findings are pinned to a moment: this uv, this codex-cli,
this kernel.

This file records the moment. `scripts/toolchain-baseline.sh check` is what
notices it has passed.

## The four probe runs this surface is built on

| Run | Host | Finding |
| --- | --- | --- |
| `01M03374VAKH0KHN0GDZ466NP4` | thor | uv is a **snap**, and snap-confine cannot start inside codex's sandbox: *"required permitted capability `cap_dac_override` not found"*. The suite never started. |
| `01M0342X60F3NY8MH150G48AZ6` | orin | uv is a **standalone binary**, gets past that, and dies initialising its cache: *"Could not create temporary file … Read-only file system (os error 30) at path /home/orin/.cache/uv"*. |
| `01M0356BK8QYR3119R8VY1YY9Q` | orin | Under `--sandbox read-only` **nothing** is writable — not `/tmp`, not the working directory — so `UV_CACHE_DIR` has nowhere to point. Not the snap, not the binary, not the cache path: the sandbox mode itself. |
| `01M039NZ2TZYFG68YZT93A6DC7` | thor | A dispatched codex session has **no network egress**: neither api.github.com nor pypi.org was reachable, while `gh auth status` over a plain ssh session on that same host reports logged in. |

The first three are the plan's stated baseline. The fourth is this cycle's
addition and the sharpest of them, because it is the case where the true
statement about the host is a false statement about the dispatch.

A fifth run, `01M03CXXZ9N34FP4CYNA0BBS43`, was dispatched an hour after this
surface was built, and is the reason thor's `go` row above is not `absent`.
Deviation d9 had established that the codex lane was exhausted by CAPABILITY
rather than by budget — every remaining plan task needed Go, docker or
network — so Go 1.26.6 was installed on thor under `~/.local/go-dist` with
its module cache pre-warmed from OUTSIDE the sandbox, and that probe measured
what a dispatch can then do with it: **build yes, vet yes, tests needing no
database yes, tests needing PostgreSQL no.**

Two things about that probe are worth keeping, because both are exactly what
this surface exists to express:

- It reported `go` as usable, and the surface reports it as
  `present-off-path`. Both are right, and they are the same fact seen from
  two sides: the probe invoked `~/.local/bin/go` by absolute path and
  succeeded, and a session typing `go build` would have failed. The surface's
  job is to say which of those a dispatch gets.
- The probe's fourth answer was WRONG, and not through carelessness.
  `go test ./internal/queue/postgres/` exited 0 printing `ok`, so it reported
  that database-backed tests run. They had all skipped:
  `pgtest.RequireStore` calls `t.Skip` when no Postgres is reachable, and
  `go test` prints `ok` for a package where every test skipped. The runtime
  is the only tell — 0.030s against 2.678s on spark. A capability surface
  that reported "go: can run tests" would have inherited that error; one that
  reports what a mode GRANTS does not.

## What the hosts measure, today

Measured by piping `adapters/codex/src/codex_bridge/preflight.py` over ssh
into `python3 -`, under the systemd user manager's PATH (the one a bridge
process, and therefore a dispatched session, actually gets) — not a login
shell's. Command: `scripts/toolchain-baseline.sh capture`.

| Tool | thor | orin | spark |
| --- | --- | --- | --- |
| `uv` | `/snap/bin/uv`, **snap**, 0.12.3 | `~/.local/bin/uv`, **standalone**, 0.11.29 | `~/.local/bin/uv`, standalone, 0.9.28 |
| `go` | **present-off-path**, `~/.local/bin/go`, go1.26.6 | **absent** | `~/.local/go/bin/go`, go1.26.5 |
| `gh` | `/usr/bin/gh`, system, 2.45.0 | `~/.local/bin/gh`, standalone, 2.96.0 | `/usr/bin/gh`, system, 2.45.0 |
| `git` | system, 2.43.0 | system, 2.43.0 | system, 2.43.0 |
| `codex` | `~/.local/bin/codex`, **off PATH**, codex-cli 0.147.0 | `~/.local/bin/codex`, codex-cli 0.147.0 | absent |
| `claude` | 2.1.87 | 2.1.221 | 2.1.233 |
| `colleague` | **off PATH**, 1.45.1 | 1.49.0 | 1.56.0 |

Three things in that table are load-bearing and none of them is visible from
a `present`/`absent` answer:

- **thor's uv is a snap and orin's is not.** That single difference is why
  the same request failed differently on the two hosts, and it is now
  derivable from the surface rather than from a post-mortem.
- **Go is absent on both agent hosts.** The Go control plane is therefore
  testable in exactly one place that is not an operator's laptop, which is
  what #115 (plan t1) is about.
- **`present-off-path` is a real state.** thor's systemd user PATH does not
  carry `~/.local/bin`, so a dispatched session there cannot invoke `codex`
  or `colleague` by name even though both are installed. orin's does. A
  measurement taken over an ssh login shell would have got this backwards for
  orin's uv — which is exactly why the script sets the PATH it does.

## Re-checking

```bash
scripts/toolchain-baseline.sh check            # thor, orin, spark
scripts/toolchain-baseline.sh check thor       # one host
```

It exits non-zero on any difference: a uv upgrade, a snap replaced by a
standalone binary, a **codex-cli bump**, a tool appearing or vanishing. The
`search_path` field is excluded from the comparison and reported separately
— it legitimately differs between two honest measurements, and a check that
cried wolf about it would stop being run.

**A red check is not fixed by re-capturing.** The three posture findings
above were measured against the old state; re-run those probes, then
capture. Editing the baseline to match is how a surface starts reporting
what someone believes instead of what a dispatch measured.

## Rendering a work-package brief

`scripts/render-work-package.py` composes the dispatch brief directly from
`.devague/plans/<slug>.json` and the target actor's registered capability
document (or the same document read live from its bridge). For example:

```bash
python3 scripts/render-work-package.py close-the-backlog t31 company/codex-thor \
  --capabilities /tmp/codex-thor-actor.json \
  --sandbox workspace-write \
  --repo /home/thor/git/culture-nodes-agent \
  --branch ctb/t31 --base 442393f
```

The capability input is deliberately the full actor or bridge document, not
one of `docs/baselines/toolchains/*.json`: those baselines record live
toolchain identity but deliberately do not duplicate the bridge's dispatch
grant map. The renderer refuses a sandbox the actor did not advertise.

Checkout preparation is also a capability-derived statement. A mode that
advertises `network-egress` can pull once the source remote and revision are
bound as dispatch inputs. A mode without it cannot truthfully be told to
pull: its output says that the checkout must already be seeded and that the
operator-side predecessor remains manual. The current codex posture is the
latter, so t31's second criterion cannot be met by brief composition alone.

## What is still not measured

The posture map itself (`_MODE_GRANTS` in each bridge's `capabilities.py`)
is a **declaration grounded in the runs above**, not a live measurement. The
bridge cannot measure "does workspace-write grant egress" without spending a
dispatch to find out, and doing that on every capability read would bill a
session per query. So the honest reading of the surface is:

- `state`, `path`, `on_path`, `packaging`, `version` — **measured**, live,
  by the bridge process on the host.
- `usable_in` / `unusable_in` — **derived** from those measurements and the
  posture map, whose grounding is the four run ids above and whose freshness
  is this baseline's job.
