# independent-review

Plan task t13 (`docs/plans/2026-08-12-operate-through-the-ui.md`), spec claim
c17, honesty condition h11: an independent-LLM-review node pattern, where
the node reviewing a change set dispatches to a **different actor backend**
than the node that built it.

## Why this is a separate example from self-hosting-loop

`examples/self-hosting-loop/workflow.yaml` already has a `verify` node that
reviews `build`'s diff. But nothing about that example's `uses:` values
forces `verify` onto a different backend than `build` — both are just
`actor://company/<role>@sha256:...` identities, and on the thor/orin fleet
today they can both resolve to the same underlying model family. That is
the gap this example closes: it is authored so the build and review actors
are provably different bridge implementations, not merely different actor
*names*.

- `build` is placed on `company/developer` — the claude-code bridge
  (`adapters/claude-code`), same actor identity `examples/self-hosting-loop`
  and `examples/delivery-loop` place their own `build` node on. The digest
  placeholder is reused verbatim from those examples on purpose: it names
  the same identity, the same way `examples/codex-smoke-pair`'s header
  comment explains reusing self-hosting-loop's own placeholders.
- `review` is placed on `company/codex-thor` — the codex bridge
  (`adapters/codex`), the actor identity `examples/codex-smoke-pair`
  registers and dispatches to for real. Its digest placeholder is reused
  verbatim from that example for the same reason.

