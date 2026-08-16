# Delivery Summary — upkeep dispatch, stable addresses, the gate as a node

plan: `upkeep-dispatch-stable-addresses-gate-as-node` · run: `partial` · date: `2026-08-16`
baseline: `devague summary skeleton`

## Intent

The owner's ordering from issue #139, taken through the full devague chain:
handle #125 (every triggered upkeep run fails closed) first, then #136 (five
actors unreachable), then #101 (the merge gate runs in a human's session).

The scope pass changed two of those materially before any code was written.
Issue #125's code fix had already shipped in `261524f`; the live blocker was the
*deployed* bridge config. And #136's actors were reachable at scope time, with a
worse cause than the issue recorded: spark holds **two** dynamic wireless NICs on
one `/24`, so re-registering at the other address relocates the fragility rather
than reducing it. The owner ruled that **no participant address is stored at
all** — bridges dial out and identify themselves with a control-plane-issued
token — and separately deferred WebSocket to its own issue once it became clear
that *direction of initiation*, not wire framing, is what removes a stored
address.

A rigorous `/challenge` pass (five escalation signals: migration,
security-sensitive credentials, distributed state, hard-to-reverse column drops,
concurrency) found eight things before planning. Two changed the plan's shape and
are the reason this run is `partial` rather than `complete`.

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — Control plane sends a repository IDENTITY on dispatch, read from actor.metadata
- `t2` — All five adapters map a repository identity to a local checkout, refusing collision and miss
- `t3` — Carry the actor's rejection body and class into the attempt result
- `t4` — Live proof: a triggered pr-upkeep run dispatches with the allowlist still multi-entry
- `t5` — Control plane ISSUES per-bridge dial-in credentials: mint, digest, reveal once, revoke
- `t6` — Dial-in presence is a read-only operator view: which bridges are connected right now
- `t7` — human-inbox tracker confirms its bridge identity without reading `endpoint_ref`
- `t8` — Convert all five bridges to authenticated dial-in using issued credentials
- `t9` — Register actor.metadata.`handover_remote` for every actor before the column goes
- `t10` — Demonstrate all five bridges dialled in SIMULTANEOUSLY — the gate v5 became
- `t11` — Re-justify migration 0036's ADR 0002 bypass against the measured fleet, or drop the bypass
- `t12` — Disabling the outbound fallback checks live presence for all five at that moment
- `t13` — Distribute the issued credential without acquiring the secrets lane's known hazards
- `t14` — Apply migration 0036 as the final step and prove the suite green without the columns
- `t15` — Measure node and npm in the toolchain baseline and recapture all three hosts
- `t16` — Express the TDD gate as a code node through the runner boundary, with a merge node routing on its outcome
- `t17` — State the gate's bootstrap asymmetry in the delivery summary rather than implying self-verification
- `t18` — Guard the adapters' zero-dependency and byte-identical properties through the dial-in change
- `t19` — Sequence 0036 last so every prior step is demonstrated while the per-bridge rollback still exists
- `t20` — Decide the duplicate-execution window's disposition: bound it, instrument it, or accept it time-bounded
- `t21` — Write the cycle's narrative record: who it is for, what it was like before, and why it matters
- `t22` — Live acceptance of all three halves, each named with a run id
- `t23` — File the websocket exploration issue the owner deferred
- `t24` — Close the issues this cycle resolves, and update the ones it does not

