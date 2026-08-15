# Build Plan — own-the-work-end-to-end

slug: `own-the-work-end-to-end` · status: `exported` · from frame: `own-the-work-end-to-end`

> Culture Nodes owns a ticket end to end across hosts: work crosses a machine boundary as a portable handle, a deadline stops the session it bounds, and the run explains itself without a companion document.

## Tasks

### t1 — Bridge push credential: delivery seam + per-host provisioning

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: deploy/prod/install-secrets.sh (follow its line-289 doctrine — relay an externally-issued token, never fabricate one), deploy/prod/\*.service. The host is DERIVED from the actor registration, never declared — deploy.sh already resolves it that way; copying that resolution is the fix for #72. Do NOT read the token value; the operator holds it as `GITHUB_TOKEN_WORKER`. Restart all four spark bridges deliberately after the unit change (c13/h18) — they run from the operator's own checkout, so a restart deploys whatever is on disk.
- covers: c71, h50, c72, h53, c73, h54, c75, h51, c13, h18
- acceptance:
  - Each culture-nodes-claude-\* and codex bridge unit declares an EnvironmentFile, and `GITHUB_TOKEN_WORKER` is readable from INSIDE the running service's environment (verified by reading it from the service, never from a login shell)
  - install-secrets.sh relays `GITHUB_TOKEN_WORKER` without fabricating it, host derived from the actor registration, file mode 600
  - The sweep's `GITHUB_TOKEN` and the handover's `GITHUB_TOKEN_WORKER` are demonstrably different values: a push attempted with only the sweep's token FAILS
  - A work package touching .github/workflows/\*\* is refused at scoping time with a message naming the workflow-scope boundary, not at push time

### t2 — human-inbox units: adopt culture-nodes-\* naming, remove the duplicate, reconcile config paths

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: deploy/prod/deploy.sh lines 197-430, deploy/prod/human-inbox-\*.service. Live state to reconcile against, already measured: spark runs culture-nodes-human-inbox.service (Environment=`HUMAN_INBOX_BRIDGE_CONFIG`=~/.config/culture-nodes-bridges/human-ops.json, no EnvironmentFile) and culture-nodes-human-inbox-tracker.service (EnvironmentFile=~/.config/culture-nodes-bridges/human-ops-tracker.env), and the stale human-inbox-bridge.service FILE is still in ~/.config/systemd/user/ though disabled — removing it, not just disabling it, is the fix.
- covers: c12, h11
- acceptance:
  - deploy.sh installs culture-nodes-human-inbox{,-tracker}.service with the JSON config the running bridge reads
  - The lane stops, disables AND REMOVES any human-inbox-\* unit file serving the same actor — verified by running the deploy TWICE against a host that already has the other naming installed, observing adoption both times rather than a port conflict
  - 'Address already in use' on this lane is a hard deploy failure whose message names the conflicting unit

### t3 — Capability surface: probe bwrap instead of reading two sysctls, across all four bridges

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: adapters/{claude-code,codex,colleague,notify}/src/\*/preflight.py — all four are BYTE-IDENTICAL (md5 76e595f6, 339 lines), so make one edit and copy it, then diff the digests. Keep the injectable 'probes' parameter; the tests rely on it to assert both kinds of kernel. Also edit deploy/prod/README.md and codex-preflight.sh check 7 in the same diff so the two doctrines stop disagreeing.
- covers: c10, h9, c11, h10, c45, h31
- acceptance:
  - A bwrap capability probe is the authority; the sysctl read is demoted to explaining WHY on failure; 'not probed' is reported distinctly from 'available'
  - All four adapters/\*/preflight.py copies remain byte-identical after the change, verified by comparing digests
  - Artifact publish appears in the capability surface as a three-valued fact; notify and human-inbox report not-applicable-no-workspace rather than being silently skipped
  - The losing doctrine's prose is edited in the SAME diff: preflight.py's docstring, deploy/prod/README.md and codex-preflight.sh check 7 no longer disagree

