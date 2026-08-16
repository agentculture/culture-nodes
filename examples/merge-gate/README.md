# merge-gate — the TDD gate as a node, not as a habit

The gate this repository merges on — build, vet, test, the adapter suites, the
web build, the linters — has always been a thing an operator types and a thing
an operator reads. It ran that way nine times in the #87 batch and six more
hand-turns in the #128/#137 cycle.

An operator reading a green tick is not evidence of anything. It leaves nothing
behind that names the suite, the exit code, or the commit; and a week later
nobody can tell a suite that passed from a suite that never ran.

This example is the gate as a **`code` node dispatched through the runner
boundary**. What it leaves behind is one `derived`, validator-origin ledger
record per declared gate, plus an aggregate the control plane computes over
them.

## What is new here, and what was already shipped

The mechanical half of issue #101 already existed and this builds directly on
it rather than beside it:

| Already shipped | Where |
| --- | --- |
| Recording a suite's command, exit code and commit as a `derived` validator record | `internal/api/suiteverdicts.go` |
| Running a suite in a detached worktree at the collected commit | `scripts/collect-handover.py --gate` |
| Routing a rejecting verdict to a bounded repair (2 attempts / 24h, both ceilings reaching a human) | `internal/api/repairroute.go` |

A gate that RAN is composed here through the very same
`handover.SuiteVerdict`, so its record is byte-compatible with the ones
`collect-handover.py --gate` writes today, and each rejecting one goes through
the very same `routeGateFailure`.

What was missing is the gate as a **node in a graph**, and the two record
shapes a node needs that a single suite verdict cannot express:

- **a gate that measured nothing.** `suite-verdicts` requires `exit_code` and
  never defaults it — precisely so a malformed request cannot become a pass —
  which leaves no way at all to say "this instrument never reached the changed
  tree". Posting `0` would be the false green the endpoint exists to prevent;
  posting `1` would manufacture a defect the repair router would then act on.
- **the aggregate.** Nobody merges on one gate; they merge on "did the gate
  pass", which is a computation over the per-gate findings.

## The three domain outcomes

```text
exit 0   gates_passed             every applicable gate measured and passed
exit 1   changes_required         an applicable gate missed its threshold
exit 2   measurement_incomplete   a gate that should have measured did not
```

Each is a **domain outcome** with its own edge. A threshold miss follows an
edge — it is never an engine failure and never a retry (PRD §3.4).

The third answer is the load-bearing one. A gate that can only say pass/fail
has to fold "I could not measure this" into one of the other two, and both
foldings are lies with consequences: folded into `gates_passed` it is the
empty-scan false green, and folded into `changes_required` it sends a repair
session after a defect nobody observed.

Those exit codes are one contract shared by four places, and none of them may
drift alone:

- `scripts/merge-gate.py`'s docstring (the program that exits with them);
- `internal/worker/code.go`'s gate vocabulary (the table that maps them back
  onto declared outcomes);
- `internal/handover/gate.go`'s `GateExitCode` (the outcome the ledger's
  aggregate computes);
- `workflow.yaml` in this directory (the outcomes and edges).

`TestGateExitCodesMatchTheLedgerVocabulary` holds the middle two together
mechanically; the other two are prose, and this list is the reminder.

## How the merge node consumes the verdict

```text
merge-gate.gates_passed            -> human-merges       (approval)
merge-gate.changes_required        -> changes-requested  (end)
merge-gate.measurement_incomplete  -> human-overrides    (approval)
```

**`human-merges` is reachable from exactly one place.** There is no edge into
it from `changes_required`, none from `measurement_incomplete`, and there is no
agent node anywhere in this graph — so "a package cannot reach the merge
decision without a derived gate record" is a property of the *edges*, checked
by the compiler when the workflow is published, rather than a discipline
somebody has to keep. That is the before-and-after difference: the old rule was
a habit, and a habit cannot be refused.

What the approval node binds is the gate node's output and the run's ledger
evidence. It binds no `proposed` claim, because this graph contains nothing
that could author one.

