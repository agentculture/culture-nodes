# Live audit A — wave A of web-ui-lift on the production pair (2026-09-03)

Plan task t18. Deployed revision: `c89cee4bd2cc` (thor control plane, thor and orin
workers; `deploy/prod/deploy.sh` twice per host — see the hand-turns below).
Namespace `01KZJTJZS8Q03AQQPVFR7QNW90`. Evidence below is from the LAN API
(`http://192.168.1.146:18080`, reads only) and the operator's browser on
`https://nodes.culture.dev`; anything not observed is marked so.

## API-side checks (agent, reads only)

| Check | Spec | Result |
| --- | --- | --- |
| Tail-only connect: first frame is the snapshot marker, history not replayed | c2, h1, h25, c30 | **pass** — `GET /v1alpha1/events?from=latest` answered: `id: 01M1MDE2BTNEBSM05R9ED3PWRX event: stream.snapshot data: {"snapshot_id":"01M1MDE2BTNEBSM05R9ED3PWRX"}` |
| Worker presence rows exist, names only | c19, h19, c38 | **pass with a finding** — `orin-worker-1` hostname=`2b8c7e0c574d` revision=`c89cee4` actor_keys=6 (names only); `thor-worker-1` hostname=`53ceb94f0a51` revision=`c89cee4` actor_keys=6 (names only). Finding: `hostname` is each worker's container id, not the host (fix package in flight). |
| Mesh endpoint answers without probing in-request | c40, h32 | **pass (latency)** — `GET /v1alpha1/mesh` returns in well under a second. |
| Machines keyed on self-reported hostname | c23, h26 | **not observable on this revision** — `machines` is empty and every actor's bridge block reads "not observed by the bridge collector": `cmd/nodes/serve.go` wires the collector with no targets. Fix package in flight (`fix-mesh-collector`). |
| Failed probe renders unknown, never healthy | c34, h24 | **not observable on this revision** — no probe runs (above). |
| Version stamp readable | operating rule | **pass** — `GET /v1alpha1/version` names `c89cee4bd2cc`, clean. |

## Browser checks (operator, Access session) — to be filled in

| Check on the demo's bar | Spec | Result | Screenshot |
| --- | --- | --- | --- |
| Mesh: events since load stays 0 on a fresh load; 1 after one real committed event | c30 | _(operator)_ | _(path)_ |
| Design: a workflow with zero runs draws its graph | c31 | _(operator)_ | _(path)_ |
| One node draws Mesh, a run, the gallery | c32 | _(operator)_ | _(path)_ |
| Mesh edges from real rows; spark/thor/orin as machines with revision and install mode | c34 | _(operator — expected NOT to pass on this revision, see above)_ | _(path)_ |
| The two workers' key sets: same six names on thor and orin (no #224-style divergence today) | h17 | _(operator)_ | _(path)_ |
| No layout jump; toggles are one control | c26 | _(operator)_ | _(path)_ |

## Divergences from the acceptance reference (docs/demos/web-ui-lift/)

1. Node treatment: built nodes are flat 224×128 cards; the demo's compact card with glowing core, breathing halo and bezier edges is the reference. Fix package `fix-node-style` in flight on the developer lane.
2. Mesh machines row: absent live (collector targets), present in the demo.
3. Worker hostname: container id live; the demo names hosts.

## Hand-turns counted

- `deploy.sh thor` ran twice: the first exited 0 after the account provision refused a diverged actor checkout and shipped nothing (issue #289); the checkouts were moved to `web-ui-lift` tracking `origin/feat/web-ui-lift` as `culture-codex` on both hosts, then the second run landed.
- `deploy.sh orin` ran twice: the first shipped a later docs commit and failed the pair's revision parity (scheduler paused); the second ran from a worktree pinned at the thor revision and resumed the sweep.
- A live deploy is a deploy of the **pair**: the parity rule pauses the scheduler until both workers match the API.

## Verdict

The operator fills this in after the browser rows: does wave A on the pair read as the demo promised, and what goes back to the fixes cycle.