### t4 — File length: gate at 1000 lines repo-wide and split the four files over it

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. Four tracked files exceed 1000 lines: adapters/human-inbox/src/`human_inbox_bridge`/tracker.py (1523), adapters/human-inbox/tests/`test_tracker.py` (1144), tests/`test_run.py` (1019), scripts/`stickiness_ab.py` (1016). 239 of 805 files exceed 300, so 300 stays ADVISORY — do not gate on it. Record the planned splits for the three near-limit files this batch extends (complete.go 939, callback.go 931, `engine_store.go` 923) without splitting them here; their own tasks do that.
- covers: c66, h46
- acceptance:
  - A lint check fails any tracked source file over 1000 lines and is green on the tree after the splits
  - adapters/human-inbox tracker.py (1523) and `test_tracker.py` (1144), tests/`test_run.py` (1019) and scripts/`stickiness_ab.py` (1016) are each under the limit
  - The three near-limit files this batch extends — complete.go 939, callback.go 931, `engine_store.go` 923 — have their splits PLANNED in the packages that touch them, recorded before those packages dispatch

### t5 — Artifact carrier: settle the authorization model, then mount Put/Get and persist `artifact_refs`

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: internal/api/server.go (route table at 300-346), internal/actors/client.go:192 (where `artifact_refs` is nil-defaulted today), internal/runners/headspace/verbs.go (the documented refusal to replace), api/openapi/\*. internal/artifacts is COMPLETE — Store, Router, ref.go's artifact://<namespace>/<id>, postgres + s3 drivers — and both backends are already deployed on thor (prod-postgres-1, prod-minio-1) with migrations 0004/0006 applied. No provisioning, no new library. Settle the authorization model (spec q5) and record it BEFORE writing the route.
- covers: c2, h2, c41, h28
- acceptance:
  - q5 is answered and recorded BEFORE the route is written: a run-scoped read grant, a per-artifact capability, or a re-issued attempt token — with the publish-side TTL problem answered in the same pass
  - A node on ONE host writes an artifact that a node on a DIFFERENT host reads; a same-host round trip does not count as proof
  - The consuming node presents only credentials it was legitimately given — no shared secret, no token minted for another attempt, no bypass of attempt scoping
  - InvocationResult.`artifact_refs` is persisted and resolvable instead of nil-defaulted at internal/actors/client.go:192, and headspace/verbs.go resolves a Ref instead of refusing it

### t6 — `git_ref` carrier: extend handoff.kind, widen the cross-host guard, reuse the preserve machinery

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: tests/lint/`crosshosthandoff_test.go`, examples/pr-upkeep/workflow.yaml (its header at 51-101 already documents this work), schemas/. Reuse t25's write-tree/commit-tree/update-ref machinery in adapters/\*/preserve.py rather than writing a second git path. The guard's bare-path refusal is load-bearing and must survive the widening.
- depends on: t1
- covers: c3, h48, c69, h52
- acceptance:
  - handoff.kind carries `git_ref` beside artifact, and the routing rule is declared once: a runner's changes take `git_ref`, context and data take artifact
  - tests/lint/`crosshosthandoff_test.go` still REFUSES a bare filesystem path handed between nodes — verified by a case that hands one and expects rejection
  - ONE run moves both carriers in the same graph across two machines: the sweep's item list reaches fix as an artifact, fix's diff reaches review as a git ref
  - The ref is produced by t25's existing write-tree/commit-tree/update-ref machinery, not a second git path

### t7 — Artifact retention: reconcile Delete with ledger immutability

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: internal/artifacts/store.go:39-42 (Delete), internal/runners/dispatch.go:375-384 (where evidence already carries `artifact_refs`), plus a migration. The tension is that a ledger record is immutable and can point at content Delete removes.
- depends on: t5
- covers: c44, h30
- acceptance:
  - Following an artifact ref recorded in a ledger evidence record resolves to content OR to an explicit tombstone naming when and why it was reaped — never a bare not-found
  - Storage growth on prod-postgres-1 and prod-minio-1 is sized and recorded, since this batch adds the first production writer to both

