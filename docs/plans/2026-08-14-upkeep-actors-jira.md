# Build Plan — upkeep-actors-jira

slug: `upkeep-actors-jira` · status: `exported` · from frame: `upkeep-actors-jira`

> culture-nodes ships the upkeep-actors-jira cycle: every economy-discord-graphs issue whose delivery is evidenced is closed with that evidence cited, the pr-upkeep loop's four upkeep defects are fixed so a real PR flows sweep-to-merge without an operator nursing it, dispatched actors get a clarify-then-commit gate, credential rotation stops destroying keys it does not own, and every committed demo becomes loadable by a deployment that is not this one — with three of this cycle's pr-upkeep items running through culture-nodes rather than beside it. The Jira Cloud node-loop the frame was named for was scoped in full here and deliberately deferred to issue #76.

## Tasks

### t1 — Re-verify all nine batch items against main before any implementation starts

- instruction: For each of #61 #66 #67 #69(items 1-2) #71 #72 #73 #74 and demo portability, re-run the check that established it: grep the tree, run nodes validate, or hit the live API. Record verdict + evidence in a short docs/ note. #71 and #73 are already known partly/mostly delivered — confirm and close rather than plan.
- covers: c50, h34
- acceptance:
  - Each of the nine items has a recorded verdict (real / partly delivered / already delivered) citing a file:line, run id or validator output
  - An item found already delivered is closed with evidence instead of becoming an implementation task

### t2 — Record the issue-hygiene closures already executed this cycle

- instruction: Already executed this session: ten issues closed with citations, #54/#48/#66 commented. Verify the comments still read correctly and record the list in the delivery summary.
- covers: c2, h1, c3, h20, c4, h2
- acceptance:
  - Ten delivered issues are closed, each comment naming a delivery-doc claim row or file:line
  - \#54, #48 and #66 remain open, each carrying a written reason referencing the unverified claim or the tree check

### t3 — Add a credential and personal-identifier lint gate under tests/lint

- instruction: Add tests/lint/`credentialisolation_test.go` mirroring `webhookisolation_test.go` and `github_isolation_test.go`. Patterns: account emails, ATATT/`gho_`/`ghp_` token prefixes, known personal handles. Walk committed paths including examples/ fixtures and adapters/\*/tests.
- covers: c25, h14, c29, h17
- acceptance:
  - The lint fails on a fixture containing an account email, an API token prefix or a personal handle
  - The lint passes over the current tree, and both the sweep and human-inbox suites stay green

### t4 — Notifier renders the workflow name and omits legitimately empty fields (#66)

- instruction: internal/notifier/rundetail.go carries only `workflow_digest`. Add a cached digest-to-key lookup against the workflows read API (the mapping is immutable, so the cache is safe), render 'name (short-digest)', and omit the actor field entirely when empty. Extend `rundetail_test.go`.
- covers: c5, h3, c42, h33
- acceptance:
  - A run detail with a known digest renders as 'name (short-digest)' via a cached digest-to-key lookup
  - A run with no agent actor produces a message carrying no actor field at all, asserted in `rundetail_test.go`

### t5 — CI job compiles every workflow under examples/ (#73's recurrence half)

- instruction: cmd/nodes validate compiles offline through internal/compiler — no control plane needed, verified: all eleven examples pass today. Add a CI job looping over examples/\*\*/\*.yaml. Prove the gate by breaking one example in the PR and showing the red job.
- covers: c7, h5, c38, h30
- acceptance:
  - The job runs 'nodes validate' over every examples/\*\*/\*.yaml with no control plane and passes on the current tree
  - Deliberately breaking one example makes the job fail, demonstrated in the PR before merge

### t6 — Typed literal binding restores observable visibility in the graph text (#73 option A)

- instruction: Four sites: schemas/workflow/workflow.schema.json ($defs.inputBinding.bindings), internal/compiler/model.go:166, internal/engine/workflow.go:162+464 (InputBindings map\[string\]string), internal/worker/bindings.go:82. Allow a binding value to be a pointer string OR a declared literal object; migrate examples/pr-upkeep back to the literal observe form.
- covers: c59, h38
- acceptance:
  - A binding value may be a pointer string or a declared literal object, accepted by schema, compiler, engine and worker
  - An author reading only the workflow text can name what each human node observes; the pr-upkeep example is migrated back to the literal form and still validates

### t7 — sweep.py gains failed CI check runs as a third finding source (#61)

