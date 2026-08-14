# upkeep-actors-jira

> culture-nodes ships the upkeep-actors-jira cycle: every economy-discord-graphs issue whose delivery is evidenced is closed with that evidence cited, the pr-upkeep loop's four upkeep defects are fixed so a real PR flows sweep-to-merge without an operator nursing it, dispatched actors get a clarify-then-commit gate, credential rotation stops destroying keys it does not own, and every committed demo becomes loadable by a deployment that is not this one — with three of this cycle's pr-upkeep items running through culture-nodes rather than beside it. The Jira Cloud node-loop the frame was named for was scoped in full here and deliberately deferred to issue #76.
> instruction: Treat each announcement clause as a coverage target; at cycle end record any clause not achieved as not achieved, the way signals 2 and 5 were handled last cycle

## Audience

- The operator who watches this repo's PRs and the SCRUM backlog and today spawns a subagent because it is faster than dispatching through the engine; secondarily the workflow author declaring observables and cross-machine handoffs, and the third party who loads a committed demo flow into a deployment that is not this one

## Before → After

- Before: Today: exactly one real work item has gone through the engine (run 01KZZSGSWH11J7R7P4V2HPTZZQ — a pr-upkeep sweep that dispatched a fix landing actual SonarCloud fixes as commit b01608c), against 35 plan tasks in the last batch that went around it; that one item's review leg 403'd on a path that does not exist on the reviewing host, the observable authoring convention has never compiled, the merge tracker watches a host the actor is not on, credential rotation silently drops keys it does not own, and nine of eleven committed examples bake this deployment's topology
- After: Three pr-upkeep items complete sweep-to-merge without an operator nursing the loop; a parked human task completes itself on merge; a workflow whose fix and review land on different machines finishes; every committed example compiles in CI and is loadable by someone who is not this deployment; a rotation preserves keys it does not own; and a dispatched actor is briefed before its first billable turn rather than corrected after it

## Why it matters

- The product proved itself but never built itself, and the honest reason is ergonomics: dispatching through culture-nodes is slower for the operator than spawning a subagent, so under a deadline the fast path wins. Each defect in this batch individually stops the loop from completing, so fixing them is what converts a demo into the path an operator actually reaches for

## Requirements

- Issue hygiene: PR #70's body used a prose 'Closes the ten-issue batch: #41, #43, ...' form GitHub does not parse, so all ten (#41 #43 #45 #46 #47 #48 #49 #54 #56) plus #68 are still OPEN despite being delivered; this cycle closes each evidenced issue with a comment citing its row in docs/deliveries/2026-08-14-economy-discord-graphs.md
  - instruction: Close each delivered issue with 'gh issue close --comment' citing its delivery-doc claim row or the file:line proving it; already executed for ten issues — apply the same rule to any further closure
  - honesty: Every issue closed this cycle names a specific artifact — a run id, file:line, migration number, or delivery-doc claim row — and no issue is closed on the strength of a merged PR alone
- \#64 and #65 are verified delivered in tree — `human_inbox_bridge`/config.py no longer requires `GITHUB_TOKEN`, and compose.thor.yml:137-149 + compose.orin.yml:30-38 carry `NODES_CODE_RUNNER_`\* and `NODES_RUNNER_SERVICES_FILE` — so both close with those citations
  - instruction: No implementation — verification is re-reading config.py and both compose files, already done and cited in the closure comments
  - honesty: Re-reading config.py and both compose files today still shows the token-optional path and all four runner env keys; the closure comments cite the tree, not the PR body
- \#66 is NOT delivered: internal/notifier/rundetail.go still carries only `workflow_digest` with no digest-to-key lookup, so the Discord message still shows 64 hex characters where a human needs a name — it is live work this cycle, not a closure
  - instruction: In internal/notifier, add a cached digest-to-workflow-key lookup against the workflows read API and render 'name (short-digest)'; omit the actor field entirely when empty rather than rendering it blank; extend `rundetail_test.go`
  - honesty: The notifier renders a workflow key resolved through a cached digest-to-key lookup, and a run with no agent actor produces a message carrying no actor field at all rather than an empty one
