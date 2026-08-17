# Build Plan — easy-pickings batch

slug: `easy-pickings-batch` · status: `exported` · from frame: `easy-pickings-batch`

> Ten open issues close in one batch without any of them being triaged first: each one's fix was already written down in the issue itself, so the work was to apply it, prove it, and close the record

## Tasks

### t1 — Reproduce all eight defects at HEAD and record the reproducing command per issue

- instruction: OPERATOR LANE. Run each reproduction read-only and paste the command + output. Already run this session: flake8 adapters (7 findings), the merge-gate key-set read, capture() read. Still to reproduce: #146 (capture against an unreachable host), #148 (a matrix with a bogus key), #153 (a reordered matrix). #123 and #131 are a missing file and a wrong sentence — say so rather than claiming a reproduction.
- covers: c2, h2, c28, h28
- acceptance:
  - each of #123 #131 #134 #146 #148 #150 #152 #153 has either a recorded command that demonstrates the defect at HEAD, or an explicit note that reproduction is not meaningful (a missing file for #123, a wrong sentence for #131)
  - any issue that turns out to be already fixed leaves the batch and is closed as verify-then-close instead

### t2 — \#153: make merge-gate's `local_outcome` order-independent, proved by a failing-first shuffle test

- instruction: WORK PACKAGE A, worktree ../.worktrees.culture-nodes/wp-a, branch easy-pickings/wp-a. Write the shuffle test FIRST and watch it fail, then rewrite `local_outcome` to count before deciding. Do not also do t3 yet.
- depends on: t1
- covers: c3, h3
- acceptance:
  - tests/`test_merge_gate.py` permutes a \[failing, `instrument_unavailable`\] matrix and asserts `changes_required` for every permutation
  - that test fails against the pre-fix scripts/merge-gate.py and passes after
  - `local_outcome` counts failures across all entries before returning, mirroring internal/handover.GateResults.Outcome

### t3 — \#148: refuse unknown keys in the pinned gate matrix and wire the check into validate-examples.sh

- instruction: WORK PACKAGE A, same worktree and agent as t2 — both rewrite scripts/merge-gate.py and tests/`test_merge_gate.py`, so they must not be split across agents. Land t2 first, then add the unknown-key refusal on top.
- depends on: t2
- covers: c5, h5
- acceptance:
  - a gate entry carrying a key outside the twelve the parser reads is refused, naming the offending key and listing the valid set in the hint
  - scripts/validate-examples.sh exercises the refusal, so a malformed matrix fails in CI rather than on a live dispatch
  - the twelve-key set is asserted by a test, so a new key added to the parser without adding it to the set fails

### t4 — \#152a: replace the pinned adapter-lint command with explicit per-adapter src/tests paths

- instruction: WORK PACKAGE B, worktree ../.worktrees.culture-nodes/wp-b, branch easy-pickings/wp-b. Copy the path style from .github/workflows/adapter-codex.yml. Changing this file changes the workflow content digest — see t6.
- depends on: t1
- covers: c4
- acceptance:
  - examples/merge-gate/workflow.yaml's adapter-lint command names explicit paths and no longer walks adapters/\*/.venv
  - the command yields the same verdict on a checkout with adapters/\*/.venv present as on one without

### t5 — \#152b: clear the seven real flake8 findings the .venv noise was hiding

- instruction: WORK PACKAGE B, same worktree as t4. Fix the seven findings measured at HEAD, not the twelve the issue counted. Do not add a CI workflow for the three ungated adapters — that is parked (v1), not in scope.
- depends on: t1
- covers: h4
- acceptance:
  - 'uv run flake8 adapters/\*/src adapters/\*/tests' returns 0 findings
  - the two late imports in adapters/human-inbox/src/`human_inbox_bridge`/nudge.py are either moved to the top or carry a justification, not silenced blindly

### t6 — State the digest gap: the repo fix does not reach thor's running gate

- instruction: OPERATOR LANE, after WP-A and WP-B merge. Write the digest-gap sentence into both closing comments before closing either issue.
- depends on: t3, t4
- covers: c32, h32, c15, h15
- acceptance:
  - \#152's and #148's closing comments each contain a sentence naming that thor's merge gate still runs the pre-fix pinned digest and a republish is owed
  - \#152's closing comment states that only the explicit-paths half is done and leaves the structural half (refusing git-untracked paths / gating a clean export) on the record as an open successor

