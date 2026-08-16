# Invariant gates

Invariants 1 and 2 below are batch-wide promises enforced by one source
sweep; [invariant 3](#invariant-3--every-committed-example-compiles-73) and
[invariant 4](#invariant-4--every-committed-example-is-portable-t16) are
later, differently-shaped gates over the `examples/` tree. All of them run
under the ordinary `go test ./...`.

Two batch-wide promises from the attempts-evidence-humans-loops spec
(`docs/specs/2026-08-13-attempts-evidence-humans-loops.md`) are gated
mechanically by `internal/invariants/invariants_test.go` — a pure source
sweep (no database, no network) that runs with the ordinary
`go test ./...`, so CI enforces both on every change. The delivery summary
for the batch cites this gate as the c16/h14 and c17/h15 proof.

The gate was built by task t20 and verified against the tree at the batch
base (commit `b975a60`, the plan landing) plus the merged wave-0/1 tasks.
The sweep found **no violations** in that tree: every mention of a gated
token sat exactly where the allowlists below say it belongs.

## Invariant 1 — Provider neutrality (spec c16, honesty h14)

The core engine never branches on provider names or actor kind. Human
actors ride the same 202+callback dispatch path as agents; kind-aware code
lives outside dispatch, following the grades-API precedent.

Enforced by three layers:

- `internal/actors/neutrality_test.go` (pre-existing): greps
  `internal/{actors,worker,engine,compiler,api}` for provider names and
  `go.mod` for vendor agent SDKs. **h14 requires this file unmodified and
  green through the batch** — the gate pins its SHA-256
  (`3b335704e0c9…af6da07a`, byte-identical to the batch base) and asserts
  both of its test functions still exist, so `go test ./internal/actors/`
  still runs them.
- `TestActorKindReadsStayOutOfDispatch` (new, t20): sweeps the same five
  trees for reads of a `Kind` field off an actor-shaped identifier
  (`actor.Kind`, `grader.Kind`, …). Node kinds, event kinds, and ledger
  origin kinds do not match and are legitimate.
- Runtime: dispatch resolution (`internal/worker/registry.go`) reads
  `endpoint_ref` and metadata only, never kind.

Sanctioned kind-aware file (the only allowed match):

| File | Why it may branch on actor kind |
| --- | --- |
| `internal/api/grades.go` | Grade authority follows the grader's registered kind: a human grading directly is their own confirmation; an agent grading is a proposal. Outside dispatch — the c16 precedent. |
| `internal/api/preflights.go` | A clarify-then-commit acknowledgement's ledger **origin** follows the acknowledging actor's registered kind — `agent` when a bridge answers for itself, `human` when an operator answers on its behalf (issue #67, task t14). The authority is `proposed` either way, so the kind decides who the record says produced it, never what the control plane does with it; the gate's dispatch-side half (`internal/worker/clarifygate.go`) reads no kind at all. |

## Invariant 2 — The ledger authority ladder (spec c17, honesty h15)

Agents propose; runner-boundary code observes facts it directly measured;
deterministic validators derive; humans confirm — only through the
ledger's review transaction, plus the one recorded human-grade precedent.
No actor promotes its own proposal (PRD §10.4).

`TestAuthorityLadderWritersAreAllowlisted` sweeps every non-test `.go`
file in the repository and requires each mention of a gated token to be on
that token's allowlist — and each allowlisted file to still mention it
(stale entries fail too, so the lists cannot rot). Mention-level
granularity is deliberate: a new reader is cheap to allowlist in review; a
new writer hiding as a reader is what a looser sweep would miss.

### `AuthorityObserved` — direct measurement only

The standing test is not which package a writer lives in: it is whether
every field the record stamps came from the writer's **own measurement**
rather than from something an actor reported. `internal/runners` qualifies
because a runner watched the process it reports on. `internal/handover`
qualifies because the control plane fetched the ref itself — the agent's
report supplies only the ref *name* to look for, and a ref that cannot be
fetched produces no record at all rather than one marked unmeasured.

| File | Standing |
| --- | --- |
| `internal/ledger/record.go` | Vocabulary: defines the constants |
| `internal/ledger/authority.go` | Append-time enforcement: observed needs a runner manifest |
| `internal/engine/ledgerdelta.go` | Refusal gate: node deltas may propose/observe per declared contract only |
| `internal/runners/dispatch.go` | **The writer**: boundary-measured evidence, `OriginRunner` + observed |
| `internal/handover/handover.go` | **Second writer** (task t10, issue #13): what a `git fetch` of a handed-over ref measured — ref, commit sha, changed paths. Reads nothing the agent reported except the ref name (`actors.Handover.ClaimedRef` is the only accessor); refuses to write without an identified measuring actor |

### `OriginRunner` — stamped only by a boundary reporting its own measurement

A writer here must also appear on the `AuthorityObserved` list above: the
two travel together, and a file stamping one without the other is claiming
an identity it is not using or an authority it has not earned.

| File | Standing |
| --- | --- |
| `internal/ledger/record.go` | Vocabulary |
| `internal/ledger/authority.go` | Enforcement: runners write manifest-checked observed evidence only |
| `internal/runners/dispatch.go` | **The writer** |
| `internal/handover/handover.go` | **Second writer**: the git-fetch observer, under its own configured actor id |

### `AuthorityConfirmed` — human acceptance only

| File | Standing |
| --- | --- |
| `internal/ledger/record.go` | Vocabulary |
| `internal/ledger/authority.go` | Enforcement: confirmed/rejected only inside review transactions (plus the human-grade carve-out) |
| `internal/ledger/ledger.go` | **The writer**: `Verdict.authority` maps `CommitReview` verdicts |
| `internal/ledger/projection.go` | Reader: folds confirmed decisions into live state |
| `internal/api/grades.go` | Sanctioned writer: a human grading directly lands confirmed |
| `internal/devague/claims.go` | Pre-batch import mapper: mirrors decisions humans already recorded in devague frames |
| `internal/devague/deviations.go` | Pre-batch import mapper (task t22): mirrors devague deviation approvals into review records, the same split claims.go uses for claims |

### `AuthorityDerived` — deterministic producers only

Each writer must also stamp its deterministic origin (checked by the
sweep; refused at append time by `internal/ledger/authority.go` otherwise).

| File | Standing |
| --- | --- |
| `internal/ledger/record.go` | Vocabulary |
| `internal/ledger/authority.go` | Enforcement: engine/validator origins write derived and only derived |
| `internal/worker/acceptance.go` | Validator-origin writer: acceptance evaluation (issue 37) |
| `internal/worker/successsignal.go` | Validator-origin writer: mechanical `success_signal` evaluation (t18) |
| `internal/worker/hooks.go` | Validator-origin writer: assurance-hook rejection reviews |
| `internal/handover/verdict.go` | Validator-origin writer (task t11, issue #101): a suite verdict over a handed-over commit. A test suite **is** a deterministic producer — a commit plus a command yields the same exit code every time — where an operator reading a green tick is not evidence of anything, which is precisely the substitution t11 replaces. It sits in `internal/handover` rather than `internal/api` because the verdict and the ref measurement it judges must name the same commit, and the refusal that enforces that (a full 40-hex sha, or no record at all) belongs with the fetch it is about |
| `internal/handover/gate.go` | Validator-origin writer (task t16, issue #101): the two gate records `verdict.go` cannot compose. A gate that **measured nothing** — an instrument that does not reach the changed tree, or is absent from the host that ran it — has no exit code to record, and recording `0` would make a suite that never ran and a suite that passed the same green tick; its record therefore carries no `verdict` key at all, a reason from a closed vocabulary, and the files left uncovered. The **aggregate** is the sharper case for keeping this in `internal/handover` rather than `internal/api`: its four counts and its outcome are computed from the per-gate statuses and have no request field, so no caller can report a green gate over a report in which nothing was applicable |
| `internal/repair/route.go` | Validator-origin writer (task t32, issue #102): where a failing merge gate goes next. `Decide` is a pure function of already-recorded facts — the suite's exit code (itself a derived record), the run's own prior routings, the changed paths `internal/handover` measured, and the repair lane's advertised capability surface — so the same inputs yield the same destination every time, which is §10.4's test for `derived`. It is deliberately **not** `confirmed`: routing decides where a failure goes, and a human deciding to merge remains that human's own transaction. Nor `proposed`: nothing in the record is anybody's suggestion, and there is no field a caller can use to argue with the bound |
| `internal/devague/deliverables.go` | Engine-origin writer: devague delivery-summary derivation (pre-batch) |
| `internal/preflight/records.go` | Engine-origin writer: the clarify-then-commit gate's briefing (issue #67, task t14) — a deterministic composition of the host capabilities a bridge advertised and the pinned task declaration, computed by the engine and asserted by nobody |

## Invariant 3 — Every committed example compiles (#73)

`examples/pr-upkeep/workflow.yaml` shipped an authoring convention that had
never compiled. The mechanism worked (the observable rode run input as a
pointer, proven live); the *documented shape* — a nested object under
`bindings` — was schema-invalid, and nothing noticed because nothing ever
compiled the examples. Task t5 closes the recurrence half; task t6 gave the
convention a shape that does compile (`#/$defs/bindingValue`'s `literal:`
wrapper), so the documented and the compilable form are now the same one.

Two layers, deliberately different in kind:

| Layer | What it enforces |
| --- | --- |
| `scripts/validate-examples.sh` | Runs `nodes validate` — the verb an author runs by hand — over every `examples/**/*.yaml`. No control plane: compilation is offline, and `NODES_DATABASE_URL` is scrubbed from each invocation so that stays true by construction. Exits 1 on a non-compiling example, 2 if it finds no files at all. |
| `tests/lint/examplescompile_test.go` | Compiles the same set in-process through `internal/compiler` (so `go test ./...` is red too), and locks the CI wiring: a job runs the script, declares no service containers or database env, and — the subtle half — `.github/workflows/go.yml`'s `paths` filters list `examples/**`, so the change that breaks an example is the change that runs the gate. |

Both refuse to pass vacuously: the script exits 2 on an empty file list, and
the test fails below `exampleWorkflowFloor` discovered files. A gate reporting
a clean sweep over zero files is how this check would rot back into the
state issue #73 describes.

Run it locally with `scripts/validate-examples.sh` (set `NODES_BIN` to reuse
an already-built binary).

## Invariant 4 — Every committed example is portable (t16)

Invariant 3 asks whether an example *compiles*. This one asks whether a
deployment that is not ours can actually **load** it. A committed demo is
read and copied by third parties; one that quietly assumes our fleet is a
demo that fails for everyone else, and — in the case this invariant was
written for — one that quietly ran our code on their machines.

An environment-specific value reaches a graph through exactly three named
sources, and nothing else:

| Source | Shape in the graph | Who supplies it |
| --- | --- | --- |
| Run input | a `/run/input/...` pointer, property declared in `spec.contract.input` | the run's input document |
| Actor / runner registry | an `actor://` or `runner://` id in `uses:` | the deployment's actors table, resolved by `internal/worker/registry.go` with the `@sha256` revision suffix stripped |
| Granted environment values | an `environmentRefs` name on a code operation | the worker process that dispatches it; `internal/runners/headspace/bridge.go`'s `resolveEnv` refuses the operation **by name** when one is unset |

Everything else in a graph is deployment-independent. A hostname, an
address, a filesystem path or a source URL may appear in a **comment**,
where it is provenance — "this deployment observed the 403 on thor" is worth
keeping — and never in a **value**, where it is a requirement the loader
cannot satisfy.

| Layer | What it enforces |
| --- | --- |
| `tests/lint/exampleportability_test.go` | Reads each committed document (not the compiled IR — what a third party copies is the document). No value may carry a hostname, an IPv4 address, an absolute host path, or an absolute URL; `uses:` identities are the one exception, being registry keys. Every registry id and every `environmentRefs` name must be named in that file's own `Deployment configuration` block, since those are the values that resolve *outside* the document. And no code operation's `argv` may contain a URL at all. |
| `tests/test_pr_upkeep_sweep.py::TestTheSweptRepoIsPinnedAndSaysSo` | The one deliberate exception, both halves. `sweep.py`'s `SONAR_COMPONENT_KEY` and `GITHUB_REPO` stay hard-coded — a blast-radius boundary, since the sweep walks every open PR on the repo it names — and the test asserts the pin is *real* (plain literals; the module's set of environment reads is exact and repo-free) and that it is *documented*, at the constant and in the example's README, as the one value a new operator changes. |

The sharpest thing this invariant catches is not portability at all. Before
t16, `examples/pr-upkeep/workflow.yaml`'s sweep node fetched its script from
a `raw.githubusercontent` URL pinned to one org, one commit and one path: a
third party who loaded the demo got a graph that silently fetched and
executed *our* bytes. The source is now a granted value the deployment
chooses, verified against a granted sha256 before anything runs.

## Extending an allowlist

Applies to invariants 1 and 2. A failing gate on a file you just added means
one of two things, and the failure message says which:

1. The write belongs behind an existing boundary — observed evidence
   through `internal/runners`, confirmation through `ledger.CommitReview`,
   derivation behind a validator/engine-origin producer. Move it.
2. The file genuinely has new standing. Then — deliberately, in a reviewed
   change that says so — extend the allowlist in
   `internal/invariants/invariants_test.go` **and** the matching table
   here, stating why the file has standing. Never extend an allowlist just
   to make the gate pass.