Two different model families behind two different bridge implementations,
reviewing the same diff, is exactly `.claude/skills/ask-colleague/SKILL.md`'s
own stated reason for existing: *"The point isn't a stronger model; it's a
second, independent mind, and that diversity is the value."* This workflow
is that principle expressed as a graph node instead of an interactive skill
invocation — the same second-opinion reflex, but running through the normal
engine dispatch path (create run → engine claims node → worker dispatches
to the actor's own bridge), not a throwaway worktree.

## What `review` actually receives, and what it does not

Acceptance for this task requires the review node's input to carry "the
measured facts from the build attempt... NOT the builder's self-reported
summary alone." Getting this right meant reading `internal/worker/bindings.go`
and `internal/engine/binding.go` (the two binding resolvers — the worker's
is what agent nodes actually use) rather than assuming task t10's
`workspace_measured` block was already reachable. It is not, and that gap is
this task's one real deviation. In full:

**What t10 actually shipped.** `adapters/claude-code/src/claude_code_bridge/
mapping.py`'s own docstring is explicit: every bridge response carries a
`workspace_measured` key — `head_before`/`head_after`, `status_porcelain`,
`diffstat`, `changed_files` — measured by real `git` calls
(`workspace.py`) bracketing the session, structurally separate from
`output` (the model's own, always-empty-for-claude `changed_files`). All
three bridges (`claude-code`, `codex`, `colleague`) ship it, per the
all-backends rule.

**Where it stops.** `internal/actors/protocol.go`'s `InvocationResult` — the
Go struct that decodes a bridge's synchronous response — has no
`WorkspaceMeasured` field at all (confirmed by reading the whole type and by
`grep -rn "workspace_measured\|WorkspaceMeasured" internal/`, which returns
nothing outside the Python `adapters/` tree). A `workspace_measured` key in
the bridge's JSON is simply extra input `json.Unmarshal` silently drops.
Neither binding resolver's own doc comment claims otherwise:
`internal/worker/bindings.go` names its three resolvable surfaces as
`/run/input`, `/nodes/<id>/output`, and `/ledger/projections/<name>`, and
says plainly that `/nodes/<id>/evidence` "needs the runner boundary and the
artifact router" and is refused rather than silently resolved.
`internal/engine/binding.go` (used for control-plane-only nodes like
`approval`/`end`) resolves even less — `/run/input` and `/nodes/<id>/output`
only, no ledger projections at all. So today, **no binding pointer this
compiler accepts can reach t10's measured git facts.** This is the
deviation this task's own rules anticipated: "implement the closest honest
binding, and report exactly what's missing." The rest of this section is
that binding.

**The closest honest binding, in three parts:**

1. `taskInstruction: /run/input/build_instruction` — the ORIGINAL
   instruction given to `build`, read straight from the run's own input.
   `review` never sees `build`'s paraphrase of its own assignment; it sees
   what the run actually asked for, the same way self-hosting-loop's `plan`
   node reads `/run/input/plan_instruction` rather than trusting `intake`'s
   retelling of it.

2. `changeSet: /nodes/build/output` — `build`'s own contract-shaped answer,
   bound as the WHOLE output object rather than narrowed to
   `.../output/summary`. Today that object is just `{summary}` (build's
   self-report, honestly labelled as such in this README and in the
   workflow's own comments) — but the binding is future-proof: if build's
   contract later adds a self-reported structured field, or if a future
   task wires `workspace_measured` into `InvocationResult.Output` itself
   (the most direct fix — see "Recommended follow-up" below), `review`
   picks it up with no change to this file.

3. `measuredEvidence: /ledger/projections/evidence` — the run's
   observed-evidence ledger projection, the SAME surface (and the same
   binding idiom, under the name `testEvidence`) `examples/delivery-loop`'s
   and `examples/self-hosting-loop`'s own `verify` nodes already use to read
   their `test` node's runner-measured evidence. `build` here carries the
   `post_run` workspace-snapshot hook from task t12
   (`examples/workspace-snapshot-hook` is that pattern's own dedicated
   example) instead of a separate `test` node, because it is `build`'s own
   workspace being measured. `internal/worker/hooks.go`'s
   `buildHookOperation` requests a runner-measured before/after comparison
   around every hook dispatch; when a runner can honour that request, the
   result lands as `observed` evidence through `internal/worker/hooks.go`'s
   append path — never through `build`'s own `ledger.propose` delta,
   because `internal/compiler/ledger.go`'s `checkLedgerDelta` never lets an
   agent node declare `observe` at all.

**Why (3) resolves to nothing today, and it is two gaps, not one.**
`examples/workspace-snapshot-hook/README.md` already documents the first
gap honestly: no runner this build ships (`internal/runners/headspace`,
`internal/runners/lambda`) can honour a workspace-comparison request yet, so
`build`'s hook evidence carries no `changed_paths`/`snapshot_digest` today —
identical to that example's own "why the evidence is empty" section. Reading
`internal/ledger/projection.go` while building this example surfaced a
SECOND, independent gap, pre-existing and not introduced here:
`internal/worker/bindings.go`'s `projectionKindFor` comment for `"evidence"`
claims binding it "selects the run's evidence records" with an empty
subject, but `EvidenceForSubject` (`internal/ledger/projection.go`) only
appends a record when `ref != ""` and the record's `SubjectRef` or
provenance names that ref — with `ref == ""` (exactly what the worker
resolver always passes for this binding name), the loop body never runs and
the projection is unconditionally empty, independent of whatever evidence
the run actually holds. `examples/delivery-loop` and
`examples/self-hosting-loop` bind this exact projection (`testEvidence`)
today and inherit the same gap; it is not new to this example, but nothing
in either of those two reference workflows' own docs calls it out, so this
is the first place it is written down. Not fixed here — Go source is out of
scope for this task — only reported, per the task's own instruction to
report exactly what is missing.

**Recommended follow-up**, for whichever task picks this up next: (a) decode
`workspace_measured` into a typed field on `internal/actors.InvocationResult`
(or a sibling type) and surface it as a resolvable binding — most directly
by folding it into `/nodes/<id>/output`'s answer, since agent nodes cannot
hold `observe` authority and a brand-new bindable surface would need its own
task the way `/nodes/<id>/evidence` already does; (b) fix
`EvidenceForSubject`'s empty-subject case (or `projectionKindFor`'s call
site) so the binding idiom `testEvidence`/`measuredEvidence` actually
returns a run's evidence records as its own doc comment already claims.

## Structured review output: findings + verdict, and why two outcomes

`review`'s contract declares two outcomes, `approve` and `changes_required`,
each requiring the same payload shape: a `verdict` field and a `findings`
array of `{severity, note}` objects (`changes_required` additionally
requires at least one finding — an empty change-required list would be a
contradiction the schema now catches at commit time, not just in review).
Two declared outcomes rather than one `completed` outcome with a `verdict`
string buried inside its payload, on purpose: PRD's domain-outcome rule is
that `changes_required` is a domain outcome that follows its own graph
edge, never a value a human has to notice inside a payload or an engine
failure to work around. `examples/delivery-loop`'s own `verify` node already
establishes this shape (`passed`/`changes_required`/`blocked`); this example
follows it.

**Bridge honesty this schema does not paper over.** Both shipped bridges'
own mapping modules (`adapters/claude-code/src/claude_code_bridge/
mapping.py`'s `classify()`, `adapters/codex/src/codex_bridge/mapping.py`'s
equivalent) pick the reported outcome name from the invoking node's own
STATIC `input.success_outcome` field — never by parsing the model's answer
content. `adapters/codex/README.md`'s own "Invocation input fields" table
says it plainly: `success_outcome` is "Domain outcome reported for
`status: ok`". So the OUTCOME NAME this workflow routes on (`approve` vs
`changes_required`) and the `verdict` field VALUE inside the model's own
`findings` payload can, in principle, disagree — a live run's edge is
decided by what `input.success_outcome` names (bound here from
`/run/input/review_success_outcome`, so an operator chooses it per run — see
`input.json`), not by what codex-thor actually concluded in prose. This is
the exact limitation `examples/self-hosting-loop`'s own header comment
records for its `verify` node ("the claude bridge maps success to one
declared outcome, so branch selection... belongs to the human reviewing the
real diff") — this example inherits it because it dispatches through the
same bridge family, and states it explicitly rather than letting a reader
assume the graph edge reflects the model's judgment. `ship-review` below is
exactly why that gap does not matter operationally: a human reads `review`'s
actual output — verdict text and findings both — before anything ships,
regardless of which edge got the run there.

## §10.4 honesty: proposed claims into a human decision, nothing auto-confirms

`review` proposes a `claim` record (`ledger: propose: [claim]`) — the same
record type `examples/self-hosting-loop`'s and `examples/delivery-loop`'s
own `verify` nodes propose, and deliberately NOT the ledger's `review`
record type. `schemas/ledger/review.schema.json` reserves `review` records
for the human confirm/reject transaction itself (PRD §10.8); `internal/
ledger/authority.go`'s `checkHumanAuthority` only lets `confirmed`/
`rejected` authority land through a review transaction appending a `review`
record referencing its target. An agent's own verdict on a diff is not that
transaction — it is a completion claim about the diff, exactly as PRD §10.4
frames it: "an agent saying 'done' is a completion claim, not verified
evidence." Using the `claim` record type keeps that distinction visible in
the ledger, not just in this README.

Both of `review`'s outcomes route to a place a human decides, never straight
to `finish`:

- `review.approve` → `ship-review` (an `approval` node, placed exactly where
  self-hosting-loop places its own happy-path gate) → a human's `approved`
  decision → `finish`, or a human's `rejected` decision → back to `build`.
- `review.changes_required` → straight back to `build` (delivery-loop's own
  pattern: a domain answer that loops without needing a human to approve
  sending more work back).

There is no edge from either of `review`'s outcomes to `finish`. The ONLY
path to `finish` is `ship-review.approved` — a human decision recorded
through `POST /v1alpha1/human-tasks/{id}/decision`
(`internal/api`), never something the engine or either agent node can
produce on its own. So even in the branch where the model itself says
`approve`, nothing ships until a person reads `ship-review`'s bound context
(`/nodes/review/output` — the same findings and verdict this README already
described) and decides. That the graph makes this the ONLY path to `finish`
— not a convention this README asserts but a shape `go run ./cmd/nodes
validate` and the compiler's reachability check both enforce — is the test
this acceptance criterion actually asks for: a workflow shape, not a prose
promise, guarantees the review's claim lands proposed and feeds a human
decision.

## Offline validation

```bash
go run ./cmd/nodes validate examples/independent-review/workflow.yaml
```

compiles clean with zero errors and zero warnings — no network, no bridge,
no runner required:

```text
valid: independent-review 1.0.0 (0 errors, 0 warnings)
digest: sha256:848e802716eced17b05f943e1fbf9253838cf39f2e9150235793f0f62279fd79
```

`input.json` is a sample run input matching `spec.contract.input`'s schema
— `review_success_outcome: "approve"` walks the happy path through
`ship-review`; set it to `"changes_required"` to exercise the direct loop
back to `build` instead.

No compiler-level test fixture references this example, matching the
existing convention: `examples/self-hosting-loop`,
`examples/workspace-snapshot-hook`, and `examples/placement-proof` are also
validated only by the `nodes validate` command in their own README, not by
a Go test. (`examples/delivery-loop` is the one exception — it is the PRD's
own canonical reference workflow, wired into `tests/e2e` for that reason —
and `examples/codex-smoke-pair` has its own dedicated
`tests/deploy/codexsmoke_test.go` because it is a live, billable smoke
check, not an offline-only pattern demonstration like this one.)