### t7 — \#146: stop toolchain-baseline.sh destroying a baseline on a failed probe

- instruction: WORK PACKAGE C, worktree ../.worktrees.culture-nodes/wp-c, branch easy-pickings/wp-c. Copy check()'s existing drift-accumulator shape from the same file rather than inventing one.
- depends on: t1
- covers: c6, h6
- acceptance:
  - a capture against an unreachable host leaves the committed baseline byte-identical
  - the command exits non-zero and names every host it could not measure
  - the JSON is written to a temp file and validated as parseable before it replaces the baseline

### t8 — \#123: one script that runs every lint job CI runs, called by CI itself

- instruction: WORK PACKAGE C, same worktree as t7. Scope is the three jobs literally named lint; whether go vet and web.yml's webglass belong is parked (v4) — do not widen it unilaterally. If you touch a shared adapter bridge module, format once and copy to all five.
- depends on: t1
- covers: c8, h8
- acceptance:
  - scripts/lint-all.sh carries all three lint jobs verbatim: root scope, adapter-codex from the repo root, adapter-claude-code from its own directory
  - the three lint workflows invoke the script rather than duplicating its commands, so a green local run and a red CI lint job cannot coexist
  - shared adapter bridge modules stay byte-identical across all five adapters afterwards (tests/lint/ passes)

### t9 — \#131: name the fourth doctor check in CLAUDE.md

- instruction: WORK PACKAGE D, worktree ../.worktrees.culture-nodes/wp-d, branch easy-pickings/wp-d. One sentence in the Mesh identity paragraph.
- depends on: t1
- covers: c7, h7
- acceptance:
  - CLAUDE.md's Mesh identity paragraph names `unprivileged_userns` alongside the backend-to-prompt-file mapping, prompt-file-present and skills-present
  - the count in the prose matches what 'uv run teken cli doctor . --strict' reports

### t10 — \#150: document the four inbound dial-in routes and regenerate the JSON companion

- instruction: WORK PACKAGE D, same worktree as t9. This is NOT a doc-only change: run 'go run ./internal/api/testdata/regen-openapi-json' and commit openapi.json alongside the YAML, then 'go test ./internal/api/...'. `contract_test.go` has no exemption list, so the four routes will be exercised.
- depends on: t1
- covers: c9, h9, c29, h29
- acceptance:
  - all four routes appear in api/openapi/openapi.yaml with the 204-empty-body idle response, both authentication headers, the mailbox envelope, and issuance's reveal-once semantics stated explicitly
  - api/openapi/openapi.json is regenerated from the YAML in the same change and TestOpenAPIJSONIsTheYAMLRendered passes
  - 'go test ./internal/api/...' passes with the four new documented routes exercised by `contract_test.go` — not skipped, not exempted

### t11 — \#134: scrub relay inputs in the install-secrets test harness, and say 'contained' not 'fixed'

- instruction: WORK PACKAGE E, worktree ../.worktrees.culture-nodes/wp-e, branch easy-pickings/wp-e. Scrub in the harness, which already controls HOME per host. The relay at install-secrets.sh:554 stays — the honest word is contained.
- depends on: t1
- covers: c11, h11, c33, h33
- acceptance:
  - running the harness with `DISCORD_WEBHOOK_URL` set in the invoking environment produces prod.env files that do not contain its value, asserted by a test
  - \#134's closing comment uses the word contained and names install-secrets.sh:554 as the surviving relay, rather than claiming the relay was removed

### t12 — \#94 (backfill): report git-metadata writability as a host fact in the capability surface

- instruction: WORK PACKAGE E, same worktree as t11. preflight.py is byte-identical across five adapters: edit one, copy to the other four, then re-run tests/lint/. Formatting them independently is how #123 happened.
- depends on: t1
- covers: c34, h22
- acceptance:
  - `git_metadata_writable` joins `HOST_KEYS` with the three-valued vocabulary supported / unsupported-by-sandbox / not-probed
  - the value is measured by attempting a write under .git, never derived from the sandbox mode name
  - preflight.py is formatted once and copied byte-identically to all five adapters; tests/lint/ passes