## Actual Delivery

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | `internal/actors/repository.go`, `repositoryIdentityOf` in `registry.go`, one line in `dispatch.go`. Key is `repository_identity`, deliberately **not** `repo`. Merged `be3ffd8` |
| `t2` | delivered | Shared `repositories.py` byte-identical across **three** adapters (not five — see drift), resolution ladder `input.repo` → identity → `only_allowed_repo()`, four named refusals. Merged `e1f0dc4` |
| `t3` | delivered | `internal/actors/actorerror.go`; the bridge's class and the engine's kept in separate fields; bounded at 2048 (the package's existing `maxCapturedBodyBytes`); UTF-8 and control-character sanitization. Merged `2b21ea8` |
| `t4` | **blocked** | All preconditions staged (actor revision 2 carries `repository_identity`; bridge config carries `repo_identities`; allowlist still multi-entry). The control plane must ship t1's code first, and the production deploy is not performed |
| `t5` | delivered | `inbound_issuance.go`, `POST /v1alpha1/inbound/credentials{,/revoke}`, migration `0037`. Reveal-once with buffer zeroing; redacts in `String`/`GoString`/`MarshalJSON`/`LogValue`. Merged `ffca814` |
| `t6` | delivered | `GET /v1alpha1/dial-in-presence` + `nodes actors dial-in`; the 30s window extracted out of `registry.go` so dispatch and the view share one definition. Merged `d7ac0b6` |
| `t7` | delivered | Bridge mints a `store_id` (`O_EXCL`, 0600) reported over authenticated `GET /identity`; `tracker.py` reads no address at all. Merged `13705bd` |
| `t8` | **partial** | **One** bridge converted (`company/developer`), proven live. The other four are blocked by #147 |
| `t9` | **partial** | The *tooling* is delivered and verified in production — `register-actor.sh` now merges metadata instead of destroying it, confirmed by registering `repository_identity` on `company/developer` at revision 2 with `auth_token_env` preserved. `handover_remote` itself is **not** registered on any actor |
| `t10` | **blocked** | Requires four more converted bridges; blocked by #147 |
| `t11` | delivered | Bypass **withdrawn**, not reworded; `migrations_test.go` inverted to pin the withdrawal and forbid the old premise reappearing. Commit `f5a62c9` |
| `t12` | **blocked** | Its own guard correctly refuses: one of five actors has current presence |
| `t13` | delivered | `deploy/prod/issue-dialin-credential.sh`; credential flows `ssh control 'curl' \| ssh bridge` as a pipeline, never a variable or argv. Also closed a gap t5 left: nothing installed the issuance bearer. Merged `2441a32` |
| `t14` | **dropped** | Cannot happen in this release at all — see #143 |
| `t15` | **partial** | `node`/`npm` added to `TOOLS`; **spark recaptured only**. thor and orin could not be reached (LAN outage) and the capture truncated their baselines to empty while exiting 0 (#146); restored |
| `t16` | delivered | `examples/merge-gate/`, `scripts/merge-gate.py`, `internal/handover/gate.go`, `POST /v1alpha1/runs/{id}/gate-reports`. Node-kind enum unchanged and now test-pinned. Merged `06e9925` |
| `t17` | delivered | This document — see "The bootstrap asymmetry" below |
| `t18` | delivered | `tests/lint/dialintransport_test.go` (the guard CLAUDE.md claimed existed and did not), widened `check-zero-runtime-deps.sh`, AST-based import scan. Merged `fb3b35b` |
| `t19` | **dropped** | Superseded by #143: 0036 is not sequenced last in this cycle, it is deferred to a later release entirely |
| `t20` | delivered | `docs/decisions/dialin-duplicate-execution.md` — disposition INSTRUMENT, with the reasoning. Commit `9fba797`'s sibling |
| `t21` | delivered | This document |
| `t22` | **blocked** | Two of the three halves are unproven without the deploy |
| `t23` | delivered | Issue #141 |
| `t24` | **partial** | 13 issues opened and 3 commented; **no issue closed** — closing #125/#136/#101 requires the live proofs |

## Mid-work Decisions

No deviations were recorded in devague's delivery store: the owner directed that
deviations be filed as GitHub issues prefixed `deviate:` instead, so those issues
are the record and are cited by number below.

- **#142** — `register-actor.sh` built each new revision's metadata from a
  hardcoded `{"auth_token_env": …}` literal. Actor rows are append-only, so the
  moment an actor carried `handover_remote` the next registration would erase it.
  It also hardcoded `kind`/`protocol` (so it could not register the human actor
  at all) and its idempotency check could not see a metadata-only change. Fixed
  by merging inside Postgres.
- **#143** — the ADR 0002 bypass in migration 0036 rests on a premise that was
  measured and is false. `deploy.sh` takes one host argument, `migrate` is
  thor-only, and orin's N-1 worker reads `actors.endpoint_ref` and reads/writes
  `runner_invocations.endpoint`. Withdrawn, not reworded.
- **#144** — CLAUDE.md asserted `tests/lint/` enforced byte-identity for
  `dialin.py`. It did not. The guard had to be built, which was the more valuable
  half of t18, because the dial-in rewrite lands in five files at once.
- **#149** — the full cutover does not land: one bridge converts, not five.
- **WebSocket dropped from the cutover** (owner decision, mid-run): it would not
  have changed what addresses are stored, and would have cost a dependency in
  five zero-dependency adapters or hand-rolled RFC 6455 framing in a
  byte-identical module. Deferred to #141 on its own merits, so t23's recorded
  decision **stands** rather than being overturned.
- **t2 shipped to three adapters, not five** — `human-inbox` and `notify` have no
  `repo_allowlist`, no checkout and nothing to map an identity to. A lint guard
  makes the split a *discovered property* (resolver iff `repo_allowlist`) rather
  than a hand-maintained roster.
- **t2's inference mechanism was added deliberately** — a declared map alone
  makes the ambiguity acceptance criterion vacuous, since one dict key cannot
  collide with itself.
- **The credential was rotated mid-run** after `systemctl --user show` printed it
  into an operator session. The control-plane half of that rotation is **still
  pending** (see Remaining Work).

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t14` (#143) | The drop requires a *later release* than the code that stops using the columns; orin's N-1 worker reads both dropped columns and `deploy.sh` cannot restart both hosts in one operation | needs-follow-up |
| `t19` (#143) | Sequencing 0036 last within this cycle is moot once it is deferred to a later release | acceptable |
| `t8`, `t10`, `t12` (#149, #147) | `dialin.configured()` reads only `os.environ` and spark's four claude bridges share one `EnvironmentFile`, so per-bridge identity has nowhere to live | needs-follow-up |
| `t4`, `t22` | The production deploy was refused by the operator environment's permission classifier; every other precondition is staged | needs-follow-up |
| `t9` (#142) | The tooling gap was not in the plan; `handover_remote` registration itself remains undone | needs-follow-up |
| `t15` (#146) | thor and orin unreachable during recapture; the capture *truncated* both baselines and exited 0 | needs-follow-up |
| `t2` | Three adapters, not five — two have nothing to map an identity to; shipping there would have been dead code | acceptable |
| `t24` | Issues opened and commented, none closed — closure depends on the blocked live proofs | needs-follow-up |
| `t18` (#144) | Acceptance criterion 3 said the guard "still passes"; it did not exist and had to be built | acceptable |

## Evidence

- go: `go build ./... && go vet ./... && go test ./...` — pass
- python: `uv run pytest -n auto` — **334 passed**
- adapters: `adapters/human-inbox` — 188 passed; codex and claude-code lint jobs
  each run exactly as its workflow invokes it (codex from the repo root) — pass
- lint: `black`, `isort`, `flake8`, `bandit`, `markdownlint-cli2` (134 files),
  `uv run teken cli doctor . --strict` — all clean
- examples: `scripts/validate-examples.sh` — all 13 compile
- commits: `57d5386..f94e6d7` (25 commits, 9 TDD-gated merges)
- issues opened: #141–#153; comments on #121, #136, #147
- dispatched runs: `01M05ZG7GNPZM96RKNR6RBW4B1` (graded 5),
  `01M05ZGNT86MAFDHATB6W5VYPN` (graded 5), `01M05ZH6AW6TFDSESF3B0GKD3A`
  (graded 5), `01M063JW8M1G0NSVZY6MFT3DJV` (dial-in transport proof)

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| The control plane supplies a repository identity that no payload can influence | high | commit `be3ffd8` · `internal/actors/repository.go` · `TestRunInputCannotInfluenceTheDispatchedRepositoryIdentity` |
| A bridge resolves an identity to a checkout while its allowlist holds several entries | high | commit `e1f0dc4` · `test_a_multi_entry_allowlist_resolves_the_identity_it_was_given` |
| **A triggered `pr-upkeep` run dispatches** | **unverified** | t4 blocked — the control plane does not yet run t1's code. Not claimed done |
| A refused dispatch's reason is readable from the run view | high | commit `2b21ea8` · `TestRunViewShowsTheActorsRejectionTextAndClass` |
| The control plane can issue, reveal-once and revoke a per-bridge credential | high | commit `ffca814` · migration `0037` · `TestIssueInboundCredentialPersistsOnlyTheDigest` |
| **A bridge that dials out is dispatchable through the mailbox** | high | run `01M063JW8M1G0NSVZY6MFT3DJV` — one `inbound_actor_mailbox` row, claimed→completed in **36 ms**, attempt `att_01M063JWFZX12573EHQQVG5791`. First row that table has ever held |
| An operator can ask which bridges are connected, without dispatching | high | commit `d7ac0b6` · `TestDialInPresenceIsServedWithoutProbingAnyParticipant` |
| `register-actor.sh` adds a metadata key without destroying the existing one | high | verified **in production**: `company/developer` revision 2 carries both `auth_token_env` and `repository_identity` |
| The TDD gate runs as a code node and emits derived per-gate records | high | commit `06e9925` · a real run of the pinned matrix over 100 changed files returned `gates_passed` for `go-build` with `instrument_version` recorded |
| A package cannot reach the merge node without a passing gate | high | `examples/merge-gate/workflow.yaml` — exactly one edge into `human-merges`, no agent node in the graph |
| **Stored participant addresses are retired** | **unverified** | Explicitly NOT claimed. Four of five bridges dispatch outbound, the fallback is on, both columns exist. Step 2 of a 5-step expand-contract sequence |
| **All five bridges dial in simultaneously** | **unverified** | t10 blocked by #147. Not claimed done |

## The bootstrap asymmetry (task t17)

**This cycle's own merges were operator-gated, not gate-node-gated.** Nine
merges were TDD-gated by the operator typing the suites and reading the result —
the exact practice #101 exists to replace.

That is not an oversight; it is structural. A gate node cannot gate the delivery
that introduces it. Its first real verdict will be on a package merged *after*
this cycle, and that run id is owed before anyone may say the gate is in force.

What the cycle *can* claim is stronger than a promise: the gate program was run
for real against this branch's own diff — 100 changed files, ten gates — and it
worked, and it found three defects in itself that its tests did not (#148, #152, #153).
One of those, #152, is that the pinned `adapter-lint` command lints
`.venv`, so the gate's verdict depends on whether anyone has run the adapter
tests. A gate whose answer depends on the filesystem is exactly the thing a
digest-pinned matrix is supposed to prevent, and it was found by using it rather
than by reading it.

## Hand-turn accounting (task t21)

CLAUDE.md: *"Manual operator work is invisible by default… An issue per hand-turn
makes the backlog of un-automated steps countable."*

**21 operator hand-turns**, against fifteen in the previous cycle. The comparison
is not like-for-like and should not be read as regression:

- **9 are TDD-gated merges**, which #101's gate node is designed to replace and
  could not yet, for the bootstrap reason above.
- **6 are production/host steps** the previous cycle did not attempt at all
  (first-ever dial-in provisioning, credential rotation, actor re-registration,
  bridge config, the tailnet switchover during the outage).
- **4 are dispatches**, three of which produced work the operator would otherwise
  have done by hand — and all three were graded, per the dogfooding reflex.

The honest reading: the count went **up** because the cycle did more production
work, and the mechanism that would bring it down (#101) shipped but cannot take
effect until the next cycle. That is the number to beat, and #118 does not close
until it falls with the workload held constant.

## Remaining Work / Follow-up

**Blocking, in order:**

1. **The production deploy** — refused by the operator environment's permission
   classifier. Run `bash deploy/prod/deploy.sh thor`. **Read #151 first**: the
   hand-provisioned credential has `issued_at IS NULL` and the new admission
   default will refuse it. The issuance bearer is already provisioned on thor so
   re-issuing is possible; without it the bridge would have been stranded.
2. **Finish the credential rotation** — the drop-in holds a new token, the
   control plane still holds the old digest. Nothing breaks until the bridge
   restarts. Command is in #147's thread.
3. **`t4` / `t22`** — the two live proofs, immediately after (1).
4. **#147** — give dial-in settings a per-bridge home. Everything downstream
   (`t8` remaining four, `t10`, `t12`) is blocked on it.
5. **#143** — ship a release where no binary reads either column, deploy it to
   **both** hosts, and only then apply 0036 in a later release.

**Newly discovered, filed, not started:**

- #145 — implement the overlap instrumentation `t20` decided
- #146 — `toolchain-baseline.sh` truncates baselines on failure and exits 0
- #148, #152, #153 — three defects the gate found in itself
- #150 — the whole inbound lane is missing from `openapi.yaml`
- `deploy/prod/actor-placement.sh` is a **fourth** unconverted `endpoint_ref`
  consumer, found by t7 and recorded on #121; the decision doc still lists two

**Also outstanding:**

- `handover_remote` is registered on **no** actor (`t9`'s remaining half), and
  #121 records that `collect-handover.py` falls back to a template without it
- thor and orin toolchain baselines still lack `node`/`npm` measurements
- Twelve genuine `flake8` findings in `adapters/human-inbox`, which no CI
  workflow covers — three of the five adapters have no workflow at all