### t8 — Continuation condition: continue.while declaration, compilation, CEL evaluation and bounds

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: schemas/workflow/workflow.schema.json, internal/compiler/, internal/engine/workflow.go. #80's issue body specifies the continue.while / bounds / onExhausted shape — implement that, do not invent a second one. The engine evaluates the condition in CEL; no model decides whether to keep going.
- covers: c64, h45
- acceptance:
  - A node declares continue.while plus bounds (maxContinuations, maxWallClock, maxSessions) and an onExhausted domain outcome; the engine evaluates the condition in CEL with no model involved
  - A gate-failure continuation loop TERMINATES on a package whose gate is genuinely unreachable — set a coverage threshold that cannot be met and observe the run reach onExhausted rather than looping
  - Gate-retry bounds are independent of the deadline's bounds: a node can exhaust its clock and its gate-retry budget separately
  - Each continuation is its own attempt row with its own usage, so the cost of continuing is visible rather than folded into one long attempt

### t9 — Deadline cancels the session: scheduler gains a registry and client, cancelling after commit

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: internal/scheduler/scheduler.go (failWaitingExternal:547, applyEffect:469, fireOne:386, tick:344). failWaitingExternal ALREADY holds the PendingInvocation with ActorRef, ActorID and InvocationID — no lookup needed. Copy the shape of internal/worker/budget.go's cancelPendingInvocation minus its lookup, and internal/api/cancelpropagate.go for how a package acquires a registry. The cancel goes AFTER commit (spec decision c48); putting it in applyEffect stalls every timer in the deployment.
- depends on: t8
- covers: c4, h3, c40, h27
- acceptance:
  - A fired deadline consults the declared continuation condition first: holds -> PAUSE keeping the session warm; absent or false -> CANCEL
  - The cancel is verified by observing the ACTOR SESSION end — a process gone, a callback arriving — never by reading the run's status back
  - Nothing in applyEffect's deadline branch makes a network call and fireOne's transaction closes before any cancel is attempted, kept that way by a tests/lint guard
  - Scheduler tick latency stays bounded with a deliberately unreachable bridge holding expired deadlines
  - registryFor(ns) mirrors the existing engineFor(ns) per-namespace cache

### t10 — Workspace fence: refuse a second dispatch into a workspace whose prior session is unconfirmed

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: internal/engine/complete.go:415 (the retry decision), internal/engine/types.go:261-268 (retryable() returns true for StatusTimedOut — this is why the hazard is live). Write the failing test first and confirm it fails against today's main.
- depends on: t9
- covers: c42, h29, c49, h34
- acceptance:
  - A node with MaxAttempts > 1 whose deadline fires produces exactly ONE writing session, verified by observing the second dispatch REFUSED — not by observing that no corruption happened to occur
  - The fence FAILS CLOSED: if occupancy cannot be determined, the dispatch is refused
  - A failing test exists first and fails against today's main

### t11 — Late-callback reconciliation: a superseding record that does not inflate retry burn

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: internal/actors/callback.go:748-755 (late() — records an event and writes NO attempt row, which is the whole gap), internal/store/postgres/`engine_store.go`:570-680 (insertAttemptSQL, and MAX(`attempt_number`)+1 at :659). The unique constraint is `attempts_node_run_attempt_number_key` at migrations/`0002_runtime_execution`.sql:71. Retry burn counts every attempt regardless of outcome (actorstats.go:373-376) — do not inflate it.
- depends on: t9
- covers: c37, h16, c39, h26
- acceptance:
  - A terminal callback arriving after the deadline lands as a superseding record carrying preserve, usage, `usage_model` and `termination_reason` — read back from the attempts table, not from a TypeCallbackLate event body
  - GET /v1alpha1/actors/{id}/stats reports an UNCHANGED Attempts count before and after a deadline reconciliation
  - The representation is chosen before the migration is written, and respects ADR 0002's additive-first rule or documents why it cannot

### t12 — Deadline-cancel event type: a third origin, distinguishable from operator and branch cancels

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: internal/engine/events.go, internal/api/cancelpropagate.go:39 and internal/worker/branchcancel.go:46-49 — read their comments on why the existing two are separated by ORIGIN, and follow that reasoning for the third.
- depends on: t9
- covers: c46, h32
- acceptance:
  - An operator reading only the run's event stream can tell a timeout-driven session stop from an operator-driven one
  - The three cancel-requested types are asserted distinguishable in a test

