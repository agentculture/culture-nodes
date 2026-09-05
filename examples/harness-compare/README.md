# harness-compare

Fan **one** instruction to several harnesses and join their answers into one
result. Plan task t5 of `docs/plans/2026-09-04-harness-interop-pi-qwen.md`
(spec claim c31, honesty condition h26; issue #294).

Running a comparison is **optional**. This is a flow an operator can run when
they want to see how two or more harnesses answer the same request — it is
not a property every dispatch has, and nothing about registering a pi or qwen
actor obliges anyone to run it.

## What one run does

```text
                 ┌── claude     ──┐
                 ├── codex      ──┤
  fan (parallel) ├── qwen       ──┤ gather (join, all) ── finish (end)
                 ├── pi         ──┤
                 └── colleague  ──┘
```

1. `fan` is a `parallel` node. Its `split` outcome has one edge per harness
   slot, each guarded by `has(input.actors.<slot>)`: a slot the run input
   names fans out, a slot it leaves out is skipped before the actor registry
   is consulted.
2. Every named slot is an `agent` node that receives the **same**
   `instruction`, `sandbox` and `handover` from the run input, and its own
   `repo` from `input.actors.<slot>.repo` (a path is meaningful on exactly one
   host — issue #74 — so each actor names its own checkout). The `qwen` slot
   additionally receives `input.actors.qwen.mode`, the ACP session mode its
   bridge requires. The `colleague` slot takes only `repo`, the same shape as
   `pi`.
3. `gather` is a `join` with `policy: all` over the *realized* cardinality:
   it waits for every slot that fanned out, and only those.
4. `finish` emits `/nodes/gather/output` — the join's arrival array — as the
   run's result.

Every slot declares three outcomes and routes all of them into the join:
`completed`, `no_changes` (a write dispatch whose measured change set is
empty), and `permission_blocked` (a session that stopped at a permission
prompt). An actor that did not complete still *arrives* and is compared under
its own outcome name instead of stranding the barrier.

## What the joined result carries, per actor

The run's output is `internal/worker/paralleljoin.go`'s arrival array:

```json
{
  "policy": "all",
  "cardinality": 2,
  "arrivals": [
    {
      "from_node": "codex",
      "outcome": "completed",
      "output": {
        "summary": "…the actor's own summary…",
        "changed_files": ["…model-claimed…"],
        "workspace_measured": { "measured": true, "changed_files": ["…bridge-measured…"], "…": "…" }
      },
      "token_id": "…", "arrived_at": "…"
    },
    { "from_node": "pi", "…": "…" }
  ]
}
```

| Per-actor fact | Where it is | Why there |
|---|---|---|
| **outcome** | `arrivals[].outcome` | the slot's domain outcome, as the join recorded it |
| **changed_files** | `arrivals[].output.workspace_measured.changed_files` | the bridge measures the workspace with real `git` calls around the session and the worker merges that block into the node output (`internal/worker/dispatch.go`); `arrivals[].output.changed_files` beside it is the model's own claim — read the measured one |
| **usage.model** | the slot's attempt: `attempts[].usage.usage_model` in the run view (`run <id>`) | usage is attempt telemetry (ADR 0009/0017), not node output; no binding surface can move it into the join |
| **handover ref** | the run's ledger (`ledger <id>`), as an `observed` evidence record | `internal/handover` fetches the ref the bridge names from a remote the *control plane* is configured with and records what it measured; the bridge's own handover block is never promoted, and it has no binding surface either. If the control plane has no handover remote configured, or the ref is unfetchable, there is **no** record — absence is the honest answer |

So the joined result carries outcome and the measured change set directly,
and the run beside it carries model and handover ref. A comparison table is
those two reads side by side; nothing in this workflow claims more than the
run measured.

## The limitation, stated plainly

**The workflow language cannot fan out over a run-time list.** A node's
`uses:` is a static registry id, and there is no node kind that creates one
token per element of an input array. What this example does instead:

- the fan-out is over a **fixed set of five actor slots** — `claude`, `codex`,
  `qwen`, `pi`, `colleague` — one per harness this deployment registers;
- the run input's `actors` **map** says which slots run. Its keys are the
  slots; a missing key is an unset slot. Guards on the split edges
  (`has(input.actors.<slot>)`) are the selection mechanism, and the join
  counts only the branches that actually fanned out;
- the `actors` value is a map rather than a list because a binding is an RFC
  6901 pointer, so a slot can only reach *its own* entry through a fixed key
  (`/run/input/actors/pi/repo`), never by searching a list for itself;
- at least one slot must be named — a split that selects no edge is a routing
  failure, not an empty comparison;
- comparing a sixth harness means adding a sixth slot to `workflow.yaml`, not
  a sixth element to the input.

Nothing here invents a language feature; if a run-time fan-out lands later,
this example is the one to rewrite against it.

## Each harness runs in its own shape

Spec decision q4 (2026-09-04): fairness means adjusting each harness to its
own best shape, not stripping them to a common denominator.

- **pi is a bare harness by design** — `pi --mode json -p <instruction>` with
  no vendored skills or project context beyond the checkout.
- **qwen and colleague carry their vendored skills** and context; the qwen
  bridge is ACP-backed and needs a session `mode`, which is the one
  per-harness input this graph carries.
- **codex** and **claude** run as their bridges run them (`adapters/codex`,
  `adapters/claude-code`).

This workflow levels nothing: the same words go to every slot, and each
bridge runs its harness the way that harness is meant to run. The run records
which harness served each slot (the actor row's `harness=` metadata, once
registered with it, and the attempt's reported model), so the comparison is
read with that in mind rather than pretending the harnesses were equivalent.

## Deployment configuration

`workflow.yaml`'s header block names every value that resolves outside the
file. In short:

| Slot | Registry id in `uses:` | Bridge |
|---|---|---|
| `claude` | `actor://company/developer` | `adapters/claude-code` |
| `codex` | `actor://company/codex-thor` | `adapters/codex` |
| `qwen` | `actor://company/qwen-thor` | `adapters/qwen` |
| `pi` | `actor://company/pi-thor` | `adapters/pi` |
| `colleague` | `actor://company/colleague-spark` | the colleague bridge |

These are registry **keys** a deployment registers against its own bridge
(`deploy/prod/register-actor.sh`), not hostnames the graph requires. Register
only the slots you intend to name; a slot you never name is never resolved.

The `colleague` slot's actor, `company/colleague-spark`, is registered by
task t12 once the `culture-colleague` account exists on spark — it is not
registered yet. Until then, leave the `colleague` key out of the run input:
an unregistered actor id would fail dispatch, not the graph, so the slot's
guard is what keeps it from ever being consulted.

Run input (`input.json` is a complete example):

| Field | Meaning |
|---|---|
| `instruction` | the one request every named actor receives |
| `sandbox` | `read-only` or `workspace-write`; a write comparison needs `workspace-write` |
| `handover` | ask each bridge to create a handover ref for what it changed (needs `workspace-write`) |
| `actors.<slot>.repo` | that actor's checkout on its own host |
| `actors.qwen.mode` | the ACP mode for the qwen slot (required when `qwen` is named) |
| `measurement` | optional; tags the run with the measurement manifest (task t7) that drove it — `manifest_digest` and `rule_id` — so per-actor stats can be read per manifest version. Omitted for a run that is not part of a measurement pass. |

## How an operator runs it

Every dispatch this creates is a **real, billable agent session**, one per
named slot. Confirm intent the way the `nodes-operator` skill's guard does.

With the `nodes-operator` skill (`.claude/skills/nodes-operator/SKILL.md`):

```bash
op=.claude/skills/nodes-operator/scripts/nodes-op.sh
bash $op validate examples/harness-compare/workflow.yaml      # server-side compile check
digest=$(bash $op publish examples/harness-compare/workflow.yaml)
bash $op create "$digest" examples/harness-compare/input.json --category compare --yes
bash $op watch <run-id>                                       # outcomes + ledger at terminal
bash $op run <run-id>                                         # per-slot attempts: usage_model
bash $op ledger <run-id>                                      # proposed claims; observed handover evidence, if any
```

With the `nodes` CLI (`uv run nodes …`, against `NODES_API_URL`):

```bash
uv run nodes workflow validate examples/harness-compare/workflow.yaml
uv run nodes workflow publish examples/harness-compare/workflow.yaml   # prints the digest
uv run nodes run create --workflow <digest> --input examples/harness-compare/input.json --category compare
uv run nodes run get <run-id>           # run, tokens, node runs, attempts (usage_model per slot)
```

Offline, with no control plane, the file compiles through the same path
`nodes validate` uses:

```bash
go run ./cmd/nodes validate examples/harness-compare/workflow.yaml
```

Grade each slot's run afterwards (`nodes-op.sh grade <run-id> --actor …`) —
CLAUDE.md's assessment half applies to every actor a comparison dispatches.

## What proves it

`tests/e2e/harnesscompare_test.go`:

- `TestHarnessCompareWorkflowCompilesCleanlyAndDeterministically` — no
  database: the example compiles to the same digest twice and has exactly
  this shape (parallel entry, five guarded agent slots on the documented
  registry ids, one `all` join, one end node emitting the join's output).
- `TestHarnessCompareFansOneInstructionToTwoActorsAndJoins` — real
  PostgreSQL, real API/engine/worker, **two fake actors** behind the `codex`
  and `pi` slots; the run input names only those two. It checks that both
  received the same instruction in their own checkouts, that the run ends with
  one joined result carrying both outcomes and both bridge-measured change
  sets kept apart per slot, that the unset `claude`/`qwen`/`colleague` slots
  never ran, that each slot's attempt reports its own model, and that each
  actor's claim stays `proposed`.

`tests/lint/examplescompile_test.go` and `tests/lint/exampleportability_test.go`
compile and portability-check this example with every other one.

## What it does not prove

- No live dispatch to a registered pi or qwen actor happened here; that is
  task t10's proof, on the deployed fleet.
- The handover ref path is not exercised: the e2e stack configures no
  handover remote, so the fake actors' handover blocks produce no ledger
  record — exactly what a production run without a remote produces.
- Whether a given bridge ever reports `no_changes` or `permission_blocked` is
  that bridge's own contract; the edges here route them if it does.