- \#73's recurrence half is the real acceptance: no job under .github/workflows references examples/, so a compile check over every workflow in examples/ is what prevents a non-compiling example shipping again — the example edit alone does not
  - instruction: Add a CI job compiling every workflow under examples/ via the compiler package or the validate endpoint; prove it by deliberately breaking one example and observing the failure before merge
  - honesty: The CI job FAILS when a deliberately malformed example is committed — demonstrated once before merge, not assumed from a passing run
- \#72: the live actor list on thor:18080 resolves company/human-ops to <http://192.168.1.157:8090> (spark) while deploy/prod/install-secrets.sh:236 and human-inbox-bridge.service both declare the bridge and tracker 'thor only' — move the tracker to the actor's host AND make a tracker refuse to start when its bridge does not serve the actor it observes
  - instruction: Move the human-inbox tracker unit to the host serving company/human-ops in deploy/prod/deploy.sh; at startup resolve the configured actor's `endpoint_ref` from the control plane and exit non-zero when it does not match the tracker's own bridge URL; pin with a test
  - honesty: A tracker configured against a bridge that does not serve its actor exits non-zero at startup naming both the actor endpoint and its own bridge URL; and in production the tracker and the human-ops bridge resolve to the same host
- \#74: examples/pr-upkeep/workflow.yaml binds fix to actor://company/developer (live endpoint 192.168.1.157:8088, spark) and review to actor://company/codex-thor (192.168.1.146:8086, thor) and then hands a filesystem path between them; the portable handle is a pushed git ref, reusing t25's preserve-branch plumbing rather than duplicating it
  - instruction: Replace the path binding between fix and review with a git ref: the fix lane pushes its branch through t25's write-tree/commit-tree/update-ref plumbing, and review binds the ref; document bare paths as same-host-only or refuse them at compile time
  - honesty: A run whose fix and review actors sit on different hosts completes, and the review lane's report quotes content it could only have obtained by fetching the fix lane's ref
- \#61: sweep.py's main() (:429-458) merges exactly two finding sources (`sonar_work_items`, `qodo_work_items`); a third from GET /repos/{repo}/commits/{sha}/check-runs is additive inside the same prioritise / `build_report` / `exit_code_for` shape and needs no new credential
  - instruction: Add a check-runs source to sweep.py reading GET /repos/{repo}/commits/{sha}/check-runs per open PR; map required failures to HIGH/CRITICAL and optional to MEDIUM; skip Sonar-named checks to avoid double-counting; unit-test against a recorded fixture
  - honesty: A recorded check-runs fixture yields work items carrying check name, PR number and details URL; Sonar-named checks are skipped so a quality-gate failure does not double-count; and the 0/10/other exit contract is unchanged
- \#67's prior art is tests/deploy/`destructiveconfirm_test.go`'s single-use windowed confirmation file; generalizing it from danger to understanding keeps that shape, and its ledger fit is preflight = derived, acknowledgement = proposed
  - instruction: Implement the gate in the engine: a derived preflight record composed from host capabilities plus the task declaration, and an actor acknowledgement as a proposed ledger record via a dispatch confirm verb, single-use and windowed like the destructive-confirm protocol; bridges expose host capabilities only
  - honesty: An actor's acknowledgement exists as a ledger record before its first billable turn, and a dispatch whose preflight was never acknowledged does not proceed
- The dogfooding baseline is ONE, not zero, and the distinction matters: docs/deliveries/2026-08-14-dogfooding-baseline.md measured '0 of 35' counting only the batch's 35 PLAN TASKS, but a real pr-upkeep item did run through the engine in the same window — run 01KZZSGSWH11J7R7P4V2HPTZZQ swept, triaged and dispatched a fix that landed genuine SonarCloud fixes as commit b01608c on a spark actor, before its review leg 403'd on #74's cross-machine path. So the honest baseline is 1 out of 35, this cycle's target of three items completing the full loop is a real increase over a real number, and the count must come from a ledger query rather than session memory
  - instruction: Write the ledger query counting real items through the engine; run it at cycle start to pin the baseline of one and again at cycle end; put both numbers in the delivery summary
  - honesty: The count comes from a query over runs and attempts, not from session memory or commit prose
  - honesty: The baseline and the end-of-cycle count are produced by the same ledger query over runs and attempts, so 'more than last cycle' compares like with like rather than one measured number against one remembered one