### t13 — Preserve on the deadline path

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: adapters/codex/src/`codex_bridge`/`async_runner.py`:201-278 and adapters/claude-code/src/`claude_code_bridge`/{server.py:543,`async_runner.py`:192}. The preserve path ALREADY fires on a cancelled session — cancel SIGTERMs, the workspace is measured unconditionally afterwards, and a non-success terminal event routes through preserve. You are proving and pinning that, not building it.
- depends on: t9, t11
- covers: c6, h4, c30, h6
- acceptance:
  - A test pins the DEADLINE path specifically: the deadline fires with a dirty workspace and a preserve ref exists afterwards
  - Proven by RUNNING one — cancel a live session with a dirty workspace and observe the preserve branch — not only by reading `async_runner.py`'s ordering
  - Where preserve genuinely cannot run, the attempt records `preserve_attempted`:false with a reason rather than a null indistinguishable from 'nothing to preserve'

### t14 — Attempt attribution on the API surface

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: api/openapi/openapi.{json,yaml} (Attempt schema — today it has only `actor_id`, `attempt_number`, `completed_at`, `fencing_token`, id, `node_run_id`, `preserve_`\*, result, `started_at`, status), internal/api/runs.go, web/src. The write path already works: attempt 01M00P2B1CGGGJP63TCDY5F18V records `usage_model`=claude-opus-5\[1m\]. You are surfacing it, not building it.
- depends on: t5
- covers: c8, h7
- acceptance:
  - api/openapi's Attempt schema carries the usage block including `usage_model`, plus `termination_reason` and `continuation_ref`
  - The OpenAPI document, the Go handler and the run-detail page land together, so no surface claims a fact another cannot show
  - A fresh attempt from each workspace-holding bridge reads back a non-null `usage_model` through the API

### t15 — colleague and notify report an explicit unknown for model

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: adapters/colleague/src/`colleague_bridge`/mapping.py:234-249 (its docstring states model and thread are 'omitted, never zero-filled' — that is the deliberate behaviour to change), adapters/notify/. claude-code and codex already report model; do not touch them.
- covers: c9, h8
- acceptance:
  - A rollup grouping tokens by model can distinguish 'this backend cannot say' from 'nobody wrote it' — a null serves neither
  - The representation carries through the wire Usage block rather than omitting the field

### t16 — Workspace provisioner: a node that mints a unique worktree per writer

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: adapters/\*/workspace.py (begin(repo) takes a path it is HANDED and only measures — nothing mints a worktree today), adapters/claude-code/src/`claude_code_bridge`/config.py:196-201 (exact-match allowlist). CLAUDE.md's convention is ../.worktrees.culture-nodes/<name>/. Engine-side path handing re-collides with #74 — weigh that before choosing.
- depends on: t1
- covers: c51, h35, c54, h39, c56, h41
- acceptance:
  - Ownership is decided and recorded: bridge-side minting under a permitted root, or engine-side path handing — noting engine-side re-collides with #74's path-portability finding
  - Two dispatches that would resolve to the same worktree: the second is REFUSED or disambiguated, never silently reused
  - No writer's worktree is reachable from another writer's allowlisted root — a check that FAILS against today's nested .claude/worktrees/web-ux-quick-wins case
  - The repo allowlist gains a scoped-prefix form, since a per-writer path cannot be pre-enumerated; spark's stale aehl-t\*/upkeep-t12 entries are reaped

### t17 — Cleanup: a follow-up node plus an age-based orphan sweeper

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. Probed fact: git worktree remove REFUSES a dirty worktree ('use --force to delete it'). Preserve refs live in the shared .git and SURVIVE worktree removal, so the danger is confined to work never preserved. Four stale worktrees exist on spark now (aehl-t13, aehl-t19, upkeep-s3516-node-runs, web-ux-quick-wins) — reclaim them as the acceptance case.
- depends on: t16
- covers: c52, h36, c55, h40, c57, h42
- acceptance:
  - Removal is gated on POSITIVE evidence the work is safe — a preserve ref exists or the artifact handle is published — never on the absence of an error, and --force is never the default
  - Cleanup run against a dirty worktree whose preserve ref does NOT exist REFUSES, proven by that case rather than by a clean-worktree test
  - An orphaned worktree from a cancelled run is still reclaimed by the sweeper, which independently refuses one whose work is unpreserved or whose session is unconfirmed
  - The four stale worktrees on spark are reclaimed as the acceptance case
  - A failed cleanup is a routable domain outcome leaving the worktree in place, never an engine failure