- instruction: Add a check-runs source to examples/pr-upkeep/sweep.py reading GET /repos/{repo}/commits/{sha}/check-runs per open PR. Required failure to HIGH/CRITICAL, optional to MEDIUM. Skip Sonar-named checks (they already arrive via the Sonar feed). Record a fixture under fixtures/ and test in tests/`test_pr_upkeep_sweep.py`.
- covers: c12, h8, c16, h21
- acceptance:
  - A recorded check-runs fixture yields work items carrying check name, PR number and details URL
  - Sonar-named checks are skipped so a quality-gate failure does not double-count, proven by a fixture containing both
  - sweep.py imports nothing outside the 3.12 stdlib and each of the three exit-code branches has a test

### t8 — Tracker refuses to start when its bridge does not serve the actor it observes (#72)

- instruction: At tracker startup resolve the configured actor's `endpoint_ref` from the control plane and compare against the tracker's own bridge URL; exit non-zero naming both when they differ. Note the idempotency store is per-bridge and file-based, so it cannot dedupe across a split deployment — this check is the only guard.
- covers: c9, h6, c40, h31
- acceptance:
  - A tracker pointed at a bridge serving a different actor exits non-zero naming both the actor endpoint and its own bridge URL
  - The refusal is exercised by a unit test, not only by the production fix

### t9 — Probe whether the spark bridge SERVICE can push, and record the verdict

- instruction: Run as the bridge's systemd service identity, NOT an interactive shell. Probed already from a shell: spark has keyring gh auth and no credential.helper, thor has store+auth, orin has neither. The question is whether the service can reach the keyring. Record command and output; the verdict picks t13's mechanism.
- acceptance:
  - The probe runs as the bridge's service identity, not an interactive shell, and its result is recorded with the command and output
  - The verdict selects git-ref handoff or artifact handoff for the #74 task, rather than leaving it assumed

### t10 — Deploy the human-inbox tracker to the host serving company/human-ops (#72)

- instruction: deploy/prod/deploy.sh and install-secrets.sh currently pin the human-inbox bridge and tracker to thor while company/human-ops resolves to spark:8090. Move the tracker unit to the actor's host. Expect a parked task on the merged PR #70 to auto-complete once co-located — that closes last cycle's signal 5.
- depends on: t8
- covers: c37, h29
- acceptance:
  - The tracker unit and the bridge it reads run on the same host, and the tracker starts without tripping the identity refusal
  - A human task parked with an observe declaration auto-completes on merge, its ledger record naming the merge commit and `collection_method`, with no manual submit in the task history

### t11 — Credential rotation merges instead of replacing, with an explicit removal path (#69 item 1)

- instruction: install-secrets.sh:114 is the only wholesale 'cat > prod.env'; lines 187 and 196 already merge key-by-key (grep key, sed in place, else append). Apply that existing idiom to the prod lane, and add an explicit documented removal path so merge-only does not make the file append-only.
- depends on: t10
- covers: c58, h37, c41, h32
- acceptance:
  - Rotating with an externally-issued key present in prod.env leaves that key untouched, proven by a test
  - Removing a key through the documented path actually removes it, proven by a test that adds then removes one
  - The prod lane reuses the key-by-key merge idiom already at install-secrets.sh:187,196 rather than a new mechanism

### t12 — Post-deploy credential audit classifies declared vs present keys (#69 item 2)

- instruction: Compare compose-declared env keys against prod.env at the end of deploy. Classify required / optional-closed-by-default / unknown; fail loudly on a missing required key. The manual version was about five lines of shell and found the missing token immediately.
- depends on: t11
- acceptance:
  - The audit compares compose-declared env keys against prod.env and classifies each as required, optional-closed-by-default, or unknown
  - It runs at the end of deploy and fails loudly on a missing required key, proven against a fixture missing one

### t13 — Portable handle replaces the filesystem path between fix and review (#74)

- instruction: Gated on t9's verdict: git ref reusing t25's write-tree/commit-tree/update-ref plumbing if the service can push, else an artifact through the S3-compatible store. Either way the bare-path binding in examples/pr-upkeep/workflow.yaml goes away, and an unavailable handle must surface as a named domain outcome rather than a 403 at review.
- depends on: t9
- covers: c10, h7, c52, h35, c36, h28
- acceptance:
  - A run whose fix and review actors sit on different hosts completes, the review lane quoting content obtainable only by fetching the fix lane's handle
  - The run's attempt rows name two different hosts
  - A fix host that cannot produce the handle yields a named domain outcome identifying the missing capability, not a 403 at the review node