- Batch scope is NINE work items, Jira excluded: the four upkeep bugs (#71 residual, #72, #73, #74), the two small verified-unfixed items (#61, #66), the clarify-then-commit gate (#67), #69 items 1-2 (rotation-merges-not-replaces, post-deploy credential audit), and demo portability across all eleven committed examples — plus a ledger-measured target of three pr-upkeep items completing the full loop through the merge gate. The Jira Cloud node-loop was scoped in this cycle and deliberately moved out to issue #76
  - instruction: Track the nine items as the plan's coverage targets; anything dropped is recorded with a reason in the delivery summary rather than quietly omitted
  - honesty: Every item in the declared scope either ships or is recorded as dropped, with a reason, in the delivery summary
- Committed example flows are DEMOS others load and modify, not this deployment's private config: every environment-specific value an example carries — actor keys, endpoint IPs, hostnames, repo and component keys, site URLs, filesystem paths — must arrive as run input or as clearly-marked overridable config, never as an invisible constant only this deployment can satisfy
  - instruction: Move every environment-specific value in examples/ to run input or documented config; where a value must stay pinned, document it at the constant as the one value a new operator changes
  - honesty: Loading an example into a different deployment requires changing only documented configuration, never editing the graph; a reviewer can point at each environment-specific value and say where it comes from
- Recorded fixtures carry no real account identities: examples/pr-upkeep/fixtures/sonarcloud-issues.json's four assignee values and adapters/human-inbox/tests/`test_tracker.py`'s three reply-author logins were personal handles in committed files and are now neutral placeholders, with both suites still green (31 and 144 passed)
  - instruction: Already executed — fixtures scrubbed, both suites green; the durable part is the lint from c25
  - honesty: Both affected suites stay green after the scrub, and no committed fixture carries a real account identity
- CHALLENGE FINDING: this batch's issues were written against a tree PR #70 then changed, and two of nine items are already substantially delivered (#71's redesign, #73's example fix). Every item must be re-verified against main before it becomes a plan task, and an item found already delivered is closed with evidence rather than planned — otherwise the plan bills work that does not exist
  - honesty: Each of the nine items is re-checked against main at plan time, and the check is recorded — an item found already delivered is closed with evidence instead of becoming a task
- The handoff mechanism must degrade honestly: if the fix lane cannot push, the run reports an explicit domain outcome naming the missing capability rather than failing as a 403 at the review node — the failure mode #74 exists to eliminate must not simply move one node later
  - honesty: A run whose fix host cannot push produces a named domain outcome identifying the missing capability, demonstrated by disabling the credential deliberately
- \#67's gate ships per-actor and default-off, turning on only for actors whose bridge advertises the capability surface: an engine-side gate that fails closed for every actor at once would stop all ten registered actors on the day it merges, and there is no preflight or acknowledgement surface anywhere in internal/ or cmd/ today for any of them to satisfy
  - honesty: Enabling the gate for an actor whose bridge does not advertise the capability surface is refused at configuration time, and the ten existing actors keep dispatching unchanged on the day it merges
- Merge-not-replace must not make deliberate key REMOVAL impossible: a key that should genuinely go away needs a documented, explicit removal path, or the rotation fix trades a silent-destruction bug for a file that can only ever grow
  - honesty: Removing a key from prod.env through the documented path actually removes it, proven by a test that adds then removes one
- \#73's remaining substance is the typed literal binding (its option A): a binding value may be a pointer string OR a declared literal object, so an author reads the graph and can see what a node observes. Today the observable lives in the run-input schema and the graph shows only a pointer — the visibility loss option B knowingly accepted, and the reason the convention was worth having
  - honesty: An author reading only the workflow text can name what each human node observes, without opening the run-input schema

## Honesty conditions

- Every headline in the announcement is backed by a live artifact at the end of the cycle — a ledger-counted number for the three items, a completed cross-machine run, a self-completing human task, a CI job proven to fail on a broken example, and a Jira work item out of a sweep — and anything not achieved is recorded as not achieved rather than softened, the way signals 2 and 5 were last cycle
- No issue closes this cycle without a named artifact, and #54 in particular closes only on a ledger record naming a merge commit
- sweep.py imports nothing outside the Python 3.12 stdlib, and each of the exit-code contract's three branches is covered by a test
- A grep for credentials, account emails and personal handles over every committed path returns nothing, and the rule is enforced by a test rather than by reviewer attention
- A new operator can find the one value to change from the README or a comment at the constant itself, without reading the parsing code
- At least one of this cycle's own work items is dispatched through the engine by the operator it is built for, not only by its author
- Every clause of the after-state maps to a success signal with a named artifact; a clause with no signal behind it is struck rather than carried
- Every 'today' statement is re-checkable against the tree or a run id at the start of the cycle, so drift is visible when the delivery summary compares
- The ergonomics claim is tested rather than asserted: the end-of-cycle count of real items through the engine is higher than the baseline of one
- The three are counted by a ledger query, and a partial completion is reported as partial rather than rounded up to a whole item
- The review lane quotes content obtainable only by fetching the fix lane's ref, and the run's attempt rows name two different hosts
- The ledger record names the merge commit and its `collection_method`, and the task's history shows no manual submit
- The job is proven by deliberately breaking one example and watching CI fail, before merge
- The refusal is exercised by a test that points a tracker at a bridge serving a different actor, not only by the production fix
- The proof rotates with an externally-issued key present in the file and finds that key still there afterwards
- A run with no agent actor produces a message carrying no actor field at all, and the workflow appears by name with the digest secondary
- Any new ledger record type this batch introduces is added by an expand migration an N-1 binary ignores safely, verified the way migrations 0017-0025 were

## Success signals

- Three pr-upkeep items complete sweep -> fix -> review -> human-merges-pr, counted from the ledger rather than remembered
- A workflow whose fix and review land on different machines completes, with review reading the fix lane's actual work through a portable handle instead of a filesystem path
- A human task parked with an observe declaration completes itself when the PR merges, with a ledger record naming the merge commit and its `collection_method` — closing the signal 5 that #70 could not close
- Every workflow under examples/ compiles in CI, and a deliberately broken example fails that job — the check is proven by breaking it, not by passing once
- A tracker whose bridge does not serve the actor it watches refuses to start, instead of reporting pending=0 forever
- A credential rotation preserves every key it does not own, proven by rotating with an externally-issued key present and finding it still there
- A Discord run notification names its workflow and omits fields that are legitimately empty

## Scope / boundaries

- Closure is evidence-gated, not merge-gated: the delivery doc's claims table marks 'Merging a PR completes a human task' and 'Resuming reduces uncached input' as unverified, so #54 and #48's stickiness half do NOT close on this pass — they close when a live observation exists
  - instruction: Before closing any issue, check the delivery doc's claims table: an 'unverified' row means the issue stays open until a live observation exists
- sweep.py's invariants hold for every new source: Python 3.12 stdlib only, the repo and SonarCloud component keys hardcoded (spec claim c26), and the 0 / 10 / other exit-code contract that the triage decision node routes on
  - instruction: Any new finding source keeps sweep.py stdlib-only and leaves the 0/10/other exit contract untouched; add a test per exit branch
- No credential, account identity or personal identifier is recorded in any committed artifact of this cycle: secrets are read from the environment at run time and appear in no spec, plan, frame, example workflow, argv or log line — the same custody rule tests/lint/`webhookisolation_test.go` and `github_isolation_test.go` already enforce for the webhook and GitHub credentials, extended to recorded test fixtures after four personal handles were found in committed ones
  - instruction: Add a tests/lint check that fails on credential and personal-identifier patterns across committed paths, mirroring `webhookisolation_test.go`
- Hard-coding the swept repo stays a deliberate blast-radius boundary (the prior spec's claim c26 — the sweep must not generalise to arbitrary repos), but for a loadable demo that boundary has to be VISIBLE and documented as the one value a new operator must change, rather than an unexplained constant
  - instruction: Keep the repo hardcoded but add a comment at the constant and a README line naming it as the single value to change
- \#67's preflight and acknowledgement records follow ADR-0002's expand-only migration policy like every other ledger change — a new record type is an expand migration an older binary can ignore, never a contract that breaks N-1 compatibility

## Non-goals

- No egress-allowlist work this cycle: the sweep node's operation declares network: full because headspace-cli 0.11.0 rejects egress-allowlist (#50), so the runner boundary honestly reports the gap rather than hiding it — the posture is documented, not fixed
  - instruction: Leave network: full in place and keep the declared-intent comment; revisit only when headspace ships allowlist support (#50)
- The ledger authority model does not bend for any of this: a notify send or a human-inbox submission reported by a bridge is a proposed claim, never observed — only facts a trusted runner or the merge tracker directly measured carry observed authority with a `collection_method`, and no actor promotes its own proposal
  - instruction: When adding any bridge-reported outcome, assert its ledger authority is proposed in a test rather than relying on review
- The Jira Cloud node-loop is NOT in this cycle. It was fully scoped here — auth shape verified live, rate budget measured, boundaries and portability requirements settled, and the empty-backlog blocker on live proof identified — and all of it was carried into issue #76 so the next batch picks it up without re-investigating
  - instruction: Do not implement any Jira surface this cycle; issue #76 carries the full scope

## Assumptions

- Option A (typed literal binding) is a four-site change, not a schema bump: schemas/workflow/workflow.schema.json, internal/compiler/model.go's inputBinding, internal/engine/workflow.go's InputBindings map\[string\]string (:162, :464), and internal/worker/bindings.go's resolveNodeInput
- \#71 is already substantially on main from b5cdc5b: workflow.yaml routes triage.empty to backoff ('issue #71: idle is no longer a human task') and tracker.py carries `REPLY_OBSERVATION_KIND` and `CLOSED_OBSERVATION_KIND`; what is absent everywhere in the tree is `github_pr_opened` — so #71's residual is only its new-PR trigger, option A or B
- The fleet is live and available: thor:18080 answers healthz 200 with ten registered actors (codex-thor, codex-orin, developer, intake, planner, verifier, human-ops, notify-discord, operator-claude, and the headspace/docker runner), so routing this cycle's work packages through the engine is a scheduling choice rather than a provisioning one
- Audited: committed examples currently bake this deployment's topology. Nine of eleven example workflows name thor/orin/spark or 192.168.1.x endpoints; sweep.py pins `GITHUB_REPO` and `SONAR_COMPONENT_KEY` as module constants; and pr-upkeep's sweep node fetches its script from a raw.githubusercontent URL pinned to one org, commit and path. A third party loading these today gets a flow that cannot run and cannot easily be told why
- CHALLENGE FINDING: #74's recommended git-ref handoff depends on a push credential nobody has verified on the host that must push. Probed live: spark — where the fix lane's company/developer actor runs — has gh auth via KEYRING and no git credential.helper, and a systemd user service typically cannot reach a keyring; thor has credential.helper=store and gh auth OK; orin has neither. The delivery doc already flagged the preserve push leg as unverified, and this spec now makes that unverified capability load-bearing for c10 and success signal c36
- CHALLENGE FINDING: the human-inbox idempotency store is per-bridge and file-based (one JSON file per key under Config.`state_dir`), so 'one logical human inbox' is enforced by deployment convention rather than by the store — two bridges serving one actor would not deduplicate each other's submissions. c9's startup identity check is therefore the ONLY mechanism standing between a split deployment and double submission, which raises its priority above a mere ergonomics fix
- CHALLENGE FINDING: the three-item target has almost no slack in its work supply. Probed live: ZERO open PRs and three unresolved SonarCloud issues on main. The Qodo feed and the per-PR Sonar query both need open PRs to return anything, and #61's new check-runs source needs open PRs with failing checks — so today's entire supply is three main-branch findings for a target of three items. The loop is partly self-feeding (this batch's own PR generates PR-scoped findings once open), but a plan that assumes work will be there is assuming, not measuring

## Scope exploration

- `s1` — `PR #70 body + gh issue list (open state)`: the prose 'Closes the ten-issue batch: #41, #43, ...' form is not parsed by GitHub, so ten delivered issues plus #68 are still open
  - seeds: `c2`
- `s2` — `docs/deliveries/2026-08-14-economy-discord-graphs.md (Delivery Claims table)`: two claims are explicitly marked unverified — signal 5 (merge completes a human task) and stickiness's uncached-input reduction (measured 0.0%, twice) — so those issues are not closable on merge evidence
  - seeds: `c3`
- `s3` — `adapters/human-inbox/src/human_inbox_bridge/config.py + deploy/prod/compose.thor.yml:137-149 + compose.orin.yml:30-38`: \#64's token-optional path and #65's worker runner-service wiring are both present in tree; verified by reading the files rather than trusting the PR body
  - seeds: `c4`
- `s4` — `internal/notifier/rundetail.go`: still models only `workflow_digest` with no digest-to-key lookup, so #66's raw-digest legibility defect is unfixed and belongs in this cycle
  - seeds: `c5`
- `s5` — `schemas/workflow/workflow.schema.json ($defs.inputBinding)`: bindings.additionalProperties is $ref $defs/pointer, i.e. every binding value must be a JSON-Pointer string — the object-literal observe convention is schema-invalid exactly as #73 reports
  - seeds: `c6` (rejected)
- `s6` — `.github/workflows/ (adapter-claude-code, adapter-codex, deploy, go, publish, release, tests, web)`: no job references examples/, so nothing compiles the shipped example workflows — this is why the defect survived from t15 to now
  - seeds: `c7`
- `s7` — `internal/compiler/model.go:166 + internal/engine/workflow.go:162,464 + internal/worker/bindings.go:82 + internal/worker/ir.go:167`: the binding value is typed as a string across compiler, engine and worker, so a literal-object binding is a four-site change
  - seeds: `c8`
- `s8` — `live actor list at thor:18080/v1alpha1/actors vs deploy/prod/install-secrets.sh:236 and human-inbox-bridge.service`: company/human-ops resolves to 192.168.1.157:8090 (spark) while both deploy artifacts declare the bridge and tracker thor-only — the split is observable in production right now
  - seeds: `c9`
- `s9` — `examples/pr-upkeep/workflow.yaml (fix :305-358, review :359-430) vs the live actor endpoints`: fix runs on company/developer at spark:8088 and review on company/codex-thor at thor:8086, and the workflow hands a filesystem path between them
  - seeds: `c10`
- `s10` — `examples/pr-upkeep/workflow.yaml edges (:627-686) + adapters/human-inbox tracker.py:40-54 + a tree-wide grep for github_pr_opened`: the #71 redesign already landed in b5cdc5b — triage.empty routes to backoff and the reply/closed observation kinds exist — while `github_pr_opened` appears nowhere, so only the new-PR trigger remains
  - seeds: `c11`
- `s11` — `examples/pr-upkeep/sweep.py (main :429-458, prioritise :357, exit_code_for :373)`: exactly two finding sources feed one prioritised list behind a 0/10/other exit contract; a check-runs source and a Jira source are both additive inside that shape
  - seeds: `c12`
- `s12` — `examples/ (delivery-loop, self-hosting-loop, pr-upkeep, independent-review, cross-machine-proof, notify-message, parallel-live-proof, placement-proof, codex-smoke-pair, newsletter-decompose, workspace-snapshot-hook)`: the node-loop skeleton is already shipped and proven, so a Jira loop is authoring work against existing engine capability
  - seeds: `c13` (rejected)
- `s13` — `adapters/notify + adapters/human-inbox (the credential-holding bridge precedent) vs sweep.py's no-write-credential docstring`: a Jira lane that writes back is an actor outside the deployment, matching #68's reasoning, not a node kind inside the control plane
  - seeds: `c14` (rejected)
- `s14` — `.env (JIRA_CLOUD_API_TOKEN present) + a tree-wide grep for jira (zero hits)`: the Jira surface is greenfield and the credential is incomplete for Basic auth — no email, no site base URL, no existing code to extend
  - seeds: `c15` (rejected)
- `s15` — `examples/pr-upkeep/workflow.yaml sweep node operation (:220-283) and issue #50`: network: full is a documented honest fallback because headspace-cli 0.11.0 rejects egress-allowlist, so adding a Jira host changes nothing about the posture this cycle
  - seeds: `c17`
- `s16` — `tests/deploy/destructiveconfirm_test.go + deploy/prod/install-secrets.sh confirmation protocol`: the single-use windowed confirmation-file gate exists and is behaviorally pinned, so #67 generalizes a shipped mechanism rather than inventing one
  - seeds: `c18`
- `s17` — `docs/deliveries/2026-08-14-dogfooding-baseline.md + run 01KZZSGSWH11J7R7P4V2HPTZZQ + thor:18080 healthz and actor list`: 0 of 35 tasks went through the engine last cycle and exactly one pr-upkeep item reached fix live; the fleet answering healthz with ten actors means the blocker is ergonomics, not capacity
  - seeds: `c20`
- `s18` — `the Jira Cloud site (host from config) /rest/api/3/myself — read-only probe, Basic vs Bearer`: a read-only auth probe settled the shape: Basic authenticates, the same token as Bearer is refused (403), so the missing config value is an account email alongside the token — the concrete account identity is deliberately not recorded here
  - seeds: `c15` (rejected)
- `s19` — `the Jira Cloud site (host from config) /rest/api/3/project/search, /rest/api/3/search/jql, /rest/agile/1.0/board/1/backlog — read-only probes`: one project (SCRUM), board 1 backlog reachable, x-ratelimit-limit 350 per window, and zero issues on the entire site right now — so the loop can be unit-tested against fixtures but not live-proven until the backlog has content
  - seeds: `c23` (rejected)
- `s20` — `examples/ (all eleven flows) + examples/pr-upkeep/sweep.py:56-57 + workflow.yaml:236`: committed demos bake this deployment's hosts, endpoints, org keys and a raw.githubusercontent URL pinned to one org and commit — portable-for-others is a real gap, not a hypothetical one
  - seeds: `c27`
- `s21` — `examples/pr-upkeep/fixtures/sonarcloud-issues.json + adapters/human-inbox/tests/test_tracker.py + the qodo body fixtures`: the sonarcloud fixture carried a personal SonarCloud assignee in four places and the tracker tests a personal GitHub login in three; assignee is parsed by neither sweep.py nor its tests, and the login only needed to be absent from the ignored set, so both were scrubbed without semantic loss (31 and 144 tests still pass)
  - seeds: `c29`
- `s22` — `run 01KZZSGSWH11J7R7P4V2HPTZZQ vs docs/deliveries/2026-08-14-dogfooding-baseline.md`: the '0 of 35' figure counts plan tasks only; one real pr-upkeep item DID execute through the engine and landed SonarCloud fixes as commit b01608c, so the true baseline is 1 out of 35 and the two numbers measure different populations — a distinction the next delivery summary has to keep straight
  - seeds: `c20`
- `s23` — `challenge pass / counter-evidence lens: nodes validate + the live /v1alpha1/workflows/validate endpoint over all 11 committed examples`: every example compiles clean today, including the one #73 reports as broken — the CLI and the live API agree (rc=0 / valid:true, 0 diagnostics), so #73's example half is already delivered
  - seeds: `c49`
- `s24` — `challenge pass / hidden-dependency lens: gh auth status + git config credential.helper on spark, thor and orin`: the pushing host for the fix lane (spark) has keyring-based gh auth and no credential helper, thor has store+auth, orin has neither — #74's recommended mechanism rests on an unverified capability on the one host that must use it
  - seeds: `c51`
- `s25` — `challenge pass / containment lens: grep for preflight/acknowledgement across internal/ and cmd/`: no preflight or acknowledgement surface exists for any of the ten registered actors, so an engine-side gate that fails closed would stop all dispatch on merge day
  - seeds: `c53`
- `s26` — `challenge pass / concurrency lens: adapters/human-inbox/src/human_inbox_bridge/idempotency.py`: the replay store is one JSON file per key under each bridge's own `state_dir`, so it cannot deduplicate across two bridges serving one actor — the tracker/actor identity check is the only real guard
  - seeds: `c55`
- `s27` — `challenge pass / work-supply lens: gh pr list + SonarCloud issues API against this component`: 0 open PRs and 3 unresolved main-branch issues — the entire current supply equals the target, and both PR-scoped finding sources are empty until PRs exist
  - seeds: `c56`
- `s28` — `challenge pass / reversibility + operations lens: deploy/prod/install-secrets.sh`: lines 187 and 196 already merge key-by-key while only the prod lane at line 114 rewrites wholesale — the fix reuses the file's own idiom, but merge-only removes any path for deliberate key deletion
  - seeds: `c57`
- `s29` — `challenge pass / CI-feasibility lens: cmd/nodes/validate through internal/compiler (CLEAN)`: the compile gate needs no control plane — nodes validate compiles offline and all eleven examples pass, so the risk that c38's gate would require a live API does not exist; residual risk is only that CLI and API validation could diverge in future, which this pass found they do not today
- `s30` — `challenge pass / migration lens: migrations/ 0017-0025 + ADR-0002 (CLEAN)`: the expand-only policy is intact and #67's new records fit it; no contract migration is implied by anything in this batch

## Decisions

- \#73 follows its own recommendation C: the pointer form now so the examples compile, plus the CI example-compile gate that prevents recurrence, with the typed literal binding as the real fix
- \#71's residual is in scope: the backoff re-sweep already on main is the issue's own 'strictly worse' option C and does not close it — a `github_pr_opened` observable or an onEvent pickup route still lands
- \#67's gate splits along the all-backends rule: the PROTOCOL lives in the engine — the dispatch gate, the derived preflight record, the actor's proposed acknowledgement, and the confirm verb — while each bridge contributes only its own backend-specific host capabilities through a capability surface the engine composes. A per-bridge protocol was rejected as four implementations of one contract, the exact duplication that let `resolve_actor_row_id` ship as the same bug in three separate lanes; the prompt-composer-only option was rejected because it produces no ledger record, leaving 'the actor was told' an assumption rather than the evidence the issue exists to create
- Demo portability ships IN this batch rather than as a follow-up issue: it pairs with #74 (a filesystem path between hosts is the extreme case of a non-portable value) and with #73's CI gate, which must load every example anyway. The cost is accepted knowingly — it touches all eleven example flows
- CHALLENGE FINDING: #73's headline defect is already fixed on main. examples/pr-upkeep/workflow.yaml binds observe through POINTERS (`merge_observe`, `reply_observe` declared as run-input schema properties at :171-185), and both validators agree it is clean — 'nodes validate' returns rc=0 / 0 errors, and the live API returns valid:true with zero diagnostics. The issue's '2 errors, never validated since t15' was true when filed and is not true now, because PR #70's #71 redesign shipped option B. So claim c6 is a NO-OP: what remains of #73 is the CI gate (c7) and the typed literal binding that would restore the observable's visibility in the graph text
- \#69 item 1 reuses a pattern that already exists in the same file: install-secrets.sh lines 187 and 196 already merge key-by-key (grep for the key, sed in place, else append), while only the prod lane at line 114 still does a wholesale 'cat > prod.env'. The fix is to apply the file's own existing idiom to the prod lane, not to invent a merge mechanism
- The batch ships as several smaller PRs opened early rather than one large PR, so per-PR SonarCloud and Qodo findings exist for the pr-upkeep loop to consume — the loop feeds itself on this cycle's own work, and the two PR-scoped finding sources that return nothing today actually get exercised
- \#74 is sequenced probe-first: verify whether the spark bridge SERVICE can push before committing to git refs; refs if it can (reusing t25's write-tree/commit-tree/update-ref plumbing), an artifact through the S3-compatible store if it cannot. The probe is a plan task, not an assumption

## Open parks

- [unknown_nonblocking] Bridge staleness (#69 item 3) is NOT in this batch, yet a bridge silently running old code would invalidate any measurement this cycle makes — the four claude-code bridges ran four merged tasks behind for most of last cycle and nothing reported it
- [unknown_nonblocking] Whether the same keyring-vs-service credential gap also breaks t25's preserve-branch push leg on every bridge host — same root cause, separately unverified, and it would mean preserve-on-failure has been silently local-only in production

## Resolved vagueness

- [unknown_blocking] \#67's placement is undecided in the issue itself: engine (every actor dispatch), bridges (per-backend, since the facts are backend-specific), or the dispatch prompt composer — the facts are backend-specific but the all-backends rule says the protocol must not be — resolved: Resolved: protocol in the engine, facts contributed by the bridges. See the decision claim capturing the reasoning and the two rejected alternatives.
- [unknown_nonblocking] Jira write-back custody: whether a Jira actor/bridge (the adapters/notify shape) is worth building this cycle, or the loop stays strictly read-only and a human moves the card — resolved: Moved to issue #76 along with the rest of the Jira lane; write-back custody is that issue's open question, not this cycle's.
- [unknown_nonblocking] Jira priority vocabulary does not obviously map onto sweep.py's `_SEVERITY_RANK`, which unifies SonarCloud BLOCKER..INFO with Qodo High/Medium/Low; a third vocabulary either joins that rank or the Jira loop keeps its own ordering — resolved: Moved to issue #76; the Jira priority vs `_SEVERITY_RANK` mapping is recorded there as an open question to settle before the first fixture is recorded.