### t18 — Handover gate: a deterministic validator node producing derived evidence

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. Gate reach, measured: coverage thresholds exist only for `culture_nodes`; Sonar analyses only `culture_nodes` (sonar.sources); file-length validation exists nowhere; changed-file scoping is Sonar-server-side only. Issue #88 tracks widening. Where an instrument does not reach a tree, record that tree tests-only rather than reporting a green that measured nothing.
- depends on: t4, t8
- covers: c53, h38, c58, h47, c61, h44
- acceptance:
  - The node measures tests, coverage, cognitive complexity and file length ON CHANGED FILES and reports a NUMBER per gate, not a bare pass/fail
  - Thresholds are declared before the work starts; a gate that cannot apply to a changed file is recorded not-applicable rather than counted as passing
  - A failing gate is a routable domain outcome routing to a continuation under declared bounds, never an engine failure
  - Gate results land as DERIVED records from a deterministic validator, never as an agent's proposed completion claim
  - Thresholds are asserted against a changed file in internal/ or adapters/; where issue #88 has not extended an instrument, that tree is recorded tests-only rather than reporting a green that measured nothing

### t19 — pr-upkeep sweep source: derive URL and digest from the revision deploy.sh already ships

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: deploy/prod/deploy.sh lines 26-75. It already does git rev-parse --short $BRANCH at line 30 and ships git archive of that revision — derive both values from it. The current pin (0abf042) is on an object unreachable from any branch after #85's squash merge.
- depends on: t2
- covers: c14, h12
- acceptance:
  - deploy.sh derives `PR_UPKEEP_SWEEP_SOURCE_URL` from the sha it resolves at line 30 and the digest from examples/pr-upkeep/sweep.py in that same revision
  - Proven against a SQUASH-MERGED revision — the case that orphaned 0abf042 — not against a branch tip that happens to still be reachable
  - The operator can still override both explicitly, since running someone else's copy is the point of the grant

### t20 — pr-upkeep multi-repo: a granted repo set, per-host checkouts, scoped credential

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. FILES: examples/pr-upkeep/sweep.py:85-93 (the pinned constants and the blast-radius note to rewrite, not delete), tests/`test_pr_upkeep_sweep.py`:660-740 (`ALLOWED_ENVIRONMENT_READS` is an EXACT-SET assertion designed to fail loudly — edit it deliberately).
- depends on: t19
- covers: c15, h19, c23, h13, c47, h33
- acceptance:
  - `SONAR_COMPONENT_KEY` and `GITHUB_REPO` become one per-repo pair resolved from a granted environment value; a caller naming a repo in run input is still REFUSED
  - tests/`test_pr_upkeep_sweep.py`'s environment-read assertion is edited deliberately to admit exactly the new value and nothing more, and the blast-radius note is rewritten rather than deleted
  - One repo per cycle, in configured order, and the sweep reports which one it swept
  - Every repo in the set has a checkout AND an exact-match allowlist entry on BOTH the fix and review actor hosts before the set is enlarged
  - The sweep's credential is scoped to exactly the configured set, and the deployment records that scope where an operator sees it

### t21 — The development-loop workflow: wire the new nodes, events and connections into a committed graph

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. This is the dogfooding centrepiece. Read examples/pr-upkeep/workflow.yaml and examples/delivery-loop/workflow.yaml as the skeleton; eleven committed examples already validate clean and cover sweep/triage loops, split/join, human gates and cross-machine dispatch. Validate offline with nodes validate — no control plane needed. Declare the split plan's session counts against the remaining window BEFORE any wave dispatches.
- depends on: t6, t8, t12, t17, t18
- covers: c21, h14, c24, h15
- acceptance:
  - A committed example workflow expresses the whole loop as a graph: provision-workspace -> work -> handover-gate -> (pass) merge-review / (fail) continuation -> cleanup-workspace, with every step a node carrying a contract rather than an operator habit
  - The new NODES are declared and compile on the shipped compiler: provision-workspace, handover-gate, cleanup-workspace
  - The new EVENTS are emitted and visible in the run's event stream: deadline cancel-requested, workspace-fence refusal, worktree provisioned, worktree reaped
  - The new CONNECTIONS are exercised: both handoff carriers (`git_ref` for changes, artifact for context), the gate-failure -> continuation edge under declared bounds, and onExhausted -> a human node
  - The graph declares its dependency order (spec c21) so the order is a compiled artifact rather than prose a reader must obey by hand
  - The split plan declares expected model-session count per wave against the remaining subscription window BEFORE any assignment, with big packages routed to codex actors and the operator's window reserved for operator-lane work and merge gates
  - It validates offline through internal/compiler with 0 errors, like the eleven existing examples