### t14 — Clarify-then-commit gate: engine-side protocol, per-actor and default-off (#67)

- instruction: Protocol in the engine, facts from the bridges. Preflight is a derived record composed from host capabilities plus the task declaration; the acknowledgement is a proposed record from the actor via a dispatch confirm verb; single-use and windowed like tests/deploy/`destructiveconfirm_test.go`. Per-actor, default-off; enabling it for a bridge lacking the surface is refused at config time. Any new record type is an expand migration per ADR-0002.
- depends on: t6
- covers: c18, h10, c53, h36, c54, h39
- acceptance:
  - A derived preflight record and a proposed actor acknowledgement exist as ledger records before the first billable turn
  - A dispatch whose preflight was never acknowledged does not proceed
  - Enabling the gate for a bridge that does not advertise the capability surface is refused at configuration time, and the ten existing actors dispatch unchanged
  - Any new record type ships as an expand migration an N-1 binary ignores safely

### t15 — Capability surface on all four bridges (all-backends rule) (#67)

- instruction: All-backends rule: claude-code, codex, colleague and notify each advertise host capabilities through the identical protocol shape. Do NOT inline per-bridge logic — that is the duplication that let `resolve_actor_row_id` ship as the same bug in three lanes; share the helper.
- depends on: t14
- acceptance:
  - claude-code, codex, colleague and notify each advertise host capabilities through the same protocol shape
  - A bridge that does not advertise the surface leaves its actor dispatching exactly as before

### t16 — Committed demos become loadable by a deployment that is not this one

- instruction: Nine of eleven examples name thor/orin/spark or 192.168.1.x; sweep.py pins `GITHUB_REPO` and `SONAR_COMPONENT_KEY`; workflow.yaml:236 fetches its script from a raw.githubusercontent URL pinned to one org and commit. Move each to run input or documented config. The swept repo stays hardcoded as a blast-radius boundary but gets a comment at the constant and a README line.
- depends on: t7, t13, t6
- covers: c26, h15, c30, h22
- acceptance:
  - Loading any example into a different deployment requires changing only documented configuration, never editing the graph
  - A reviewer can point at each environment-specific value and say where it comes from
  - The swept repo stays hardcoded but is documented at the constant and in the README as the one value a new operator changes

### t17 — Three pr-upkeep items complete the full loop, counted from the ledger

- instruction: Open this cycle's work as several smaller PRs EARLY so per-PR Sonar and Qodo findings exist for the sweep — today there are 0 open PRs and only 3 main-branch findings, so supply is exactly the target with no slack. Write one ledger query and run it twice: once now for the baseline of one, once at cycle end.
- depends on: t10, t13, t7
- covers: c20, h11, h18, c35, h27
- acceptance:
  - The cycle's work is opened as several smaller PRs early, so per-PR Sonar and Qodo findings exist for the sweep to consume
  - Baseline and final counts come from the SAME ledger query over runs and attempts, both recorded
  - Three items complete sweep to human-merges-pr; a partial completion is reported as partial, never rounded up

### t18 — Delivery summary: planned versus actual, with every scope item accounted for

- instruction: Use the /summarize-delivery skill. Account for all nine scope items, map every announcement and after-state clause to a named artifact or record it as not achieved, and state the final engine-executed count against the baseline of one using t17's query.
- depends on: t17, t16, t15, t12, t5, t4, t3, t2, t1
- covers: c1, h19, c22, h12, c31, h23, c32, h24, c33, h25, c34, h26
- acceptance:
  - Every one of the nine scope items either ships or is recorded as dropped with a reason
  - Each announcement clause and after-state clause maps to a named artifact, or is recorded as not achieved rather than softened
  - The end-of-cycle count of real items through the engine is stated against the baseline of one, from the same query

## Risks

- [unknown_nonblocking] Bridge staleness is not in this batch, yet a bridge silently running old code invalidates any measurement the cycle makes — four claude-code bridges ran four merged tasks behind for most of last cycle and nothing reported it
- [unknown_nonblocking] The same keyring-vs-service credential gap may mean t25's preserve-branch push leg has been silently local-only in production; unverified, and it would weaken a delivery claim already made (task t9)
- [unknown_nonblocking] Work supply for the three-item target is exactly three main-branch findings today with zero open PRs; if the early-PR mechanism does not generate PR-scoped findings, t17 cannot reach its number (task t17)
- [follow_up] \#67 is the batch's largest item — an engine protocol plus a capability surface on four bridges — and the challenge pass raised its cost estimate; it is the first candidate to drop if the cycle runs long (task t14)
