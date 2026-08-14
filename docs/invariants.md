# Invariant gates

Invariants 1 and 2 below are batch-wide promises enforced by one source
sweep; [invariant 3](#invariant-3--every-committed-example-compiles-73) is a
later, differently-shaped gate over the `examples/` tree. All of them run
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

### `AuthorityObserved` — runner boundary only

| File | Standing |
| --- | --- |
| `internal/ledger/record.go` | Vocabulary: defines the constants |
| `internal/ledger/authority.go` | Append-time enforcement: observed needs a runner manifest |
| `internal/engine/ledgerdelta.go` | Refusal gate: node deltas may propose/observe per declared contract only |
| `internal/runners/dispatch.go` | **The writer**: boundary-measured evidence, `OriginRunner` + observed |

### `OriginRunner` — stamped only by the boundary itself

| File | Standing |
| --- | --- |
| `internal/ledger/record.go` | Vocabulary |
| `internal/ledger/authority.go` | Enforcement: runners write manifest-checked observed evidence only |
| `internal/runners/dispatch.go` | **The writer** |

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
| `internal/devague/deliverables.go` | Engine-origin writer: devague delivery-summary derivation (pre-batch) |

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