### t22 — Author nodes and flows from text, powered by an agent node (#81)

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. The validate/publish door already exists and is proven: web/src/routes/AuthorWorkflow.tsx (ADR 0007) does paste -> validate -> diagnostics -> graph preview -> publish, shipping the operator's EXACT source string so the digest matches the CLI path. Build the generator in FRONT of that door; do not touch the door. Follow tests/lint/{webhook,github}`isolation_test.go` for the no-model-in-the-control-plane gate.
- depends on: t21
- acceptance:
  - A plain-text description produces a workflow compiling with 0 errors from the dashboard, the CLI, and an agent mid-run — one implementation behind one API, not three generators
  - Nothing publishes without passing validate and a human confirmation; a generated-but-unconfirmed workflow is visibly proposed
  - Generation happens in an agent node on the registered fleet — NO model call inside the control plane, enforced by a tests/lint gate the way webhook and GitHub egress already are
  - Editing an existing workflow produces a diff against the pinned version, never a silent replacement
  - A generation that cannot reach a compiling document within its bound is a routable domain outcome, not an engine failure

### t23 — Jira Cloud as a third finding source (#76) — fixtures only, live proof separately gated

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. Settled by live probe: Jira Cloud REST v3 needs HTTP Basic account-email:api-token (Basic 200, the same token as Bearer 403), so TWO config values are required. Rate limit is 350/window. The backlog is EMPTY — a JQL across the whole site returns zero issues — so fixtures are the only honest acceptance and live proof is a separately blocked gate.
- depends on: t20
- acceptance:
  - A Jira backlog item surfaces as a prioritised work item from a sweep run against RECORDED FIXTURES, with site host, project key and credentials all supplied as configuration — never module constants
  - The unit tests pass with no network access and no credential present
  - No new node kind and no engine change: it compiles and runs on the shipped compiler exactly as pr-upkeep does
  - No credential, account email or personal identifier appears in any committed artifact, argv or log line
  - Live proof is recorded as a SEPARATE gate, explicitly blocked until the backlog has content, and is NOT written into this batch's success signals

### t24 — SELF-TEST: every tree green under its own suite and every declared gate

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. Run every tree's own CI locally before claiming green: uv run pytest -n auto --cov=`culture_nodes`, go build ./... && go vet ./... && go test ./..., the adapter suites, and npm run build && npm test && npm run test:e2e. Plus the lint job: black, isort, flake8, bandit, markdownlint-cli2, teken cli doctor --strict. A self-test failure STOPS the chain.
- depends on: t3, t7, t9, t10, t11, t13, t14, t15, t19, t22, t23
- covers: c36, h25
- acceptance:
  - All four trees pass their own CI: pytest (`culture_nodes`), go build + go vet + go test ./... (internal, cmd), the adapter suites, and web build + vitest + playwright
  - Every gate this batch declared is green on the changed files: tests, coverage and cognitive complexity where the instruments reach, and file length repo-wide at 1000
  - No agent-created record in the batch carries authority above 'proposed' and no code path lets an actor promote its own proposal, checked by the existing ledger authority tests rather than by review
  - Run BEFORE the live test: a self-test failure stops the chain rather than being discovered during the live run