**On failure**, `changes_required` ends the run at `changes-requested`. The
graph dispatches nothing: the control plane already composed a bounded repair
routing beside the rejecting verdict (task t32 — two attempts per run over a
24-hour window, both ceilings reaching a human), and executing that routing
stays a deliberate step while the bridge write path is unproven (#18). Read it
with `scripts/collect-handover.py <run-id>`.

**On an incomplete measurement**, the token parks at `human-overrides`. A human
is the merge authority (§10.4), so a person may still merge over a gate that
could not measure everything it declared — but they do it at a *different node*
with a *different decision schema*, so the ledger can afterwards tell a merge
on a green gate from a merge over an unmeasured one. Collapsing the two into
one approval node would have thrown that distinction away, which is the same
information loss the green tick caused.

## Not applicable is never a pass

Per gate, the program decides applicability in this order — and the order is
the design:

1. `responsible_for` matched nothing in the changed set → `no_source_files`.
   A docs-only change is this for every code gate.
2. a required tool is missing on this host → `instrument_unavailable`, naming
   the files it owed a measurement on.
3. `reaches` matched nothing while `responsible_for` did →
   `instrument_not_reaching_tree`. Today's coverage and complexity instruments
   over `internal/`, the adapters and `web/` are exactly this (issue #88).
4. otherwise the gate runs, and its exit code is the finding.

Step 1 sits ahead of step 2 deliberately. Reversed, a docs-only change on a
host without Go would report `measurement_incomplete` for every Go gate — and
an instrument that was never owed a measurement is not a measurement that went
missing.

Every not-applicable record carries **no `verdict` key at all**. The review
vocabulary has three values (`confirm`, `reject`, `changes_requested`) and this
finding is none of them: no verdict was reached. The absence is the statement.

The aggregate reports `applicable`, `passed`, `failed` and `not_applicable`
counts (plus `declared`, so "two of ten gates measured anything" reads as a
sentence rather than an inference), and the control plane computes all of them from
the per-gate statuses. There is no field a caller could use to assert them.
That is what makes the empty-scan rule enforceable rather than aspirational: a
report in which nothing was applicable yields `measurement_incomplete`, and so
does one in which any instrument was unavailable on the host that ran it.

## Where this runs — and where it does not

The declared suites need **Go, Node/npm and uv together** on the host the
runner service runs on. Measured this cycle:

- one machine in this fleet has all three;
- a second has node, npm and uv, but `go` is off its PATH;
- a third has neither node nor npm at all, confirmed by a dispatched session
  reading its own PATH (run `01M05ZGNT86MAFDHATB6W5VYPN`).

`scripts/toolchain-baseline.sh` measures node and npm as of this cycle, so that
is a recorded baseline rather than a recollection.

So, plainly: **making the gate a node moves it out of the operator's session,
not off the operator's host.** It becomes a ledgered derived record instead of
a typed command and a read tick. It does not become something a second machine
can run today. Widening that is #96/#88 work, not this example's.

Nothing here relies on anyone remembering it. The program probes for each
declared instrument, and a lane that cannot run one reports
`instrument_unavailable` and reaches a person. There is no path through this
graph on which a missing toolchain produces a pass.

The pinned image says the same thing. `python:3.12-slim` carries neither Go nor
Node nor uv, so **as committed this node measures only what a plain Python 3.12
can measure and reports the rest as unavailable** — its outcome is
`measurement_incomplete`, and the run reaches `human-overrides`. That is the
honest default for a shipped example: it cannot produce a green it did not
earn. Re-pinning the image to a toolchain one is a *graph* edit, addressed by
the workflow digest, exactly like the thresholds, and for the same reason — the
tree under test must not choose the instruments that measure it.

## Deployment configuration

Loading this example into another deployment means supplying these. It never
means editing `workflow.yaml`.

| What | Where it resolves |
| --- | --- |
| `runner://headspace/tdd-gate` | the deployment's runner services file. A separate identity from a general-purpose runner precisely so it can be pointed at the one host whose toolchain satisfies the declared instruments. |
| `group/platform-maintainers` | the deployment's human inbox. |
| `MERGE_GATE_SOURCE_URL` / `MERGE_GATE_SOURCE_SHA256` | granted to the worker process. Where the gate program is fetched from, and the digest those bytes must have. |
| `NODES_API_URL` / `NODES_DECISION_TOKEN` | granted to the worker process. The control plane the records land in, and the same bearer secret the review surface uses — whoever can record a gate result can decide a merge. |
| `NODES_GATE_VALIDATOR_ACTOR_ID` | granted to the worker process. A registered, non-human validator identity; a derived record needs an identified deterministic producer (§10.4) and the control plane refuses a human one. |

There is **no run input**. The gate measures the workspace the runner gives it
and reads its own commit back out of that worktree.

Two things are deliberately *not* deployment configuration:

- **The gate matrix and its thresholds travel in `argv`**, so they are pinned
  by the published workflow's own content digest. `examples/development-loop`
  supplies its matrix as a granted environment value; that is weaker, because a
  granted value can change without republishing the workflow, and the t18
  design requires that "these values are part of the published workflow
  version, so a threshold cannot be selected after seeing the result". The
  difference is recorded here rather than left unexplained.
- **The run identity** (`NODES_RUN_ID`, `NODES_NODE_RUN_ID`,
  `NODES_ATTEMPT_ID`) is forwarded by the runner boundary from the operation's
  own context (`internal/runners.ContextEnvironment`). A container cannot
  choose which run it writes derived records against.

## Why the gate program is fetched rather than committed into the tree

The same reason `pr-upkeep`'s sweep fetches its script — a fixed origin baked
into a graph means every loader silently runs the demo author's bytes — plus
one more that matters here specifically: **the gate program must not come from
the tree it is measuring.** A change that could edit its own gate program is a
change that gates itself. The granted digest is what closes that.

## Validate and run

```bash
# compile locally until clean (0 errors, 0 warnings):
go run ./cmd/nodes validate examples/merge-gate/workflow.yaml

# every example, the same way CI does it:
scripts/validate-examples.sh

# the gate program's own applicability logic, against a real git worktree,
# recording nothing:
uv run pytest tests/test_merge_gate.py -q

# the same matrix this workflow pins, against a real worktree, recording
# nothing (`@file` reads a matrix from disk — it exists for authoring and for
# the tests, never for the dispatched operation, which carries the matrix in
# its pinned argv):
scripts/merge-gate.py --gates @/tmp/matrix.json --repo . --base origin/main --report-only
```

`--report-only` prints the report and **always exits 2**, even when every gate
passed. A gate whose finding is not in the ledger gated nothing, so the mode
that records nothing cannot produce the passing exit code.

## Known limits, stated rather than discovered

- **The node's timeout is capped at 900 seconds.** `internal/compiler`'s
  `RunnerMaxTimeoutSeconds` applies one cap to every code node, sourced from
  the Lambda runner's limit, and the compiler refuses a longer one. The full
  culture-nodes suite does not reliably fit in fifteen minutes, so a real
  deployment either splits the matrix across several gate nodes or raises the
  cap for a runner that has no such limit. This is a constraint the t18 design
  did not account for.
- **The gate node cannot gate its own delivery.** This cycle's merges were
  verified by the operator-typed gate that this example exists to replace, so
  the node's first real verdict is on work merged *after* it. Claiming
  otherwise would be exactly the false green issue #101 names.
- **Coverage and cognitive complexity reach only `culture_nodes`.** A change
  under `internal/`, the adapters or `web/` is tests-plus-lint only, with those
  two reported as `instrument_not_reaching_tree` naming the uncovered files.
  Widening them is issue #88.
- **Nothing is dispatched on a red gate.** The routing is a decision the
  control plane records; acting on it stays a deliberate step (#18, #119).
