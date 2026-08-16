# Changelog

All notable changes to this project will be documented in this file.

Format follows [Keep a Changelog](https://keepachangelog.com/). This project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.27.0] - 2026-08-16

A failing gate stops landing in the operator's session, and a deploy records
what it shipped (task t32, issues #102 and #104, plus #120 item 4).

**Why a minor bump.** Two new surfaces (`GET /v1alpha1/version`, a `deployment`
key in the bridges' capability contract), one changed one: POST
`/v1alpha1/runs/{id}/suite-verdicts` now answers `SuiteVerdictResult`
(`{verdict, routing}`) rather than a bare `LedgerRecord`. That route is one
release old and lives in `v1alpha1`; splitting the routing across a second
round trip would have put the operator back inside the loop the routing exists
to take them out of.

### The gate → repair → gate loop is closed, and bounded (#102)

Nine packages in the own-the-work-end-to-end batch failed their gate, and
every one was repaired by hand in the operator's own interactive session —
the most expensive lane in the deployment, and the only one whose work leaves
no ledger record. The system already had the vocabulary for this and did not
use it: a failing gate is a domain outcome (PRD §3.4), and a domain outcome is
a thing that routes.

`internal/repair` now decides where a rejecting suite verdict goes, as a
`derived` validator record composed from already-recorded facts. **The bound is
two numbers and a stated behaviour, all three enforced and all three in the
record**: at most 2 repair attempts per run, over a 24-hour window measured
from the run's first gate rejection, and a human node at either ceiling. The
two bound different things — attempts bound spend, the window bounds staleness
— and a run can hit either without the other.

Three refusals outrank the budget, because a repair that was never possible
must not be reported as one the attempts ran out on:

- **the workflow-scope boundary** — a failure implicating `.github/` goes to a
  person, because a repair attempt is a dispatch and a dispatch may not modify
  CI configuration. The implicated paths are the ones the control plane already
  *measured* for the run's handover, so this fires without anyone remembering
  to declare it;
- **a lane that cannot verify** — read off the lane's own advertised capability
  surface. A posture that grants no `workspace-write` cannot repair; one that
  cannot run the failing suite cannot check its own fix. This is #119
  mechanized: `--requires-grant network-egress` is how a database-backed suite
  says what its repair lane would need;
- **a lane that advertised nothing** — refused rather than assumed, the same
  fail-closed rule `retryRefusal` states.

**Unattended execution is deliberately not enabled.** The control plane decides
and records the route; it does not dispatch it, and the record says so in its
own payload. The write path through the bridges is unproven (#18) and an
advertised surface cannot show that a database-backed suite is runnable on a
lane (#119) — so a repair could be dispatched at work it could not verify. What
changes is that the decision is no longer made in a person's head at the moment
they read a red gate: it is a deterministic function of recorded facts, written
down under a stated bound, leaving the human step as executing a dispatch the
system already chose and justified.

Repair rounds are now a ledger query, which is the signal #28 was missing.

### A deploy records which revision it shipped (#104, #120 item 4)

Three dispatches this cycle reported `handover=true`, committed successfully,
and created no handover ref — because the bridges installed on thor and orin
predated the code that mints them. Nothing reported a problem, because
`internal/handover` correctly records nothing when there is no fetchable ref,
so a stale bridge and an honest refusal produce byte-identical evidence. It was
found by running `git for-each-ref` on the host.

The two install shapes have different answers, and reporting only one would
have been wrong for the other:

- the codex and notify bridges are `uv tool install`ed **copies** — no git near
  them, so `deploy.sh` now stamps the resolved 40-hex revision into the shipped
  tree *before* the install copies it;
- the claude bridges on spark are **editable** installs serving a live work
  tree — they cannot go stale, and they *can* be running uncommitted code,
  which is now reported as its own hazard.

So the bridges advertise a `deployment` host fact — install mode, revision, how
that revision was learned, whether the tree is dirty, and what can go stale —
on the `/v1/capabilities` surface an operator already reads without ssh. A
revision that is not a full lowercase 40-hex commit id is refused rather than
reported, and a revision nothing can establish is stated as such instead of
returned blank.

The control plane answers for itself at `GET /v1alpha1/version`, unauthenticated
like `healthz`. Issue #104 was found by POSTing at a route that should have
existed and reading the `405`; a live test can now *assert* which code it
tested. The image is built from a `git archive` with no `.git` in it, so
`deploy.sh` passes the revision through both build lanes and the Dockerfile
injects it — and `-X main.version` finally does something, having written to a
`const` since t1.

### Also

- `internal/handover.Measured` exposes the ref, commit and changed paths of a
  measured handover; `MeasuredCommit` is now a projection of it rather than a
  second walk with its own recognition rule.
- `deployment.py` joins `preflight.py` as a byte-identical shared bridge
  module, and `tests/lint` guards both — the split and the guard are one
  change, so a shared module cannot quietly become four.
- `tests/deploy/revisionstamp_test.go` builds a real wheel and looks inside it.
  Written first as a test that passed for the wrong reason: hatchling resolves
  `.gitignore` from the adapter directory, not the repo root, so the guard now
  reproduces the adapter-local ignore rule that actually drops the stamp.

## [0.26.0] - 2026-08-16

The merge gate stops being an operator looking at a green tick (task t11,
issue #101).

Two things were being substituted for evidence. The first was the *collection*
step: `git fetch ssh://<host>/<path> <branch>`, typed by hand fifteen times
this cycle, needing three facts the operator had to already know — which
machine the session ran on, where its checkout lives, and what the ref was
called. Only the third is genuinely derivable, and the other two are in the
control plane. The second was the *verdict*: a person reading a CI status and
deciding to merge leaves nothing behind that names the suite, the exit code,
or the commit — so nothing later can check whether the thing that passed is
the thing that shipped. This cycle produced two suites that passed while
testing nothing, and neither was visible afterwards from the word "passed".

Why the authority vocabulary carries the weight here: a test suite **is** a
deterministic validator (a commit plus a command yields the same number every
time), which is exactly the producer PRD §10.4 admits `derived` records from.
An operator's opinion of a rendering is not a producer at all. So the finding
can now be recorded by the thing that produced it, and the refusals that make
it evidence — a full 40-hex commit sha or no record, an absent exit code never
read as a pass — live with it.

### Added

- **`scripts/collect-handover.py <run-id>`.** A run id alone becomes a
  reviewable diff. It asks the control plane which actor ran the run, resolves
  that actor's host from the registry, and fetches
  `refs/culture-nodes/<run-id>/*` by wildcard into `refs/handover/` — no
  branch touched, nothing checked out — then reports each ref, its commit, and
  the paths it changed. The remote is the control plane's configuration
  (`metadata.handover_remote` per actor, or `NODES_HANDOVER_REMOTE_TEMPLATE`
  with `{host}` from the registered `endpoint_ref`) and never the run's own
  report, because a session that could point the fetch at a repository it
  prepared would make the measurement real and the subject forged — the same
  fence `internal/handover/doc.go` states for the control plane's own fetch.
  Configured by neither is a refusal, not a guess.
- **The no-ref case is reported as ambiguous, and exits non-zero.** After
  issue #120 an empty result has two readings — the session handed over
  nothing, or the bridge on that host cannot hand over at all — and the fetch
  distinguishes neither. Both are named. Guessing here is how a lost handover
  becomes "the agent did nothing". An *unreachable* remote is kept distinct
  from an empty one: it exits 2, not 1, because that is no gate rather than a
  passing one.
- **`POST /v1alpha1/runs/{id}/suite-verdicts`**, and
  `scripts/collect-handover.py <run-id> --gate --suite '…' -- <cmd>` that
  drives it. The suite runs in a detached worktree at the collected commit and
  the result lands as a `derived`, validator-origin record naming the suite,
  the exit code, and the commit sha. The sha is read back from the worktree
  the suite actually ran in rather than assumed, so a suite that moved the
  tree records nothing instead of misattributing its verdict.
- **`internal/handover.MeasuredCommit`**, the reader for the observed
  handover-evidence payload `buildRecord` writes — placed next to its writer
  so the two field-name opinions cannot drift.

### Changed

- `internal/handover/verdict.go` is added to the `AuthorityDerived` allowlist
  in `internal/invariants` and `docs/invariants.md`, deliberately and with its
  standing stated: it is a validator-origin writer, and it sits beside the
  fetch rather than in `internal/api` because the verdict and the measurement
  it judges have to name the same commit.

## [0.25.4] - 2026-08-16

### Added

- **Inbound authentication records that cannot retain a presented secret
  (#111).** The schema accepts only a fixed-width SHA-256 verifier or an
  environment-variable name and rejects IP-shaped party keys, because dumps
  are shipped to object storage and thor's address is not stable. Verification
  hashes both sides before a constant-time comparison, and a PostgreSQL guard
  makes schema-only and data dumps part of CI rather than trusting migration
  text alone.

### Verified

- **Criterion 1 held against a real dump**, which the dispatched session could
  not do (#119: its sandbox denies `socket(2)`, so no database). The guard
  seeds a row whose SHA-256 is stored, then asserts the plaintext canary does
  NOT appear in `pg_dump` output. Run here against PostgreSQL 17 it PASSES;
  ablated by adding a `credential_plaintext TEXT` column it FAILS with
  `plaintext-capable credential column found`. Exit codes are 0 / 1 / 2, and
  the skip code fails CI on purpose — "I could not look" is not "I looked".

### Notes

- **The dispatch was refused by the scope guard, and correctly.** It added its
  own CI step to `.github/workflows/tests.yml`, which no bridge actor may
  modify; the run ended `policy_denied` on the *bridge-measured change set*
  rather than on the instruction text, which is the #98 fix working. Eight of
  its nine files were in scope and are merged as authored. The CI step was
  re-authored in the operator lane with one change: it runs `nodes migrate`
  first, because the table otherwise exists only as a side effect of the
  earlier `go test` step, and a guard that silently depends on step ordering
  is one reordering away from exiting 2 and looking like an infrastructure
  problem.

## [0.25.3] - 2026-08-16

Work package R8-A: tasks t4, t26 and t28 — the cycle tells the truth about
itself, in numbers a reader can re-run.

### Added

- **`scripts/cycle-accounting.py`.** Renders the four numbers the delivery
  announcement needs — opened, closed, delta, and cycle-opened-but-
  undispositioned — each as a command rather than as copied arithmetic. It
  derives the cycle boundary from git on every run (commit `1e6a532`'s
  committer timestamp) instead of a hardcoded date, and follows
  `triage-report.py`'s `--issues-json` pattern so it runs both against live
  `gh` and against a snapshot. The delta is printed as a signed number: a
  cycle that ends the tracker LARGER than it started reports negative, which
  is the case the criterion exists to protect and the one the test covers.
- **`docs/deliveries/close-the-backlog-bootstrap-honesty.md`.** All fourteen
  operator steps from the last cycle's STATE section 11, each marked
  automated-by-a-merged-node or still-manual, plus the stage-2 operator shell
  transcript reported rather than omitted, plus each of last cycle's NOT MET
  signals mapped to the issue that would flip it.

### Verified

- **Fourteen of fourteen steps are still manual.** The document was written
  marking one of them automated; that row was corrected at merge against
  evidence gathered the same day (#120 — the handover the row credited had
  produced nothing in production, and the collector it would need is t11,
  dispatched but unmerged). Fourteen is the honest number.
- **The live `gh` path works** — the one path the dispatched session could not
  exercise, because its sandbox denies `socket(2)` (#119). Run in the operator
  lane it returns 11 opened, 14 closed, delta 3, undispositioned 0. Those
  differ by one from the snapshot numbers in the document because #120 was
  filed in between, which is the evidence that the script queries rather than
  remembers.

### Notes

- The document declines to force a mapping: it records that the STATE file has
  four NOT MET headings where the spec names three actionable failures, and
  that closed issue #21 concerns bridge concurrency rather than the missing
  live cancellation observation, so it is not pressed into a row it does not
  fit. A refused mapping is a more useful finding than a manufactured one.

## [0.25.2] - 2026-08-16

Issue #8's three measured gaps in the registration/worker-config surface —
plus the SPA fallback defect that made the last one hard to even detect.

### Added

- **`nodes runner-services list` / `register`.** Registration validates the
  entry and updates the configured JSON file atomically. Reload still needs a
  worker restart: a live reload has to swap the registry AND the protocol
  client/secret set together while in-flight operations keep a consistent
  snapshot, which is a larger change than this closes. Gap 1 is therefore
  **partial**, and is recorded as such rather than as done.
- **Worker config preflight.** A worker now refuses absent or partial
  code-runner identity and inconsistent callback configuration, reporting
  every problem in ONE pass, before it connects to PostgreSQL — so a
  misconfigured worker fails on its configuration instead of on a database it
  should never have reached.
- **`GET /v1alpha1/namespaces`.** Documented and served. Both deployment
  paths use it instead of shelling out to `psql`, which was the last place
  deployment needed database credentials to answer a question the API can
  answer.

### Fixed

- **The SPA fallback no longer answers 200 for an undeclared API path
  (#8).** `GET /v1alpha1/pending-decisions` against a control plane that
  predates the endpoint returned index.html with status 200, so a client
  could not distinguish an absent endpoint from an empty one — both operator
  scripts written this cycle carry a fallback for exactly this. The guard
  lives in `spaHandler`, NOT as a `mux.HandleFunc("/v1alpha1/",
  http.NotFound)` pattern: a method-less pattern that broad also matches
  wrong-method requests to real operations, turning the mux's own 405 into a
  404 and breaking `TestOpenAPIRoutesAreServed`, which probes with DELETE to
  prove a documented route is served at all. That is what the first merge
  attempt did, and 20 tests caught it.

### Verified

- The dispatched session's own namespace test asserts the 404 against a
  server built WITHOUT web assets, where it already held — it would have
  passed with or without the fix. `TestSPAFallbackRefusesTheAPINamespace`
  mounts an `fstest.MapFS` build, which is the configuration production
  runs, and was confirmed by ablation: removing the guard makes it fail with
  `status = 200`, the exact symptom #8 describes. It also pins both
  neighbours — a client-side route still gets the SPA, and a declared
  operation asked with the wrong method still refuses with 405.

## [0.25.1] - 2026-08-16

Task t17b: an event can now START a run, not only resume one waiting for it.

### Added

- **Workflow-level `triggers`.** A published workflow may declare
  `spec.triggers: [{onEvent, when}]` — an inbound event name plus an optional
  CEL condition evaluated against `event.{name, emitter, payload}`. A matching
  trigger CREATES a run, with the event payload as the run input (validated
  against the workflow's input contract like any other input). Before this,
  `signal_events` delivery could only resume a run that was already parked
  waiting on the event, so an event-driven workflow needed a permanently
  parked run to receive its first event.
- **Trigger, recording, resume, route pickup and run creation share one
  transaction.** `DeliverSignalEvent` appends the immutable event fact and
  then evaluates triggers inside the same transaction, so an event is never
  recorded-but-unhandled or handled-but-unrecorded.
- **Only the newest published version of a workflow key is offered a
  trigger.** Publishing a newer version WITHOUT the declaration therefore
  disables future starts — the off switch is a publish, not a config edit.
  The delivered event still lands in `signal_events` either way, so a
  declined condition stays queryable rather than vanishing.

### Changed

- **`examples/pr-upkeep/workflow.yaml` drops from 1068 lines to 93.** The
  example no longer carries the sweep machinery or a parked run: it starts
  from the durable `pr-upkeep.pr` fact, conditioned on
  `event.payload.source == "github_pr" && size(event.payload.findings) > 0`.
  Repository identity and findings are event data now, so they are queryable
  even on the runs the condition declines.

### Verified

- The two acceptance tests the dispatched session wrote could not be RUN on
  its host — no PostgreSQL, no Docker (see the codex Go-lane limits recorded
  as deviation d17). Both were executed in the operator lane against a real
  database before this merge: `TestConditionedTriggerRecordsNonMatchWithout
  CreatingOrResuming` and `TestTriggerCreatesRunAndNewerVersionCanRemoveIt`
  PASS, 0.06s and 0.05s.

## [0.25.0] - 2026-08-15

Work package K3: the handover mechanism gets a caller in every bridge (t9),
and the control plane learns to measure what was handed over (t10, #13).

### Added

- **Every bridge now creates the handover ref it was already able to
  create.** `preserve.handover_ref` shipped fully written and fully
  unit-tested in all three bridges with **no caller anywhere** — a
  verification dispatch measured it (`grep -rn "handover_ref(" adapters/*/
  src/*/ --include=*.py | grep -v preserve.py` returned 0, 0, 0) — so no
  dispatch in any backend had ever created one. Each bridge now reads
  `input.handover`, threads it through both terminal paths, and on a
  SUCCESSFUL dispatch creates the ref and reports it in the response body
  (sync) or the terminal event payload (async). The async half is the one
  production takes (`always_async`). New config: `handover_remote` (default
  `origin`), separate from `preserve_remote` because a handover only ever
  READS the remote's url — it never pushes.
- **Parity is on the ref creation, not the sandbox widening.** The codex
  bridge alone refuses `input.handover` without `sandbox=workspace-write`,
  because `writable_git` lowers a codex-specific
  `-c sandbox_workspace_write.writable_roots` flag; `claude -p` and
  `colleague` take no sandbox flag at all and can already write `.git`.
  Copying that 400 would refuse dispatches those bridges serve.
- **A handed-over ref is recorded as evidence the control plane measured
  itself** (#13, new `internal/handover`). It takes exactly one field from
  the agent's report — the ref NAME — fetches it from a remote the CONTROL
  PLANE is configured with, and records the ref, the commit sha the fetch
  resolved, and the paths that commit changed, as an `observed` ledger
  record. The agent's own commit sha and remote are decoded for
  round-tripping and read by nothing. Wired into both terminal paths
  (`internal/worker`, `internal/actors` callback ingest) and configured by
  `NODES_HANDOVER_REMOTE` + `NODES_HANDOVER_ACTOR_ID`; half-configuration
  refuses to start rather than silently recording nothing.
- **No fetchable ref means no record at all** — not a record marked
  unmeasured, not one citing the agent's summary. A ledger row that exists
  says a measurement happened, and there is no shape of `observed` record
  that honestly means "I could not look". The reason goes to the process's
  diagnostic stream. This closes the gap where `buildEvidence` could build
  observed evidence from a runner's answer while both shipped runners state
  they cannot answer, so no production run had ever carried one.

### Changed

- `docs/invariants.md` and the c17/h15 authority sweep now admit a second
  `AuthorityObserved` / `OriginRunner` writer, with the standing test
  restated: not which package a writer lives in, but whether every field it
  stamps came from its own measurement.

### The affirmative half of the authority model (task t30, #99)

A proposed claim can now be decided through the product, and the decision
names who decided and why.

#### Added

- `GET /v1alpha1/pending-decisions`: every proposed ledger record no review
  has decided, grouped by run, each group carrying the ledger version a
  review must be opened against. This is a join, not an authority filter —
  records are immutable, so confirming a claim appends a review record and
  leaves the claim reading `proposed` forever; "what is still undecided" was
  previously a question only a hand-maintained manifest
  (`docs/triage/cycle-runs.txt`) could answer. Filters: `run_id`,
  `record_type`, `actor_id`, `limit`; an unrecognised one is refused with
  400 rather than ignored.
- A **Decisions** view in the web front (`/decisions`): the undecided queue
  with each record's payload rendered in full, and a form that records the
  verdict, the reviewer and the rationale. Until now the affirmative half
  was reachable only by hand-writing two authenticated HTTP calls, which is
  not "through the product". Proven in a browser against a live control
  plane: an agent's claim confirmed, and the decision read back from the
  ledger as a human-origin `confirmed` review naming it.
- `rationale` on `POST /v1alpha1/reviews/{id}/commit`, required, recorded on
  every review record the commit appends (`schemas/ledger/review.schema.json`
  gains the field). `scripts/decide-claims.py` has demanded a `--why` since
  it was written but had nowhere to put it — it was printed to the operator's
  terminal and dropped. A confirmation with no stated reason cannot be told
  apart from an unread one.
- `nodes-op.sh pending [run-id]`, and `scripts/decide-claims.py --pending`:
  the same queue from a shell.

#### Fixed

- **An agent could decide its own claim.** `CommitReview` stamps the review
  records it appends with `Origin{Kind: human, ActorID: <reviewer>}`, so the
  producer/authority matrix — which correctly refuses an agent-origin
  `confirmed` record — saw a human no matter who was named, and nothing
  checked that the named reviewer was one. Passing an agent actor's id as
  `reviewer_actor_id` confirmed that agent's own claim, end to end, with no
  refusal anywhere. `ledger.CommitReview` now resolves the reviewer against
  the actor registry (new `ledger.Tx.ActorKind`) and refuses anything not
  registered `human` (`reviewer_must_be_human`, `ErrActorNotFound`). Every
  test fixture that decided as an agent-kind actor was fixed, not the check:
  three test files across the API and engine were doing exactly what this
  now refuses.
- The two review routes are gated by the decision bearer token, the same
  secret `POST /v1alpha1/human-tasks/{id}/decision` requires. All three
  write human-authority records into the ledger on whoever presents the
  token, which is the stated reason that one endpoint was gated; the review
  routes were the unauthenticated way to do the same thing.
- `POST /v1alpha1/runs/{id}/reviews` refuses a review with no
  `reviewer_actor_id` where it is created rather than two calls later at
  commit time.
- The Decisions view keeps its confirmation after the decided run leaves the
  queue. Found by driving the view against a live control plane: the card
  that made the decision is gone on the next refresh, so a confirmation
  rendered inside it vanished and the click looked like it had done nothing.

#### Changed

- `scripts/ledger-gate.py` asks the control plane which records are awaiting
  a decision instead of re-deriving the join client-side, so the gate and
  the decision surface cannot disagree about what "undecided" means. New
  `--all-runs` asks across the namespace with no cycle manifest at all.

## [0.24.0] - 2026-08-15

The bug tail of the `close-the-backlog` plan (task t12): #98, #95 + #105 as
one fix, #17, and #21 decided rather than patched.

### Fixed

- The workflow-scope boundary is enforced against what a session CHANGED,
  not against what its brief SAID (#98). The old guard grepped the
  instruction text for `.github/workflows/`, so a brief whose safest line
  was `Do NOT touch .github/workflows/**` was refused 403 before any model
  ran (live: run `01M039KA0QQ73XM3WQCQEQF1CN`), while a session that never
  mentioned CI could edit it freely. `scope_guard.py` now decides on the
  bridge's own measured change set — `workspace.measure()`'s `changed_files`
  plus a targeted `git status -uall` over the guarded prefixes, because git
  collapses a brand-new `.github/workflows/go.yml` into the entry
  `.github/`. A violation becomes a 403 (sync) or a `failed` terminal event
  (async) BEFORE the preserve hook, so refused work lands on a branch. Added
  to all three bridges per the all-backends rule; the broken guard existed
  in claude-code alone.
- `continue.while` conditions that could not be evaluated stopped looking
  like conditions that decided to stop (#105). An errored CEL evaluation, a
  non-boolean result and a cleanly false condition all returned the same
  zero `ContinuationDecision`, so `onExhausted` never fired and the run
  showed nothing. `Node.DecideContinuation` now returns
  `ErrContinuationUndecidable`, and the scheduler records a
  `dev.culture.nodes.continuation.undecidable` outbox event before failing
  closed.
- `node.state` is read from the node run rather than fabricated (#95).
  `deadlineContinuationHolds` passed the literal `"incomplete"`, which made
  the canonical `node.state == "incomplete"` true in every run for every
  node; it now reads `node_runs.status` in the same query as the bounds and
  maps it through `engine.ContinuationNodeState`. An unmeasured state is
  omitted from the CEL activation instead of defaulted, so it cannot be
  fabricated by omission either.
- `deploy/prod/deploy.sh` aborts when the `nodes-runner` binary fails to
  ship, and lands the binary by rename (#17). The failure was swallowed by
  `set -e`'s documented exemption for `&&` lists inside a `||` group, so a
  re-deploy restarted the unit on the previous build; the scp failed in the
  first place because it overwrote the running unit's own binary (ETXTBSY).

### Changed

- ADR 0013 records that the reference bridges stay single-threaded (#21),
  with measurements: the cancel handler costs 0.26 ms idle and 0.23 ms
  mid-session against a live async invocation, so the reported ~2.2 s is not
  in it. `adapters/codex/scripts/probe_cancel_latency.py` is committed so
  the numbers can be re-derived on any host.

## [0.23.0] - 2026-08-15

The `own-the-work-end-to-end` batch — eleven issues (76, 77, 78, 79, 80, 81,
82, 83, 84, 86, umbrella 87) taking a ticket across hosts: dispatch,
handoff, timeout, cancellation, verification, and a ledger that explains
itself. Full accounting, including the signals it did NOT meet, in
`docs/deliveries/2026-08-15-own-the-work-end-to-end.md`.

### Added

- Artifact publication: an authenticated, attempt-scoped
  `POST /v1alpha1/attempts/{attemptID}/artifacts` gives `internal/artifacts` its
  first production caller (#79). Namespace, run and attempt are derived from the
  durable invocation, never from the request body; the body is opaque and
  bounded at 64 MiB.
- Artifact retention: immutable tombstones, so following a reaped ref in a
  ledger record resolves to when and why it was reaped rather than a bare
  not-found. `Delete` fails closed; reaping goes through one guarded path.
- Continuation: `continue.while` / `bounds` / `onExhausted` compile, and the
  engine — not a model — evaluates the condition (#80). A declaration whose
  exhaustion no edge carries away is refused.
- A deadline now stops the actor session (#82). It consults the declared
  continuation first: holding pauses and **re-arms** the timer, absent or false
  cancels — after the timer transaction commits, off the tick loop.
- A deadline-origin timeout is no longer retried (#78); an unvouched origin
  fails closed. The refusal is visible as its own event.
- A late callback appends a superseding attempt row instead of vanishing, so
  reported work appears in the run's history without inflating retry burn.
- Attempt attribution end to end (#77): `usage`, `usage_model`,
  `termination_reason` and `continuation_ref` on the API and the run-detail
  page. All four backends now emit an explicit model sentinel rather than
  omitting the field — a null was indistinguishable from a field nobody wrote.
- The two-carrier handoff **contract** (#74): changes travel as a git ref,
  context as an artifact, declared once and proven to still refuse a bare
  filesystem path. The transport is not yet built; the summary says so.
- Bridge-side worktree minting with scoped-prefix allowlists, and a reaper that
  refuses dirty worktrees unconditionally and preserves unreferenced work
  before reclaiming it.
- Generate a workflow from plain text (#81) through one fleet-agent API from
  dashboard, CLI or mid-run, in front of the existing validate/publish door,
  with a lint guard that keeps model calls out of the control plane.
- Jira Cloud as a third pr-upkeep finding source (#76), fixture-backed; live
  proof is separately gated and still blocked on an empty backlog.
- `examples/development-loop/workflow.yaml` — the loop as a compiling graph
  that names seven of its own gaps rather than implying they work.
- A 1000-line file-length gate, with a test for the gate itself.

### Changed

- The capability surface probes bubblewrap executably instead of reading
  sysctls, which can advertise a sandbox mode the host cannot deliver (#83).
  Missing probe tools report `not-probed`, distinct from available.
- pr-upkeep sweeps a configured, ordered set of repositories instead of one
  pinned repo, and derives its source URL and digest from the revision the
  deploy actually ships (#86).
- Deploy installs the human-inbox units under the names spark runs, and removes
  the legacy files rather than only disabling them (#84).
- `AGENTS.md`: an agent may create a handover commit and a ref under
  `refs/culture-nodes/<run-id>`; pushing and committing onto a branch stay
  forbidden. The stale claim that `workspace-write` grants no writes is
  corrected.

### Fixed

- `DefaultTokenTTL`'s docstring described a re-issue that never happens, and
  that error had been carried into a spec question as a premise.
- A credential lint that fired on `git@github.com` — git's protocol username,
  not an account identity — in the documentation teaching the safe push command.

## [0.21.0] - 2026-08-14

### Added

- tests/test_dispatch.py — the nodes dispatch CLI surface (pending/show/confirm) had no tests: task t14 shipped it while its session was being cut off by the node deadline (#82). Covers the query it forwards, the briefing printed in full rather than digested, an absent note being an absent key rather than an empty string, the gates own refusal relayed verbatim, and byte-exact --json passthrough on all three verbs
- internal/api: a test that a HUMAN actor may still acknowledge on a bridges behalf, recorded with human origin and proposed authority. That path existed in code but was never covered, and the security fix above had to preserve it rather than close it

### Changed

- Five test functions refactored below the S3776 cognitive-complexity ceiling by lifting subtest bodies out of their loops and closures, plus one S8193 unnecessary variable declaration. Behaviour unchanged; each split is named for the property it asserts

### Fixed

- SECURITY: POST /v1alpha1/preflights/{id}/acknowledge bound an acknowledgement to no particular actor. Any registered agent could acknowledge any preflight, and because the worker consumes any acknowledged row, the gated dispatch then proceeded to an actor that never saw the briefing — which is the one thing the clarify-then-commit gate exists to prevent. The route is deliberately unauthenticated like the other ordinary routes, so this binding is the only integrity check available. An AGENT must now be the actor the preflight was issued for, matched on the rows resolved actor id when it has one so a re-registered actor reusing a key cannot answer for its predecessor. Reported by Qodo on PR #85

## [0.20.2] - 2026-08-14

### Added

- docs/deliveries/2026-08-14-upkeep-actors-jira.md — the delivery summary for this cycle (plan task t18). All eighteen plan tasks accounted for as delivered/partial/blocked, the approved d1 deviation quoted as the recorded ground truth, every delivery claim carrying a resolvable evidence pointer, and t17 reported partial rather than rounded up: pr-upkeep completions went 1 to 2 but items driven to a merged PR remain at 1 against a target of three, blocked by the artifact ingest gap in issue 79

## [0.20.1] - 2026-08-14

### Added

- tests/deploy/claudetokenplacement_test.go derives the hosts that must receive the claude actor token from the compose files that declare it, rather than from a hard-coded pair of names, so a third host is covered by construction

### Fixed

- install-secrets.sh relays the externally-issued NODES_ACTOR_CLAUDE_TOKEN to BOTH hosts, not thor alone. compose.orin.yml declares the variable for its worker, but the lane only ever called install_claude_actor_token "$THOR", so orin was left answering 401 policy_denied on every claude node dispatched to it with no deploy step saying so. Found by task t12s credential audit on its first live run and confirmed on both hosts before fixing: orins prod.env carries no NODES_ACTOR_CLAUDE_TOKEN while its running worker still holds one in memory, so the loss was invisible until the next container recreate. The neighbouring codex lanes already install to both hosts through update_actor_token_line for exactly this reason — either worker may dispatch either actor

## [0.20.0] - 2026-08-14

### Added

- `deploy/prod/audit-credentials.sh` — a post-deploy credential audit (task t12, issue #69 item 2). It compares the env keys this host's compose file declares against what `~/.culture-nodes/prod.env` actually contains, classifies every key as **required**, **optional** (absent by legitimate choice, closing a feature rather than breaking one) or **unknown** (in `prod.env`, declared by no compose file), and exits non-zero when a required key is missing or empty. `deploy.sh` runs it **last** on both the thor and orin lanes, so a deploy no longer ends without ever checking that the environment it shipped is complete
- The detector half of the incident t11 fixed the cause of: a `FORCE` rotation destroyed `NODES_ACTOR_CLAUDE_TOKEN` and nothing reported it for ~18 hours, because the running worker held the token in memory until its next restart. Merging stopped that mechanism; this catches whatever removes a key next
- The declared set is **read** from `compose.thor.yml` and `compose.orin.yml`, never from a list that could drift (`$${VAR}`, compose's escape for the container's own shell, is correctly ignored). Compose decides most of the classification itself — `${KEY:?}` is required by construction, `${KEY:-value}` works without the key by construction — so the hand-classified half covers only the keys compose leaves open (`${KEY:-}`, the shape every credential has), in one place with a comment per entry saying why it is where it is. An unclassified declared key is reported and treated as required until someone writes down which it is
- Unknown keys are reported and **left untouched**: `prod.env` legitimately carries keys compose never mentions (`NODES_RUNNER_SECRET` on both hosts today), and `remove-secret.sh` remains the deliberate removal path
- Key names only. The remote command emits `KEY<TAB>set|empty`, so no credential value is printed, logged, or placed in an argv. `tests/deploy/credentialaudit_test.go` runs the real script against a stub `ssh` under a per-host `HOME` — including a fixture missing one required key — rather than asserting on the script's source text

## [0.19.0] - 2026-08-14

### Added

- Invariant 4 — every committed example is portable: `tests/lint/exampleportability_test.go` refuses a graph value that names a hostname, address, absolute host path or URL, requires every `actor://`/`runner://` id and `environmentRefs` name to be documented in that file's own `Deployment configuration` block, and forbids a URL in any code operation's argv (task t16)
- A `Deployment configuration` block in all eleven example workflows, naming every value that resolves outside the document and where it comes from
- `tests/test_pr_upkeep_sweep.py::TestTheSweptRepoIsPinnedAndSaysSo` — the swept repo's pin is real (plain literals, an exact and repo-free set of environment reads) and documented at the constant and in the README
- `deploy/prod/deploy.sh` re-grants `PR_UPKEEP_SWEEP_SOURCE_URL`/`PR_UPKEEP_SWEEP_SOURCE_SHA256` to the runner env when the deploying operator sets them
- The preflight capability surface on all four bridges (task t15, issue #67, the all-backends rule): `claude-code`, `codex`, `colleague` and `notify` each measure the host they dispatch on and advertise it through the SAME protocol shape task t14 built engine-side. The facts are the ones a dispatched task actually depends on — which sandbox modes truly work here, what the commit/harvest policy in force is, and which paths the bridge allowlists
- `preflight.py`, one module carrying the protocol, the agreed `host` key set and the measurement helpers, **byte-identical in all four bridges**. Everything backend-specific — and only that — lives in each bridge's own `capabilities.py`. The split is not a convention: `tests/lint/preflightsurface_test.go` fails the build if the shared module diverges between bridges, if a bridge implements half the surface, or if a `capabilities.py` starts re-declaring the document's shape. A per-bridge protocol is the duplication that let `resolve_actor_row_id` ship as the same bug in three deploy lanes
- Measured, not nominal: `sandbox_modes_unavailable` reports a mode this host cannot actually deliver, with the reason. codex's `read-only`/`workspace-write` confinement rests on a bubblewrap helper backed by unprivileged user namespaces, so where the kernel restricts them (#18/#63 — requested on three hosts, every file write silently lost, shell commands still running unconfined) the surface says so instead of echoing the config. claude-code reports `plan`/`default` as unavailable because headless dispatch allocates no TTY
- A `confinement` fact on every advertising bridge, stating plainly what actually confines a session — including "nothing", which is the truth for claude-code (`--permission-mode` governs asking, not reaching) and colleague (a throwaway worktree bounds where changes land, not what is reachable)
- `GET /v1/capabilities` on each advertising bridge (`internal/actors.CapabilitiesPath`), authenticated like the invocation route, plus `--print-capabilities` for registering an actor before its bridge has ever started. Neither is on the engine's dispatch path: the surface reaches the control plane through the actor's registration, and these exist so an operator reads the facts off the host that measured them
- `tests/conformance` grew an optional capability-surface check: an actor that advertises must advertise the document `internal/preflight.ParseSurface` accepts, and one that advertises nothing SKIPS rather than fails. `TestAnActorThatAdvertisesNoCapabilitySurfaceIsStillConformant` runs the whole kit against a muted reference actor, so the task's second acceptance criterion — a bridge that does not advertise leaves its actor dispatching exactly as before — is a tested property rather than a promise. `adapters/human-inbox` is the live subject: it advertises nothing and is unchanged

### Changed

- examples/pr-upkeep's sweep node no longer fetches its script from a raw.githubusercontent URL pinned to one org and commit; the source and its expected sha256 are granted environment values the deployment supplies, and the bootstrap refuses bytes whose digest does not match (task t16). The 0/10/other exit-code contract is unchanged
- examples/codex-smoke-pair renames node ids `codex-thor`/`codex-orin` to `codex-first`/`codex-second` and run inputs `thor_repo`/`orin_repo` to `first_repo`/`second_repo`; actor placement stays on the registered `company/codex-thor`/`company/codex-orin` ids. `run-smoke.sh`'s `THOR_REPO`/`ORIN_REPO` become `FIRST_REPO`/`SECOND_REPO`
- `tests/deploy/codexsmoke_test.go` asserts node id and actor id separately instead of deriving one from the other
- `api/actor-protocol/README.md` documents the agreed `host` keys in a table, the measured-not-nominal rule with the #18/#63 evidence behind it, and where a bridge's surface comes from; each advertising bridge's README states the facts THAT backend contributes and why

## [0.18.2] - 2026-08-14

### Added

- codex-preflight.sh check 7: the host can create an unprivileged user namespace (issue #63). Codex sandboxes every shell command it runs inside a user namespace, so a host that cannot build one gets an actor that registers, dispatches, accepts work, and then fails every command it tries — after the turn is spent, and surfacing as a bridge or runner fault that it is not. The bridge unit already runs the preflight as ExecStartPre, so a host in that state now fails to start its bridge instead of accepting work it cannot do. Probed by capability (bwrap, falling back to unshare), never by reading the sysctl back: the value says what was configured, the probe says what works. When neither tool is installed the script says the capability was NOT probed rather than reporting a readiness it never established
- deploy/prod/README.md gains an "Unprivileged user namespaces" section — the provisioning step issue #63 asked to be written down. It states the chosen option (kernel.apparmor_restrict_unprivileged_userns=0, persisted in /etc/sysctl.d/60-culture-nodes-userns.conf on spark, thor and orin), its real cost (pre-24.04 behaviour for every local process, a kernel surface that has carried local-root CVEs), the better option left open (a scoped AppArmor profile granting userns to bwrap alone), and the rejected one (disabling the codex sandbox to work around a sandbox bug)

## [0.18.1] - 2026-08-14

### Fixed

- Credential rotation MERGES instead of replacing (task t11, issue #69 item 1): `deploy/prod/install-secrets.sh`'s prod lane was the one place that wrote `~/.culture-nodes/prod.env` wholesale (`cat >` from its generated block), while the file holds two populations — the six secrets the script generates, and roughly eight more that accrete afterwards (`NODES_NAMESPACE_ID` and `THOR_IP` from `deploy.sh`, `NODES_ACTOR_CODEX_*_TOKEN` / `NODES_ACTOR_NOTIFY_TOKEN` from the script's own later lanes, `NODES_ACTOR_CLAUDE_TOKEN` and `DISCORD_WEBHOOK_URL` relayed from outside). An authorized rotation deleted the second population silently. This is observed, not theorised: a `FORCE=1` rotation destroyed `NODES_ACTOR_CLAUDE_TOKEN` and nothing reported it for ~18 hours, because the running worker held the token in memory until its next restart (`company/developer` succeeded at 13:03, then answered `policy_denied` / 401 at 06:42 the next morning)
- The prod lane now reuses the key-by-key merge idiom the file's own single-key helpers already used — replace the key's line if present, append it otherwise — rather than a second mechanism that would have to be kept in agreement with the first by hand. It is hoisted into one `PROD_ENV_MERGE` definition all three prod.env-writing lanes reference, because those pasted copies had already drifted: only one of them held a guard for a file whose last line has no trailing newline, so appending a key to a hand-edited prod.env concatenated the new assignment onto the previous value and destroyed it. Same failure class as the wholesale rewrite, found by the test written for it. The FORCE_PROD guard and its windowed destructive-confirmation protocol are unchanged — merging is not permission to rotate
- Stale `FORCE=1` references in `deploy/prod/README.md` now name the per-lane switch that actually exists (`FORCE_PROD`, `FORCE_CODEX`); the old text would have had an operator authorize a rotation with a variable no lane reads

### Added

- `deploy/prod/remove-secret.sh`, the explicit removal path merge-only makes necessary (spec claim c58): merging alone would trade a silent-destruction bug for a file that can only ever grow, leaving a rotated-away actor token indistinguishable from a live one. It removes ONE named key from `~/.culture-nodes/prod.env` (or another `ENV_FILE` there) on the named hosts, is a dry run until `--yes`, prints the line it would drop with the value redacted, refuses any key name outside `[A-Za-z_][A-Za-z0-9_]*` so a pattern cannot delete lines nobody named, and writes no backup — a `.bak` beside `prod.env` would be a second unmanaged copy of live credentials
- `tests/deploy/prodenvmerge_test.go`: behavioral, not textual. It runs `install-secrets.sh` for real against a stub `ssh` that executes each remote command under a per-host `HOME`, so the merge is proven by what the file contains afterwards — a confirmed rotation performed with an externally-issued `NODES_ACTOR_CLAUDE_TOKEN` present finds it still there (honesty condition h32), a key added through the relay lane and then removed through `remove-secret.sh` is gone with its neighbours intact (h37), a fresh host still gets the whole generated block, and an unforced re-run still changes nothing

## [0.18.0] - 2026-08-14

### Added

- Cross-machine handoff contract on `examples/pr-upkeep/workflow.yaml` (task t13, issue #74): `fix.completed` now requires a portable `handoff: {kind: artifact, ref: "artifact://<namespace>/<id>"}` handle whose ref is pattern-constrained, so it cannot silently become a filesystem path again. The engine's own outcome-schema validation (`internal/engine/complete.go`'s `checkOutput`) is what enforces it — a fix that produced no handle cannot report `completed`
- `fix.handoff_unavailable`, the named honest failure: a fix host that cannot publish its work reports a domain outcome carrying `missing_capability` from a closed set (`artifact_publish`, `workspace_export`, `handoff_too_large`) instead of letting the run die as an HTTP 403 on the review host, where the error names authorization and the cause is topology
- `handoff-blocked` terminal node, which `fix.handoff_unavailable` routes to. It carries the fix node's output (where `missing_capability` lives) as the run's output rather than `finish`'s sweep report, which would bury the one fact that explains the stop
- `tests/lint/crosshosthandoff_test.go` locks all four invariants against the committed document: the required artifact-shaped handle, the closed capability enum, that `handoff_unavailable` reaches only a terminal node and never `review`, and that `review` binds and requires the handle rather than sharing the fix actor's repo pointer
- The clarify-then-commit gate, engine side (task t14, issue #67): a dispatched actor is briefed BEFORE its first billable turn, and a second, separate action commits the dispatch. It generalizes `deploy/prod/install-secrets.sh`'s single-use windowed destructive-confirmation protocol from danger to understanding, keeping every property that made it work — the composed briefing holds (`verdict: hold`), it states what does not proceed, the acknowledgement is single-use, and it expires
- Two additively-registered ledger record types with their schemas: `dispatch_preflight` (`derived` — a deterministic composition of the host capabilities a bridge advertised and the pinned task declaration, refused at any other authority by the new `preflight_derived_only` rule) and `dispatch_acknowledgement` (`proposed` by the actor, never derived under any origin — `acknowledgement_never_derived`, so an engine cannot clear its own gate). An acknowledgement names the briefing by id AND content digest
- `internal/preflight`: the protocol in one place — the capability surface a bridge advertises (`capabilities.preflight`), the per-actor gate configuration (`metadata.preflight_gate`), the deterministic document composer, and the two record builders both the dispatch site and the confirm verb use. Protocol in the engine, facts from the bridges: a per-bridge protocol was rejected as four implementations of one contract
- Migration `0026_dispatch_preflights.sql` (expand-only): the `dispatch_preflights` table, where single-use becomes a transactional fact that immutable ledger records cannot express, plus the `actors_preflight_gate_requires_surface` CHECK constraint. An N-1 binary ignores both safely
- `GET /v1alpha1/preflights`, `GET /v1alpha1/preflights/{id}` and `POST /v1alpha1/preflights/{id}/acknowledge`, with the `nodes dispatch pending|show|confirm` verbs over them. `show` prints the briefing in full: a confirm without a show is a keystroke, not an acknowledgement
- `preflight_unacknowledged`, a second reserved refusal outcome beside `budget_exhausted` (issue #67's fourth open question, answered yes): a dispatch whose window closes unacknowledged is REFUSED rather than deferred forever, and a workflow author may declare an edge from it

### Changed

- `review` reads the work under review through `handoff: /nodes/fix/output/handoff` and requires it in its input contract; its `review_repo` is now documented as only the working directory thor's codex bridge allowlists, never the source of the work under review
- `examples/pr-upkeep/driver.sh` states the handoff contract in the fix and review instructions — the fix session must declare its own result JSON (a default envelope carries no handle and would be contract_rejected) and is given the named way out rather than left to improvise a fabricated ref
- `examples/pr-upkeep/README.md` documents the cross-machine handoff, the live 403 in run 01KZZSGSWH11J7R7P4V2HPTZZQ that motivated it, why a git ref is not available on the host that must produce the handle, and — plainly — that the artifact content path is not wired yet
- Enabling the gate for an actor that advertises no capability surface is refused at CONFIGURATION time at all three doors — `POST /v1alpha1/actors` (400 with a remediation), `RegisterActor`, and raw SQL (the migration's CHECK). The gate is per-actor and DEFAULT-OFF: an actor whose registration says nothing about it, or a registry that cannot answer the question at all, dispatches exactly as before
- `internal/invariants` deliberately extends two allowlists, recorded in `docs/invariants.md`: `internal/api/preflights.go` reads an actor's registered kind to decide an acknowledgement's ledger ORIGIN (the grades-API precedent, outside dispatch), and `internal/preflight/records.go` writes engine-origin `derived` authority for the briefing composition

## [0.17.0] - 2026-08-14

### Added

- Spec and plan for the upkeep-actors-jira cycle: nine work items across the four pr-upkeep upkeep bugs (#71-#74), the notifier and sweep fixes (#61, #66), the clarify-then-commit gate (#67), credential-rotation safety (#69 items 1-2), and demo portability — scoped, challenged and planned through the devague chain
- CI gate that compiles every workflow under `examples/` (task t5, issue #73's recurrence half): `scripts/validate-examples.sh` runs `nodes validate` over each `examples/**/*.yaml` with no control plane, wired as the `examples compile` job in `.github/workflows/go.yml`; `tests/lint/examplescompile_test.go` mirrors the compile check in-process and additionally locks the job's wiring — that it exists, needs no database, and is triggered by a change under `examples/`
- Issue #76: the Jira Cloud node-loop, scoped in full (auth shape verified live, rate budget measured, boundaries and portability settled, empty-backlog blocker on live proof recorded) and deliberately deferred out of this batch
- Typed literal bindings (task t6, issue #73's option A): a node's `bindings` value may now be a JSON Pointer string OR a declared `literal:`, accepted end to end by `schemas/workflow/workflow.schema.json` (`#/$defs/bindingValue`), the compiler, the engine and the worker. A bare string is always a pointer and a literal is always wrapped, so the two can never be confused — this is not a template language, and a literal interpolates nothing. The compiler validates each literal against the node's own `contract.input` schema, so a value the node refuses is a publish-time error (`contract.literal_invalid`) rather than a first-dispatch surprise
- Literal bindings render in the web inbox's context refs as the declared value rather than a pointer, which is the point of the shape: a reader of the task can name what it observes

### Changed

- `examples/pr-upkeep/workflow.yaml` declares both observables in the graph text again (task t6): `human-merges-pr` and `human-answers-review` bind `observe: {literal: {kind: ...}}` beside `pr: /nodes/fix/output/pr_number`, and the `merge_observe`/`reply_observe` run-input properties they used to ride on are gone. The observation kind is a declaration and never changes; which PR is per-cycle data, so the two are declared separately rather than smuggling a pointer inside a literal
- The human-inbox tracker reads an observation's `pr` from the `observe` block when it is there and from the task's own input otherwise — the same fallback it has always applied to `repo`. A value that is not a positive integer is still malformed wherever it came from, and a task with no PR number from either source still stays on the manual lane
- Credential custody extended to recorded test fixtures: examples/pr-upkeep/fixtures/sonarcloud-issues.json and two test suites carried real account identities in committed files and now use neutral placeholders
- ssh probe targets in devague frames and exported specs are recorded as the generic user@host form rather than named accounts
- Design-system provenance is cited by project name rather than by domain in README, ADR-0001, guide, CHANGELOG and web/src/main.tsx

### Fixed

- The human-inbox bridge and its merge tracker now deploy to the host serving `company/human-ops` instead of a hardcoded thor (task t10, issue #72). `deploy/prod/actor-placement.sh` — shared by `deploy.sh` and `install-secrets.sh` — resolves the actor's newest registration and takes the deploy host, the bridge port and the `actors(id)` the bridge stamps as `origin.actor_id` from that one read, so they cannot come from different revisions. The engine dispatches to the registered endpoint, so a declared host was always a second value that had to agree with it by luck: tasks parked on the bridge at the registration while the tracker on the declared host watched an empty state directory and logged `pending=0`. `assert_human_inbox_colocated` now refuses the deploy, before either unit is installed, when the host does not answer on the registered address, the bridge port is not the registered one, the tracker points at another bridge or state directory, the bridge's and tracker's `HUMAN_INBOX_BRIDGE_ACTOR_ID` are swapped (one needs the row id for the ledger foreign key, the other the actor key for its own lookup), or the tracker's startup identity check is left disarmed. Both `THOR ONLY` unit-file comments are gone, and the placement library runs locally rather than over ssh when the resolved address is the deploying machine's own — which it is today, and which ssh-to-self would not have reached. Task t8's startup refusal is the runtime half of the same invariant; `HUMAN_INBOX_TRACKER_CONTROL_PLANE_URL` is now written so that half is actually armed
- Ten economy-discord-graphs issues that PR #70 delivered but left open (its body used a prose Closes form GitHub does not parse) are now closed with per-issue evidence citations; #54, #48 and #66 stay open with written reasons

## [0.16.1] - 2026-08-14

### Removed

- The `python:S3516` SonarCloud exclusion added in 0.16.0. Excluding a rule
  from the quality gate is a project-policy decision, and it was made
  unilaterally rather than proposed — the operator had observed that three
  issues remained, which is not the same as authorising a gate change. The
  three findings return to open so the decision can be made explicitly.
  The reasoning behind the exclusion still stands and is preserved in the
  0.16.0 history; only the unilateral application of it is withdrawn.

## [0.16.0] - 2026-08-14

### Added

- Session resume across all three bridges: claude via `--resume`, codex via
  `exec resume`, colleague via `work --continue` (deviation d1). `session_key`
  and `continuation_ref` are transport keys and never reach prompt text.
- Session-key serialization: exactly one in-flight invocation per key. A
  collision forks rather than queues and says so — `X-Session-Fork` header, a
  registry event and a log line — so a spent session is never inferred.
- Preserve-on-failure: a failed node's work is committed to a branch through
  git plumbing against a scratch index, leaving HEAD, the index and the
  working tree untouched. Surfaced on the attempt row (migration 0025), the
  API and the run detail page, with pushed and local-only visibly different.
- Plan ingestion: `MapPlanShow` (real dependency edges, not the lossy waves
  view) and `MapDeviations` (origin user vs llm), a durable store (migration
  0024), an import API and CLI verb, and the Implement-Plan dashboard view.
- A domain-agnostic claim-chain verifier and `nodes chain-verify`, proving the
  decompose pipeline generalizes beyond code.
- The notify actor (#68): a workflow can send a message as a declared step,
  with the webhook URL held outside the control plane and a CI gate proving it.
- `nodes doctor` reports whether this host can start a bubblewrap sandbox at
  all, so a dispatched actor learns it before spending a session.

### Changed

- The merge tracker's `GITHUB_TOKEN` is optional; the anonymous lane plans for
  half of GitHub's per-IP ceiling rather than all of it.
- Bridges classify provider quota and rate-limit failures as
  `capacity_exhausted`, carrying Retry-After.

### Fixed

- Deploy installed human-inbox units that ran from the codex agent checkout —
  a directory pinned to main — so the tracker crash-looped 6272 times over
  nine hours while merge-as-action was silently dead.
- Bridges reported an actor *key* where `ledger_records.origin_actor_id`
  requires an actor *row id*, rolling back every terminal commit.
- A deploy no longer reports success over a unit that is restarting in a loop.

### Notes

- Session stickiness stays **opt-in**: two live A/B runs measured 0.0%
  uncached-input reduction. The mechanism works and was verified to resume;
  the benefit did not appear in the metric. See
  `docs/deliveries/2026-08-14-t7-stickiness-ab.md`.

## [0.15.0] - 2026-08-13

### Added

- Extended attempt usage telemetry (migration 0017): cached-input, reasoning, model, thread and termination-reason fields carried protocol → engine → store → API → dashboard, with a cache-hit-rate tile in Statistics (ADR 0009).
- `continuation_ref` carriage (migration 0018): the field lands on the invocation request and the completed payload, persists per attempt, and dispatch offers the prior ref back to the same actor within a run (ADR 0010).
- Parallel tokens in full (migration 0019, issue #43): `parallel`/`join` node kinds, set-valued transition plans, token groups with race-free join-barrier counting, per-token bounds, sibling reaping with branch-cancel propagation, and `maxParallelTokens` finally honored.
- `capacity_exhausted` error class (§13.5): body-declared, non-retryable in-attempt, with Retry-After surviving onto the terminal error.
- `internal/notify` + `cmd/nodes-notifier`: a Discord webhook layer ported from devex's design and an out-of-process SSE consumer with a durable cursor, exactly-once delivery across restarts, and zero control-plane changes.
- Human-inbox merge tracker (issue #54): declared observables ride the task input, a stdlib GitHub poller auto-submits only the merged state as an `observed-submission` claim naming the merge commit, and manual submission stays the override lane.
- Node Graphs tab (issue #56) replacing Workflows: Nodes / Node Graphs / Active Graphs sub-tabs, a cross-workflow node catalog derived from published IRs, and Active Graphs with a breathing halo and committed-event pulses (reduced-motion safe, palette-pinned).
- Dashboard auto-refresh (issue #46): one shared app-wide event stream with per-view subscriptions, stale-while-revalidate preserved across every view.
- Deploy-lane wiring for the notifier and the human-inbox bridge + tracker on thor, with secret plumbing through `install-secrets.sh`.

### Fixed

- Bridge usage honesty across all three backends: cache/reasoning/model/thread telemetry is mapped where the backend reports it, and a codex turn that reports no usage now emits no usage block at all — fabricated `0/0` usage on failed turns is structurally impossible.
- Inbox no longer regresses agent-state to `loading` on a decision-driven reload.
- OpenAPI node-run status enums were missing `waiting_human` (pre-existing drift) alongside the new `waiting_join`.

## [0.14.1] - 2026-08-13

### Added

- Spec cycle economy-discord-graphs (issues #41 #43 #45 #46 #47 #48 #49 #50 #54 #56): converged, challenged, and exported frame — 49 confirmed claims, 42 honesty conditions, 35 scope entries with file-level provenance, 8 resolved questions, 3 parks. Covers Discord run updates (devex webhook port over the cross-run SSE feed), cache/usage telemetry, session stickiness gated on a cold-vs-resumed A/B, capacity circuit breaker, reset-clock-aware pacing, budget contracts, full parallel tokens (#43 split/join + event pickup), the generic document-to-claims pipeline reframing of implement-plan mode, merge-as-action human tasks, preserve-on-failure plumbing commits to DB-recorded branches, dashboard auto-refresh, and the Node Graphs tab. Spec at docs/specs/2026-08-13-economy-discord-graphs.md.

## [0.14.0] - 2026-08-13

### Added

- The attempts-evidence-humans-loops batch (issues #32 #33 #34 #36 #37 #38 #39 #40): failed-attempt attribution and usage, evidence bindings (run-wide and per-node), workspace_measured landing, timer+signal wait surface (migration 0016), human actors via the section-13 protocol (adapters/human-inbox, web /inbox, registration API), routable acceptance + success-signal evaluator, first-class ad-hoc runs (API + nodes run), invariant gates, and the pr-upkeep example that cleared the repo entire standing SonarCloud debt live (PRs 51/53/55/57, verified by re-analysis). Delivery summary in docs/deliveries/.

### Changed

- Bridges (all three) forward engine-resolved bindings into sessions and honor session-declared {outcome, output} final messages (deviations d3/d4).

### Fixed

- SonarCloud: S3516 blocker, both S3776 criticals, S8193 — fixed through the product itself.

## [0.13.6] - 2026-08-13

### Fixed

- `runRegisterActor` uses the `asExitError(err, &exitErr)` call directly as
  the `if` condition instead of declaring a throwaway `ok` variable
  (SonarCloud MINOR godre:S8193, `tests/deploy/registeractor_test.go:187`).
  Behaviour-identical; PR-upkeep sweep sole item
  (run 01KZXFYJR1Y6KHCZHT843PTMEG).

## [0.13.5] - 2026-08-13

### Fixed

- `TestCodexWorkerEnvInProdCompose` refactored below the cognitive-complexity
  ceiling (SonarCloud CRITICAL go:S3776, 20 > 15): the per-compose-file
  read/parse/assert body moved out of the nested `t.Run` closure into a
  top-level `assertWorkerEnvHasKeys` `t.Helper()`, same shape as the 0.13.4
  codexsmoke fix. Behaviour-identical; PR-upkeep sweep item 1 of 2
  (run 01KZXFFDZC7NF56HSS6DSWY5XN).

## [0.13.4] - 2026-08-13

### Fixed

- `TestCodexSmokePairHasTwoCodexNodesOnEntryChain` refactored below the
  cognitive-complexity ceiling (SonarCloud CRITICAL go:S3776, 19 > 15): the
  duplicated per-node existence/kind/uses assertions moved into a shared
  `assertCodexAgentNode` helper and the edge scan into `hasEdge`, on named
  `smokeIRNode`/`smokeIREdge` IR types. Behaviour-identical; PR-upkeep sweep
  item 1 of 3 (run 01KZXD609QRFHWS8YQ6MRZ1Y0F).

## [0.13.3] - 2026-08-13

### Fixed

- `nodes node-runs list` handler no longer returns an invariant `0` from every
  path (SonarCloud BLOCKER python:S3516): it is now a `-> None` procedure on
  the dispatcher's None-means-success contract, matching `whoami`. PR-upkeep
  sweep item 1 of 4 (run 01KZXATS1HM63SAQZVHX0K4ZD0).

## [0.13.2] - 2026-08-13

### Added

- Plan: attempts-evidence-humans-loops (devague /spec-to-plan) — 23 confirmed TDD-gated tasks in 6 dependency waves covering all 48 spec targets, with file-precise operator instructions, 5 recorded risks, and the PR-upkeep live run as the delivery gate

## [0.13.1] - 2026-08-13

### Added

- Spec: attempts-evidence-humans-loops (devague /scope + /think + /challenge) —
  converged, challenge-passed frame for the issue 32/33/34/36/37/38/39/40 batch
  plus the PR-upkeep live-test loop; 33 confirmed claims with instructions,
  24 confirmed honesty conditions, 20 provenance-linked scope entries (14 from
  scoping plus 6 challenge lenses incl. a live prod-actors probe), 5 decisions
  (human nodes via actor protocol, routable acceptance, external loop driver,
  spark claude-code bridge as fix executor, human performs the merge), 4 parks.
  MD033 placeholder allowance (`<id>`/`<node>`) for devague-exported specs.

## [0.13.0] - 2026-08-13

### Added

- §13.2 usage persisted at the completion seam (sync + async callback) via
  expand-only migration 0012, rolled up attempt → node run → run and exposed
  on run detail + node-runs listing with honest semantics: failed attempts
  count (retry burn), not-reported is never zero, cost never summed across
  currencies (spec t1/t2, closes #12 item 3's API half)
- Run metadata: optional name/description/category at creation (migration
  0013), category retag via PATCH, and a derived display hint that is never
  presented as a given name (spec t3, #12 item 4 + #28 item 3)
- Web: Workflows view over the existing endpoints (spec t8, #12 item 5) and
  the in-UI authoring slice — paste/upload YAML → compiler diagnostics →
  read-only graph preview → byte-identical publish (spec t9, #12 item 6),
  gated by ADR 0007 (Phase-3 timing deviation + unauthenticated LAN-bound
  exposure, #6 the gate)
- Bridge-measured workspace facts in ALL three adapters: workspace_measured
  block (HEAD before/after, status, changed files, diffstat, branch) measured
  by the bridge process from git, structurally separate from model-claimed
  output, honest degradation including mid-session workspace loss (spec t10 +
  colleague-review fix, #13 item 1)
- Workspace-snapshot hook evidence: hooks request runner-boundary snapshots
  and measured changed-paths/diff-digest/artifact-refs surface as observed
  evidence appended by the worker, never via the agent's delta; async post_run
  refusal regression-locked (spec t12, #13 item 2 sync half)
- Ledger record type `grade`: rating+rationale against an evaluated actor,
  agent grades land proposed, self-grades refused, never observed/derived
  (spec t14, #28 item 1's ledger half)
- examples/independent-review: a different backend reviews the builder's
  change-set; verdicts land proposed per §10.4 (spec t13, #13 item 3 —
  binding gaps recorded as #33/#34)
- OpenTelemetry beyond the stub: engine transition commit, worker dispatch,
  actor callback (+ scheduler's engine seam) traced/metered behind env-gated
  OTLP export with a structurally enforced attribute allowlist — ids, states,
  counts, durations only (spec t19, closes #5)
- CLI fronts for the new surfaces: `nodes run create --name/--description/
  --category`, `run retag`, `run grade`, usage rendering in run/node-runs
  views, explain-catalog entries, `nodes-op assign --category` + `grade`
  verbs (spec t4/t16)
- Web: run names/derived hints/category chips + token-first cost across
  RunsList/Board/Jobs/RunView/NodeDetailPanel — currency only when reported,
  'not reported' never rendered as zero (spec t5)
- Actors API family: `GET /v1alpha1/actors`, `/{id}`, `/{id}/stats` — per-
  category buckets (uncategorized its own bucket), runs by status+outcome
  kept separate, retry burn, duration percentiles, usage with honest currency
  semantics, grades proposed-vs-confirmed never blended (spec t15, #28
  item 2's API; registration stays #8)
- Grade API: `POST /v1alpha1/runs/{id}/grades` — origin from the grading
  actor's registered kind, authority via the ledger rules (agent→proposed,
  human→confirmed, self-grade refused), plus the review-surface confirm loop
  proven end-to-end (spec t16, completes #28 item 1)
- Web: Statistics tab — window cost totals, average and median per run,
  per-category breakdown, denominator stated with excluded-as-not-reported
  runs visible (spec t6, the operator's stats ask)
- Web: measured evidence rendered in the ship-review pause — changed paths,
  snapshot digest, artifact refs in NodeDetailPanel, evidence markers on the
  run canvas/table (spec t11, #13 item 4)
- Cross-run events surface: `GET /v1alpha1/events` — SSE across active runs
  plus run-lifecycle events, bounded polling with documented catch-up, honest
  ULID-cursor resume semantics, expand-only index migration 0014 (spec t17)
- Web: the live-mesh overview (`/mesh`) — the control plane breathing at the
  center, actors orbiting kind-differentiated, active runs as embers, every
  committed event a particle on its edge, completion rings, honest
  LIVE/RECONNECTING indicator, prefers-reduced-motion static frame,
  agent-state mirrored for webglass (spec t18, the cycle's showpiece)

### Fixed

- POST /v1alpha1/runs unknown-success window: run metadata now rides
  CreateRun's own transaction (engine.WithRunMetadata) and the create
  response uses the deterministic empty rollup — no post-commit failure can
  5xx a run that exists (qodo PR-35 finding)
- Bridge workspace measurement hardened: `--no-ext-diff --no-textconv` on
  all diff invocations in all three adapters, with malicious-repo-config
  tests — a measured repo can no longer execute commands on the bridge host
  (qodo PR-35 finding); mid-session workspace loss degrades to the honest
  unmeasured shape (colleague review finding)
- API error classification: contracts schema-validation failures from ledger
  appends render as 400s with pointer paths, not 500s (spec t16)
- colleague adapter: t10 lint leftovers (unused import, long lines)

## [0.12.2] - 2026-08-12

### Added

- Converged spec `docs/specs/2026-08-12-operate-through-the-ui.md` (devague
  /scope → /think over issues #12 items 3–6, #13, #28, #5): run cost
  aggregation + Statistics tab, run names/categories, workflows view, in-UI
  authoring slice (ADR-gated against PRD Phase 3), bridge-measured workspace
  evidence + in-page diff review, independent LLM review node, `grade` as a
  new ledger record type, per-actor analytics (introduces the actors API),
  live-mesh overview view, and OpenTelemetry beyond the stub. Decisions
  recorded: async attempts carry bridge-measured facts this cycle (hook
  evidence stays sync-only, callback-path extension parked as follow-up);
  a grade is a first-class record type, never a review extension

## [0.12.1] - 2026-08-12

### Added

- Nodes dogfooding reflex in CLAUDE.md and AGENTS.colleague.md: delegable scoped work goes through `nodes-operator assign` to the actor fleet (analysis-only until #18), and every assigned run's outcome is assessed — run + ledger read, claims decided through the approval surface, actor-quality note remembered — building the comparative which-actor-is-better-at-what record; first-class grading/analytics tracked as issue #28

## [0.12.0] - 2026-08-12

### Added

- AWS lane opened (issue #25, ADR 0006): `cmd/nodes-runner-lambda` — the minimal, honest function side of the runner contract (executes an operation's argv, reports process-reported exit facts, refuses workspaces/environment refs/shell requests it cannot honour) — with `deploy/aws/lambda-runner.Dockerfile` building the `culture-nodes/runner` ECR image
- Live `awslive` suite for the SQS driver (`internal/queue/sqs/awslive_test.go`): publish/receive/ack round-trip and delay-withholds-redelivery proven against the real `culture-nodes-awslive` queue
- ADR 0006 records the #7/#25 decisions: SQS stays the optional cloud-profile signal driver, the Lambda adapter stays first-class in-worker (refold trigger: first real cloud target), the awslive lane is manual like the codex smoke (CI never runs it), ECS/Fargate stays deferred, credential chain unchanged
- deploy/aws/README.md: the live-lane arming recipe and the standing-resource (re)creation runbook (queue, ECR repo, function, exec + worker roles)

## [0.11.1] - 2026-08-12

### Added

- Web: favicon — the AgentCulture mark, copied verbatim from the org site at
  the ADR 0001 pinned commit into `web/public/favicon.svg`, linked from
  `index.html`; Vite copies it into `dist/` so the `-tags embedweb` Go build
  serves it too (issue #12 item 1)
- Web: Board and Runs carry the Jobs view's time-range filter — the same
  `TimeRangeFilter` control and URL-search-param state idiom (new shared
  `useTimeRange` hook), the same "newest first by last update" ordering
  statement, server-side `updated_since`/`updated_until` scoping, no
  client-side re-slicing (issue #23); the Runs table gains an `updated`
  column and sorts by `updated_at`

### Changed

- Web: full-width + responsive layout (issue #12 item 2) — views move from
  the org site's 68rem prose `.container` to a full-width `.view-rail`
  (gutters still from `--ac-page-gutter`); board columns stack vertically
  below 48rem instead of scrolling six-across; Runs/Jobs/Ledger tables
  scroll horizontally in their own `.table-scroll` box (the page never
  scrolls sideways); the header nav collapses behind an aria-wired Menu
  disclosure on narrow viewports and marks the active view (`NavLink` +
  `.is-active`)

## [0.11.0] - 2026-08-12

### Added

- nodes-operator skill: drive the production control plane from any operator (Claude, colleague, codex sessions, humans) — status/runs/ledger/tasks/cancel/validate/publish verbs plus `assign <actor> "instruction"` which renders a single-node workflow, publishes it, creates the run, and watches it to terminal; billable verbs are guarded behind --yes

### Changed

- AGENTS.md and deploy/prod/README.md corrected for post-d1 drift found by a codex actor auditing them THROUGH the new skill: the Python nodes CLI (not a Go binary) is what deploys ship, dangling section/memory references removed, version claim reworded to measured-not-pinned

## [0.10.0] - 2026-08-12

### Added

- Callback compensation chain (issues #16): a failed terminal commit rolls back the sequence mark, reparks the resumed work item, records a TypeCallbackCommitFailed event, and the same-id redelivery commits once the cause clears — the incident-1 permanent-block loop is dead
- Dispatch retry budget: an actor work item parks as failed with a recorded cause after 3 dispatch attempts (deviation d1: per work item, composing with workflow-declared retries), issuing a best-effort actor Cancel on exhaustion
- Terminal-run lease guard: claim and reclaim SQL refuse work items of cancelled/failed/completed runs (issue #19's second loop shape)
- Run cancellation reaps ready+waiting+leased work items and propagates best-effort actor Cancel per pending invocation with a recorded cancel-requested event; api containers carry actor tokens (compose)
- internal/api structured logging (slog): every 5xx logs its error chain; callback terminal-commit failures log with attempt id

### Changed

- actors client disables HTTP keep-alive — kept-alive dispatch connections starved single-threaded bridges (measured live; bridge threading tracked as #21); cancel propagation timeout 30s

### Fixed

- Compensation failures (release/rollback) are recorded instead of swallowed; cancel propagation logs outcomes (review findings)
- tests/fault: the killed-worker runner-operation test no longer reads the operation row inside the window between the engine's completion transaction and the separate statement that retires the row — it waits for the row to leave `waiting_external` and still requires it to settle on `completed` (reproduced 8/8 by tightening the run-status poll; passes 8/8 after)
- tests/load: sampling-cost duration-independence is asserted on work counts — status reads per sample and database operations per sample — instead of on wall-clock cost compared between two fleets measured tens of seconds apart, which on CI differed by a factor of 3.17 while both fleets performed provably identical work; wall-clock cost and duty cycle are still measured, reported, and documented in docs/benchmarks.md

## [0.9.0] - 2026-08-11

### Added

- Codex actor bridges deployed as managed systemd user services on thor and orin (issue #14): non-billable preflight (deploy/prod/codex-preflight.sh) gating startup via ExecStartPre, per-host config template, archive-independent uv-tool install lane in deploy.sh, and the Python nodes CLI shipped to both hosts (deviation d1)
- deploy/prod/register-actor.sh: idempotent, INSERT-only, IPv4-only actor registration helper; deploy.sh resolves each bridge's registered actor row id into its config (ledger origin FK)
- install-secrets.sh codex-bridge token lane: per-host bridge tokens plus both NODES_ACTOR_CODEX_*_TOKEN worker envs over ssh stdin, with keep-existing re-run semantics
- examples/codex-smoke-pair: two-node live smoke workflow (read-only, node timeouts, CONFIRM_BILLABLE gate) with offline compile test; AGENTS.md guidance for codex sessions; codex-bridge operator section + runbooks in deploy/prod/README.md
- tests/deploy: fake-executable and manifest tests for preflight, unit/config definitions, secrets discipline, registration idempotency, and the deploy lane (60+ new assertions)

### Changed

- compose.thor.yml / compose.orin.yml worker env blocks carry both codex actor token envs
- install-secrets.sh is safely re-runnable on a provisioned pair: keep-existing refusals continue to later lanes, runner secrets are guarded with mirror consistency, FORCE now propagates to the remote guards

### Fixed

- codex-preflight accepts CODEX_BRIDGE_AUTH_TOKEN from the environment (the unit's EnvironmentFile path) instead of falsely refusing a non-loopback bind
- deploy.sh stamps the registered actors.id into the bridge config so proposed ledger claims satisfy the origin_actor_id foreign key (first live smoke looped on this)

## [0.8.0] - 2026-08-09

### Added

- Approval/human-task surface end to end: engine writes human_tasks and parks approval nodes leaselessly, API adds human-tasks list/get + POST decision (human-authority review commit), e2e walks the human-review branch (closes #3, deviation d1)
- Runner-service protocol: async-only wire contract over the runner schemas (polling authoritative, resultless optional callbacks), ServiceIdentity registry form, worker park/sample/commit dispatch under fencing, reference nodes-runner service wrapping headspace with durable status and mandatory auth, runner conformance kit
- claude-code and codex actor adapters (contract-v1 bridges, conformance-kit verified; incomplete-never-success)
- Operations web surface: runs board (cards on state columns), cross-run jobs timeline with server-side time-range filter; GET /v1alpha1/runs time params + GET /v1alpha1/node-runs (keyset cursor)
- Python CLI: human-tasks and node-runs verbs, run-list time filters; three-front parity harness
- Production deployment profiles for the thor+orin pair: compose per machine, argv-only ssh deploys, secret install over stdin, scheduled pg_dump backups + restore runbook, runner-registry file (NODES_RUNNER_SERVICES_FILE) and code-runner identity envs
- self-hosting-loop example workflow; deterministic Markdown rendering for ledger projections (?format=markdown); mechanical acceptance evaluation for process_exit/workspace_diff; load proof at 100/1000 concurrent runner operations

### Changed

- waiting_external deadline timers now fail attempts and route timed_out edges
- examples/delivery-loop regains its approval node (d1 lifted); worker docs describe engine-side human parking
- docs/acceptance.md: 42 met / 6 partial / 0 not met

### Fixed

- park-vs-callback race that left run.output null (bounded invocation-lookup retry)
- runs/node_runs gained (namespace_id, updated_at) indexes for the time-windowed listings

## [0.7.0] - 2026-08-09

### Added

- Go control plane (`cmd/nodes`): compiler + `nodes validate` (normalized IR, CEL, precise diagnostics), durable engine with the PRD's §12.5 completion transaction and bounded loops, SKIP LOCKED work claiming with leases and fencing tokens, append-only work ledger with producer-authority enforcement, deterministic projections and atomic stale-guarded reviews, transactional outbox + CloudEvents envelopes, queue abstraction with Postgres and SQS drivers (chaos-tested), scheduler with single-active advisory-lock lease and standby takeover, worker with actor-protocol dispatch, async callbacks and fenced idempotent ingest
- Actor protocol: HTTP/JSON invocation client (sync 200 / async 202 + callbacks, attempt-scoped HMAC tokens, §13.5 error classification) plus a runnable actor conformance kit (`tests/conformance`)
- Runner boundary: registry-pinned IAM-scoped AWS Lambda adapter with honest per-field evidence completeness, and a headspace-cli subprocess bridge for local dev (real-Docker tested); pre-run/post-run code hooks around agent attempts (spec c37/h32)
- Pod-agnostic artifact store (S3/MinIO + Postgres small-blob router), Postgres schema with expand-contract migrations and an N-1 compatibility harness
- OpenAPI 3.1 REST API with SSE run events, CLI/Web parity harness, and the embedded web SPA (`-tags embedweb`)
- React Flow web front: Run + Ledger read-only views carrying the AgentCulture design system (pinned org revision, dashed=proposed/solid=confirmed edges), full keyboard nav, reduced motion, webglass-testable #agent-state node
- Python CLI product verbs as thin API clients (workflow/run/ledger/review noun groups), zero engine logic, byte-exact --json passthrough
- colleague reference bridge (`adapters/colleague`): actor protocol over `colleague work` subprocess, contract v1, conformance-kit-proven
- devague conformance adapter: plan-waves/deliverables fixtures map to deterministic ledger projections
- Deployment: Docker multi-stage image (distroless, SPA embedded), docker compose local profile, Helm chart with migration Job, probes, PDB and callback Ingress (kind-smoked at worker replicas=2), ghcr multi-arch release lane
- Phase-1 vertical-slice e2e (delivery-loop reference workflow) with restart survival, live headspace runner variant, recorded benchmarks (docs/benchmarks.md) and the acceptance-evidence ledger (docs/acceptance.md)

### Changed

- Repo restructured into the PRD §18 Go-rooted monorepo; the Python package narrows to the thin CLI front over the REST API; mesh-agent identity files stay at root
- AWS SDK isolation enforced by lint (aws-sdk only in queue/sqs, artifacts/s3, runners/lambda, awsauth); credentials resolve via the standard chain incl. IRSA (`internal/awsauth`)
- CI: go.yml triggers on schemas/ and migrations/ (go:embed inputs); new web.yml, deploy.yml (kind smoke), release.yml workflows

### Fixed

- Worker-recovery fault test made deterministic: measures h19's reclaim bound directly, survivor starts post-kill, namespace-scoped accounting

## [0.6.2] - 2026-08-08

### Changed

- **`CLAUDE.md` expanded from the scaffold seed into the full runtime prompt**
  (`/init` with `docs/initial-design/*.md` as context). The new prompt frames
  the repo's two layers (the unimplemented Culture Nodes product design in
  `docs/initial-design/` vs the existing mesh-agent scaffold), distills the
  PRD's load-bearing design ground rules (graph vocabulary, ledger authority
  model, domain-outcome-vs-technical-status, content-addressed contracts,
  PostgreSQL-authoritative runtime, headspace code boundary), documents the
  dev commands mirroring CI, the CLI architecture, the CI/PR workflow, and
  restores the template conventions the seed had displaced (vendored-skills
  policy, eidetic memory discipline, ask-colleague reflex, worktree
  location).
- **`README.md` reframed from template prose to the culture-nodes project**:
  adds a Status section linking the initial-design PRD and Phase 0/1
  implementation issue, drops the obsolete "Make it your own" rename
  checklist, and keeps the scaffold inventory, quickstart, and CLI table.

### Fixed

- README quickstart and CLI table invoked the CLI as `culture-nodes`, but the
  installed entry point is `nodes` (`[project.scripts]` in `pyproject.toml`)
  — `uv run culture-nodes whoami` failed with "Failed to spawn". All
  invocations now use `uv run nodes …`; `CLAUDE.md` documents the
  prog-name/entry-point mismatch.

## [0.6.1] - 2026-07-20

### Added

- **Worktree location convention** in `CLAUDE.md` — every worktree you create
  by hand (workforce fan-out lanes, scratch checkouts) lives in
  `../.worktrees.culture-nodes/<name>/`, one
  repo-named directory beside the checkout, replacing a shared `../worktrees/`
  folder. This workspace holds many sibling projects, so a generic shared
  folder accumulates orphaned trees from several repos at once with nothing
  indicating ownership — a stale-tree sweep can't tell a live lane from junk.
  Matches the convention already documented in sibling repo `reachy-mini-cli`.
  Adds branch-prefix guidance (scope the prefix to the work; plain `agent/*`
  collides with leftovers from earlier fan-outs and fails `git worktree add
  -b`), and notes that the vendored `assign-to-workforce` skill uses both the
  shared path *and* `agent/<task-id>` branches in its fan-out example — it is
  cited verbatim and must not be edited, so both are overridden when following
  it. Teardown guidance names `git worktree remove <path>` as the verb that
  actually deletes a worktree; `git worktree prune` only clears metadata for
  directories that are already gone. Tool-managed throwaways are explicitly
  out of scope: `ask-colleague`'s read-only verbs create a detached worktree
  under `${TMPDIR:-/tmp}` and reap it on an EXIT trap, so they never persist
  to need an owner.

## [0.6.0] - 2026-07-18

### Added

- **Four devague-origin skills re-vendored into `.claude/skills/`**
  (cite-don't-import), synced to the fixed devague source
  (devague#74/#75/#76):
  - `challenge` — a risk-scaled blind-spot discovery pass that runs between
    `/think` and `/spec-to-plan`, routing findings back through the existing
    deterministic moves as human-adjudicated proposals.
  - `scope` — the idea→scope leg that surveys the surfaces an idea touches
    before framing, seeding the Announcement Frame with provenance-backed
    boundary/non-goal/assumption claims.
  - `deviate` — stops an in-flight `assign-to-workforce` run when execution
    must diverge from the confirmed plan and records the divergence as a
    first-class, append-only deviation record.
  - `summarize-delivery` — closes the loop after an `assign-to-workforce`
    run with a planned-vs-actual accountability artifact.

  These four originate in `devague` and are re-broadcast via guildmaster; see
  `docs/skill-sources.md` for provenance.

## [0.5.0] - 2026-06-24

### Added

- **Memory-discipline "Conventions and workflow" section in `CLAUDE.md`** — a
  per-task *recall-before / remember-after* convention (scope localized to this
  repo's nick) so the vendored `remember` / `recall` skills are actually used,
  not just present: `/recall` before non-trivial work to build on prior
  decisions instead of re-deriving them, and `/remember` when a non-obvious
  decision, constraint, fix-and-why, or hard-won gotcha surfaces. The section
  documents this repo's memory as **in-repo and public** — records resolve to
  `<repo-root>/.eidetic/memory` (committed, team- and mesh-shared). Inserted
  idempotently (skipped if already present), slotted under an existing
  "Conventions and workflow" heading when one exists, else appended.

### Changed

- **Refreshed the `remember` + `recall` wrappers from eidetic-cli 0.10.0**
  (cite-don't-import) — picks up eidetic's **project-local store default**: the
  files backend now resolves per record by visibility — PUBLIC records inside a
  git repo go to `<repo-root>/.eidetic/memory` (committed, team-shared), PRIVATE
  records (or any record outside a repo) go to `$HOME/.eidetic/memory` (never
  committed), an explicit `EIDETIC_DATA_DIR` still wins, and recall reads both
  stores and merges. Also carries the 0.9.3 hardening (interactive-stdin guard,
  `help` as a search term, SIGPIPE-safe suffix parsing). **Recipe policy
  override (the wrappers here are NOT byte-verbatim):** the injected default
  visibility is flipped from eidetic's `private` to **`public`**, so a plain
  `/remember` lands the note in `./.eidetic/memory` in this repo, kept as part
  of the repo — pass `--visibility private` to route a record to `$HOME`
  instead. `remember` drives `eidetic remember` (idempotent upsert of one JSON
  record or an NDJSON batch on stdin); `recall` drives `eidetic recall` with
  four search modes (exact / approximate / keyword / hybrid). Each `SKILL.md` is
  localized only in the illustrative `--scope <nick>` examples (Provenance keeps
  "First-party to eidetic-cli"). Runtime dep: the `eidetic` CLI on PATH (else a
  local eidetic-cli checkout with `uv`) — **`eidetic >= 0.10.0`** for the
  in-repo routing; on an older CLI the public records still work but are stored
  in `$HOME/.eidetic/memory` instead of in-repo. Propagated by rollout-cli's
  `eidetic-memory` recipe.

## [0.4.0] - 2026-06-23

### Added

- **Vendored the `remember` + `recall` memory skills from eidetic-cli**
  (cite-don't-import) — the write/read halves of eidetic's shared
  `$HOME/.eidetic/memory` surface, so this agent (Claude and its colleague
  backend) can persist facts across sessions and recall them later, sharing
  one store.
  `remember` drives `eidetic remember` (idempotent upsert of one JSON record or
  an NDJSON batch on stdin, dedup by id + content hash); `recall` drives
  `eidetic recall` with four search modes — exact / approximate / keyword /
  hybrid — each hit carrying text, full provenance metadata, a relevance score,
  and a freshness signal. The `.sh` wrappers are byte-verbatim from eidetic-cli
  (their first-party origin); each `SKILL.md` is localized only in the
  illustrative `--scope <nick>` examples (Provenance keeps "First-party to
  eidetic-cli"). Both default to this agent's PRIVATE scope, reading the suffix
  from `culture.yaml`. Runtime dep: the `eidetic` CLI on PATH (else a local
  eidetic-cli checkout with `uv`). Propagated by rollout-cli's `eidetic-memory`
  recipe.

## [0.3.4] - 2026-06-20

### Fixed

- Identity docs and self-description strings still claimed `backend: claude`
  (prompt file `CLAUDE.md`), but this template was promoted to a colleague
  resident in #14/#15: `culture.yaml` declares `backend: colleague` (Qwen) with
  `AGENTS.colleague.md` as the resident prompt. Corrected the stale claim in
  `CLAUDE.md` (Identity section), `README.md`, `docs/skill-sources.md`, and the
  two CLI description strings (`overview` artifacts and `explain doctor`). The
  `doctor` backend→prompt-file mapping and the tests were already on
  `colleague`; this aligns the prose and self-description with them.

## [0.3.3] - 2026-06-20

### Fixed

- pyproject.toml: correct the `license` field and PyPI classifier from MIT to
  Apache-2.0 to match the `LICENSE` file. The README License section was already
  corrected in 0.3.2, but the package metadata was missed; the built wheel now
  reports `License-Expression: Apache-2.0`.

## [0.3.2] - 2026-06-18

### Added

- ask-colleague skill: `monitor`/`guide`/`stop` pilot verbs plus a `--watch`
  flag to dispatch, watch the live feed of, send mid-flight guidance to, and
  cooperatively stop a running colleague flight (re-vendored from colleague).

### Changed

- README: correct the License section from MIT to Apache 2.0 to match the
  `LICENSE` file.

## [0.3.1] - 2026-06-13

### Changed

- CLAUDE.md: add a convention to reach for the `ask-colleague` skill reflexively
  for explore/review/write/grade — read-only `review`/`explore` are always safe;
  side-effecting `write` needs the user's go-ahead.

## [0.3.0] - 2026-06-13

### Added

- AGENTS.colleague.md resident prompt file (backend colleague <-> AGENTS.colleague.md)

### Changed

- Promote agent identity to a colleague resident: culture.yaml backend
  claude -> colleague with a pinned model. The `doctor` backend-consistency
  map gains `colleague` -> AGENTS.colleague.md.

## [0.2.1] - 2026-06-12

### Changed

- **Re-vendored the `ask-colleague` skill from colleague (now 1.7.0, up from the
  0.39.2 sync)** — the wrapper had drifted multiple releases behind origin. Picks
  up the `clean` verb (reap stale/corrupt `colleague/*` branches + orphaned
  `.colleague/` artifacts a crashed run left behind), the `--json` flag on every
  verb (result JSON on stdout, diagnostics/digest on stderr), the
  `_colleague_via_uv` local-dev resolution that honors `--repo`, and the
  tri-state (0/1/2) exit-code contract. `scripts/ask-colleague.sh` + `prompts/`
  are byte-identical to the origin; `SKILL.md` diverges only in the one
  consumer-identifying Provenance clause (`culture-nodes vendors from
  guildmaster`). `docs/skill-sources.md` sync row updated to
  `2026-06-12 (colleague 1.7.0, direct)`. Refs: colleague#183, #186.

## [0.2.0] - 2026-06-06

### Added

- **`ask-colleague` skill** (`.claude/skills/ask-colleague/`) — the first-party front door to the `colleague` CLI (the renamed `convertible`). On top of `explore` / `review` / `write` it adds a `feedback` verb (grade a finished work item — the ROI loop), and `write` now **previews by default** in a throwaway worktree (no side effects) unless `--apply` / `--pr` is given. Reach for it reflexively — `review` for a diverse second opinion on a committed diff before opening a PR, `explore` for a fresh read of an unfamiliar area.

### Changed

- **Replaced the `outsource` skill with `ask-colleague`.** `outsource` was renamed to `ask-colleague` upstream ([colleague#148](https://github.com/agentculture/colleague/pull/148)). Because guildmaster has not re-broadcast the rename yet (its kit still ships the old `outsource`), `ask-colleague` is vendored **directly from the sibling `colleague` checkout** rather than from guildmaster — a tracked local divergence recorded in `docs/skill-sources.md`, parallel to the `agex` → `devex` one. Vendored verbatim except one consumer-identifying clause in the Provenance paragraph.
- **Ledger + CLAUDE.md + `.gitignore`:** point `docs/skill-sources.md` and the CLAUDE.md Skills section at `colleague` / `ask-colleague`, swap the *optional* runtime prerequisite `convertible` → `colleague` (env prefix `CONVERTIBLE_*` → `COLLEAGUE_*`, with the legacy names kept as a deprecated fallback), and gitignore the `.colleague/` run-artifact dir the skill writes (plus the stale `.agex/`).

## [0.1.4] - 2026-05-31

### Added

- **Vendor the `outsource` skill** (`.claude/skills/outsource/`) from
  guildmaster's canonical copy (origin
  [`agentculture/convertible`](https://github.com/agentculture/convertible),
  re-broadcast via guildmaster — guildmaster
  [#51](https://github.com/agentculture/guildmaster/pull/51)). Every agent
  cloned from this template now inherits the ability to hand a scoped task to a
  *different* engine/mind: `explore` (read-only investigation), `review` (a
  diverse second opinion on the committed diff), and `write` (delegate a small
  implementation). `explore`/`review` run isolated in a throwaway `git worktree`;
  `write` refuses a dirty tree. Fulfils
  [#8](https://github.com/agentculture/culture-nodes/issues/8).
- **Ledger + CLAUDE.md:** record `outsource` in `docs/skill-sources.md`
  (origin = convertible, re-broadcast via guildmaster; vendored verbatim — it
  already carries `type: command`) and document its *optional* runtime
  dependency on the `convertible` CLI (the skill exits with an install hint if
  absent, so a clone that never uses it is unaffected).

### Changed

### Fixed

## [0.1.3] - 2026-05-31

### Changed

- Expanded the clone-and-rename instructions in `CLAUDE.md`: added `README.md` to
  the rename targets and a portable `git grep` discovery command so a cloner can
  find every occurrence of the template name (hard-coded in ~100 places across the
  package, including the CLI command files and `_ISSUES_URL` in
  `culture_nodes/cli/__init__.py`) rather than renaming by hand.
- Synced `README.md`'s "Make it your own" checklist with `CLAUDE.md`: it now lists
  `README.md` itself as a rename target and points to `CLAUDE.md`'s discovery
  command as the authoritative procedure, so the two onboarding checklists no
  longer drift.

## [0.1.2] - 2026-05-30

### Changed

- Renamed the PR-lifecycle CLI references `agex` / `agex-cli` to `devex` (same
  tool, new name) across `CLAUDE.md`, `docs/skill-sources.md`, `.gitignore`, and
  the vendored `cicd`, `assign-to-workforce`, and `communicate` skills — the
  `cicd` scripts now invoke `devex pr`.
- Logged the vendored-skill in-place patch as a local divergence in
  `docs/skill-sources.md`; the matching canonical rename is tracked upstream for
  guildmaster in
  [agentculture/guildmaster#48](https://github.com/agentculture/guildmaster/issues/48)
  so a future re-sync reconciles cleanly.
- Aligned the documented `devex` version floor to `>=0.21` across the vendored
  `cicd` `SKILL.md` and `workflow.sh` install hint (were `>=0.1`), matching
  `docs/skill-sources.md` and the `await`-era feature set; flagged upstream on
  guildmaster#48.

### Fixed

- SonarCloud now reports code coverage — added `relative_files = true` to
  `[tool.coverage.run]` so `coverage.xml` emits repo-relative paths that map to
  `sonar.sources=culture_nodes` (absolute / `.venv` paths were dropped
  as unmappable). Mirrors the sibling `convertible` setup.

## [0.1.1] - 2026-05-26

### Changed

- **CI gates on the SonarCloud quality gate**
  ([issue #3](https://github.com/agentculture/culture-nodes/issues/3)) —
  added `sonar.qualitygate.wait=true` to `sonar-project.properties` so a failing
  gate fails the `test` job when `SONAR_TOKEN` is set. Token-less repos and fork
  PRs remain green (the scan step is guarded by `if: env.SONAR_TOKEN != ''`).

## [0.1.0] - 2026-05-26

### Added

- **Onboarded into the AgentCulture mesh** ([issue #1](https://github.com/agentculture/culture-nodes/issues/1)).
- **Agent-first CLI** cited from teken's (`afi-cli`) `python-cli` reference
  (`teken cli cite`) — verbs `whoami`, `learn`, `explain`, `overview`, `doctor`,
  and the `cli` noun group. Runtime is self-contained (`dependencies = []`);
  `teken>=0.8` is a dev dependency only. Passes the seven-bundle agent-first
  rubric (`teken cli doctor . --strict`). `doctor` checks the agent-identity
  invariants (prompt-file-present, backend-consistency, skills-present).
- **Mesh identity**: `culture.yaml` (`suffix: culture-nodes`,
  `backend: claude`) and the matching `CLAUDE.md` prompt file.
- **Canonical guildmaster skill kit** (11 skills) vendored under
  `.claude/skills/` (cite-don't-import): `agent-config`, `assign-to-workforce`,
  `cicd`, `communicate`, `doc-test-alignment`, `pypi-maintainer`, `run-tests`,
  `sonarclaude`, `spec-to-plan`, `think`, `version-bump`. Every `SKILL.md`
  carries `type: command` (load-bearing for the culture/claude backend);
  `cicd` / `communicate` consumer-identifying prose adapted, all script bodies
  verbatim. Provenance in `docs/skill-sources.md`. Three skills (`think`,
  `spec-to-plan`, `assign-to-workforce`) originate in `devague`, re-broadcast
  via guildmaster.
- **Build + deploy baseline**: `pyproject.toml` (hatchling), `tests/` (pytest,
  xdist, coverage), `.github/workflows/{tests,publish}.yml` (CI rubric/lint gate,
  PyPI Trusted Publishing), `.flake8`, `.markdownlint-cli2.yaml`,
  `sonar-project.properties`, and `.claude/skills.local.yaml.example`.

### Changed

### Fixed