### t25 — LIVE-TEST: the end-to-end proof on the real fleet, across two machines

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. Use the live fleet: control plane at <http://192.168.1.146:18080>, actors codex-thor and codex-orin plus the four spark claude bridges. Drive it through the nodes-operator skill. Reference runs to compare against: 01M00NVQH582QBARWSYFR0WTG1 (terminated `handoff_unavailable`) and 01M00BKCVT04SSGPNXDF3JJX5G (the t14 timeout). The companion-document test is the hard one — hand someone only the run.
- depends on: t24
- covers: c1, h1, c32, h21, c35, h24
- acceptance:
  - ONE run of the loop completes across spark and thor: sweep finds an item, fix produces its changes on spark, review consumes them on thor, with NO path passed between them and no operator copy step
  - A node killed mid-write has its session STOPPED and its work recovered from the run alone, with no human touching a filesystem
  - Cancelling a run ends the actor session, verified by observing the session end rather than reading the run's status
  - Every attempt in the run records what ran it: model, effort, cost — read back through the API, not the database
  - The COMPANION-DOCUMENT TEST: a reader given only the run — no delivery summary, no state file, no transcript — can say what happened and what state it left behind
  - Each of the three audiences has at least one signal that could have failed and did not; any signal only proven by test rather than by this live run is narrowed to what was actually run

### t26 — Delivery summary: planned versus actual, with every claim scoped to its evidence

- instruction: READ FIRST, do not re-derive: docs/specs/2026-08-14-own-the-work-end-to-end.md (the claim this task covers, with its honesty condition and instruction) and CLAUDE.md. The spec already names the file, line and measurement for every finding — cite it rather than re-measuring. Use the summarize-delivery skill. The delivery row to correct is in docs/deliveries/2026-08-14-economy-discord-graphs.md. Report failures faithfully: this batch exists because a previous high-confidence claim was true by test and never exercised in production.
- depends on: t25
- covers: c28, h5, c31, h20, c33, h22, c34, h23
- acceptance:
  - docs/deliveries/2026-08-14-economy-discord-graphs.md's preserve-on-failure row is corrected to 'verified by test, never exercised in production' — neither 'high confidence' nor 'false', and stating the production sample size was zero
  - Every one of the five before-state costs is cited to a checkable record: run 01M00BKCVT04SSGPNXDF3JJX5G and commit 3f9bd3c, run 01M00NVQH582QBARWSYFR0WTG1, probe t9, the 1-of-140 `usage_model` measurement, and the -STATE.md file itself
  - Each success signal carries an explicit verdict INCLUDING the negative ones; no signal is marked met because the code shipped
  - The summary states, for each after-state verb, whether it was proven on the live run or only by test
  - It records whether a -STATE.md companion file was needed this cycle — the batch's own headline claim, answered rather than assumed

## Risks

- [unknown_nonblocking] The handoff design is the least settled area in the spec and everything sequences behind it: three challenge passes each overturned something already confirmed there — c37's cancel ordering, then c3's artifact-only contract, and the carrier decision re-threaded the dependency order twice. Treat the two-carrier decision as provisional at the split-plan gate rather than final.
- [unknown_nonblocking] The codex workspace-write path has never been proven to land a patch (#18). Every write-side acceptance routed to a codex actor inherits that unknown, and #63's userns fix was verified only against a read-only probe.
- [unknown_nonblocking] The disk cost of per-writer worktrees multiplied by the multi-repo set was measured on spark only (2.4TB free, ~330MB per worktree). thor and orin are Jetson-class with materially smaller storage and the two requirements compound as repos x writers.
- [unknown_blocking] The token's granted scope is not established: a permissions probe reported admin/push true, but for a fine-grained PAT that reflects the authenticated identity's role rather than the token's grant. A classic repo-scope token would give an unattended bridge admin on every reachable repo.
- [unknown_nonblocking] Least-privilege posture on the bridge push credential: an unattended service holding a broad token is a larger blast radius than the handover needs. A permissions probe reported admin/push true, but for a fine-grained PAT that reflects the authenticated identity's role rather than the token's grant, so the probe cannot tell a correctly-narrow Contents:write token from a broad classic repo-scope one. Verified at the operator's discretion (a classic PAT is prefixed `ghp_`, a fine-grained one `github_pat_`); the remedy if broad is to reissue fine-grained. Not blocking — t1's acceptance criteria carry the check. (task t1)