### t13 — \#117 (backfill): audit every site that stamps an origin on a caller-supplied actor id

- instruction: DISPATCH TO codex-thor via the nodes-operator skill as READ-ONLY analysis (c25). The bridge write path is unproven (#18), so this returns an audit, not a patch. Its completion claim is a claim, not evidence — adjudicate the findings yourself.
- depends on: t1
- covers: c35, h23
- acceptance:
  - the audit names every origin-stamping site by file and line
  - for each site it states whether the actor id is caller-supplied and whether it is checked against the actor registry — a site list with no per-site verdict is not an audit
  - dispatched to codex as read-only analysis; the returned claims are adjudicated by the operator, since a completion claim is not evidence

### t14 — Close #66 and #151 as verify-then-close, citing what already shipped

- instruction: OPERATOR LANE. Cite internal/notifier/`rundetail_test.go`:222 for #66. For #151, re-read deploy/prod/issue-dialin-credential.sh before closing and say whether the post-deploy re-issue step is covered or needs a successor.
- depends on: t1
- covers: c26, h26, c27, h27
- acceptance:
  - \#66's closing comment cites internal/notifier/`rundetail_test.go`:222 and no commit in the batch touches internal/notifier/ or internal/notify/
  - \#151's closing comment cites deploy/prod/README.md:14-16 and install-secrets.sh:436-455, and states explicitly whether the post-deploy re-issue step is already covered by issue-dialin-credential.sh or needs a successor issue

### t15 — Merge the four PRs one at a time, gate change first

- instruction: OPERATOR LANE. One PR open at a time. PR1 (WP-A + WP-B) merges and goes green on main before PR2 opens, because it changes the program that gates the rest. Bump the version as the last commit before opening each PR.
- depends on: t3, t5, t6, t7, t8, t9, t10, t11, t12, t13
- covers: c30, h30, c31, h31
- acceptance:
  - PR1 is merged and its gate change verified green on main before PR2 opens
  - no two batch PRs are open against main at the same time
  - no PR is blocked by a version-check or CHANGELOG conflict with another batch PR, and the batch can name the order they merged in

### t16 — Verify the no-prod-touch boundary by absence

- instruction: OPERATOR LANE. Verify by absence across the batch's whole command history. If a selected issue turns out to need a prod touch, stop and run /deviate.
- depends on: t1
- covers: c16, h16, c23, h21
- acceptance:
  - no ssh, no deploy.sh, no migration apply and no bridge restart appears anywhere in the batch's command history
  - if a selected issue turns out to need a prod touch to close, the batch stops and runs /deviate rather than widening the boundary to fit it

### t17 — Write the delivery record and regenerate the triage table

- instruction: OPERATOR LANE, last. Regenerate with scripts/triage-report.py, never by hand. Ten fixed and two found-already-fixed are different facts — do not present twelve closures as twelve pieces of work.
- depends on: t14, t15, t16
- covers: c1, h1, c18, h17, c19, h18, c20, h19, c36, h34, c37, h24, c38, h25
- acceptance:
  - docs/triage/open-issues.md is regenerated by scripts/triage-report.py (never hand-edited) and has twelve fewer rows
  - the record separates the ten fixed from the two found-already-fixed, rather than presenting twelve closures as twelve pieces of work
  - each of the four success signals is run as a command and its output pasted in; the digest gap is stated in prose because no command can show it
  - \#50 is named as considered-and-rejected with the reason, in one line
  - if any of the ten needed an owner decision during execution, that issue is named as the counterexample and the announcement claim is weakened rather than restated

## Risks

- [unknown_nonblocking] five of the eight original fixes were verified by reading, not by reproducing; t1 is what closes that gap and it may find more already-fixed issues, shrinking the batch again (task t1)
- [unknown_nonblocking] \#150 activates live contract assertions against four authenticated routes; a transcription error surfaces as a red CI job rather than a doc nit (task t10)
- [unknown_nonblocking] \#123's lint-all.sh scope is ambiguous — the three jobs named 'lint', or every check that can go red including go vet and web.yml's webglass (task t8)
- [unknown_nonblocking] \#117 is an audit whose output is unknown until it runs: it may yield a record, a small fix, or a finding too large for this batch (task t13)
