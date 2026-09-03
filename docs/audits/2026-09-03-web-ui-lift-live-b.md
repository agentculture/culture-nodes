# Live audit B — the full lift on the production pair (2026-09-04, ~00:30 IDT)

Plan task t20's closing audit. Deployed revision `91cac26` on both hosts
(`deploy/prod/deploy.sh thor` then `orin`, from a worktree pinned at that
commit; parity holds, the sweep schedule resumed, `prod-scheduler-1` up). The
branch head is one commit ahead (b8d7c8a: canvas page grid and pane typography,
CSS only; then f0ac4a2: version 0.48.0). Reads are from the LAN API
(`http://192.168.1.146:18080`); browser rows are the operator's.

## API-side checks (agent, reads only)

| Check | Spec | Result |
| --- | --- | --- |
| Tail-only connect: first frame is the snapshot marker | c2, c30 | **pass** — `id: 01M1MJSY5C19JZNJAH7XBKRDMV` / `event: stream.snapshot` |
| Machines keyed on the bridge's self-reported hostname | c23, h26 | **pass** — `thor` (codex-thor, notify-discord), `orin` (codex-orin), `spark-f8a9` (developer, intake, planner, qwen-developer, verifier) |
| Worker presence with real hostnames, names only | c19, h19, c38 | **pass** — `thor-worker-1` → `thor`, `orin-worker-1` → `orin`, both at `91cac26` |
| Probe classes | c40, h32 (delta b7) | **pass** — `unsupported`: `company/human-ops` (`GET capabilities: 404 Not Found`), `company/jira-comment` (`capabilities has no preflight.host.hostname`); `unobserved`: `company/operator-claude` (no endpoint), the human and engine-internal actors; no `failed` class at read time |
| Jira bridge reports its hostname | delta b7 | **not yet live** — the adapter fix is merged, but the jira bridge on thor is a `uv tool` copy that `deploy.sh` only redeploys when `JIRA_SITE` and the Jira trio are exported (see `docs/operations/jira-service-account.md`); that redeploy is an operator hand-turn still owed |
| Version stamp | operating rule | **pass** — `91cac26`, clean |

## Bridge health, observed while auditing

`GET /healthz` on all three bridges — codex-thor (`:8086`), codex-orin
(`:8086`), developer on spark (`:8088`) — gives no answer within 20–30 s, from
spark **and from thor's own localhost**, while the mesh collector's
`GET /v1/capabilities` to the same processes keeps returning `200` on its
30-second cadence and every dispatch since the 21:27Z redeploy would time out
"awaiting headers". This is issue #290, now with a fourth data point: all
three bridges at once, right after a control-plane redeploy. The audit records
it; the fix is not part of this cycle.

## Fixes cycle, what closed and what did not

Closed on this revision: the mesh's machines and probe classes (deltas b1, b2,
b7), the node treatment (b3), the fixture stream cursor (b4), the honest edge
count (b5). Closed on the branch after this revision: the canvas placement,
drop point, persisted positions, inserted-line highlight and status line (b6),
the canvas page layout. Not closed: the jira bridge hostname live (above), and
the bridge liveness defect (#290).

## Browser checks (operator, Access session) — to be filled in

| Check on the demo's bar | Spec | Result | Screenshot |
| --- | --- | --- | --- |
| Mesh: events since load stays 0 on a fresh load; 1 after one real committed event | c30 | _(operator)_ | _(path)_ |
| Design: a workflow with zero runs draws its graph; Open in canvas loads its stored source | c31, c36 | _(operator)_ | _(path)_ |
| One node draws Mesh, a run, the gallery and the canvas | c32, c22 | _(operator)_ | _(path)_ |
| Mesh: spark, thor and orin as machines with revision and install mode; unsupported and unobserved actors worded as such | c34 | _(operator)_ | _(path)_ |
| Canvas: add, connect, edit, validate, publish; comment-only republish says "no semantic change" | c33, c28, c42 | _(operator)_ | _(path)_ |
| Fluid: no layout jump; one toggle everywhere | c26 | _(operator)_ | _(path)_ |

## Hand-turns counted in this audit

Two deploys (thor, orin) from a pinned worktree; three bridge restarts earlier
in the evening (#290); the canvas polish taken into the operator lane after
three dispatches were lost to wedged bridges.

## Verdict

The operator fills this in after the browser rows.
