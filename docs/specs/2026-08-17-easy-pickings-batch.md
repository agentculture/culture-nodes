# easy-pickings batch

> Ten open issues close in one batch without any of them being triaged first: each one's fix was already written down in the issue itself, so the work was to apply it, prove it, and close the record
> instruction: Execute the four PRs in order PR1..PR4 (c24). Any issue that develops a decision mid-flight leaves the batch through /deviate rather than being decided in-session.

## Audience

- the operator running culture-nodes cycles, who currently reads a 56-row triage table and cannot tell from it which rows are ready to execute; and the next reader of the backlog, who should be able to see the batch shrink the tail without wondering what was skipped

## Before → After

- Before: 56 open issues all carry a disposition, and the ones whose fix is fully written down sit in the same undifferentiated list as the ones that need an owner to choose. docs/deliveries/close-the-backlog-bootstrap-honesty.md records the backlog growing net-positive every day since 2026-08-08, so the determined fixes are being outpaced by new findings rather than being cheap to clear
- After: TWELVE issues close: ten pieces of work across four themed PRs (#131 #146 #148 #150 #152 #153 #123 #134 plus backfills #94 #117), and two verify-then-close closures the challenge pass found already shipped (#66 #151). Each PR is green on the repo's own CI and every closure cites the change or the test that closed it. The triage table regenerates to 44 open dispositions, and the batch's boundary is on the record: what was NOT taken and why — a decision, a diagnosis, a live prod mutation, or a fix that was already there
  - instruction: Regenerate with python3 scripts/triage-report.py after the last PR merges; do not hand-edit the table.

## Why it matters

- the expensive part of a backlog is deciding, not typing. Ten issues whose fix is already written down are pure execution, and leaving them in the queue makes the queue look harder than it is — which is how a tail that could close in four PRs reads as a program of work

## Requirements

- the batch's selection rule: an issue qualifies only if docs/triage/open-issues.md already carries a disposition for it AND that disposition names an action rather than a decision — the table has 56 dispositions, so 'without triage' means 'triage already happened', not 'skip it'
  - instruction: Before touching code, re-run scripts/triage-report.py and diff the table: if a selected issue's disposition has shifted to a choice, drop it from the batch rather than deciding it.
  - honesty: each of the ten can be pointed at a row in docs/triage/open-issues.md whose disposition reads as an imperative, and each excluded near-miss at one that reads as a choice
- \#153: scripts/merge-gate.py:375-394 `local_outcome` returns inside the loop, so matrix order decides the verdict; the issue supplies the corrected count-then-decide body verbatim, mirroring internal/handover.GateResults.Outcome, plus a shuffle test in tests/`test_merge_gate.py`
  - instruction: Rewrite `local_outcome` to count failures across ALL entries before returning; add tests/`test_merge_gate.py`::`test_local_outcome_is_order_independent` permuting a \[failing, unavailable\] matrix and asserting `changes_required` for every permutation.
  - honesty: a test shuffles the gate entries and asserts an unchanged outcome, and that test FAILS against the pre-fix implementation — order-independence is proved by a failing-first test, not asserted
- \#152: examples/merge-gate/workflow.yaml:331-337 pins command \['uv','run','flake8','adapters'\], which walks adapters/\*/.venv; naming explicit paths the way adapter-codex.yml does is a one-entry edit. The real findings it hides are live today: 'uv run flake8 adapters/\*/src adapters/\*/tests' returns 7, all in adapters/human-inbox (F401 x5, E402 x2)
  - instruction: Replace the adapter-lint command with the explicit per-adapter src/tests paths the CI workflows already use; then clear the 7 flake8 findings in adapters/human-inbox (drop the unused imports; move or justify nudge.py's two late imports).
  - honesty: after the fix 'uv run flake8 adapters/\*/src adapters/\*/tests' returns 0 findings, and the pinned matrix command yields the same verdict on a checkout with adapters/\*/.venv present as on one without
- \#148: merge-gate.py reads exactly twelve gate keys (gate, suite, instrument, requires, reaches, `responsible_for`, command, measurement, repair, cwd, `timeout_seconds`, `version_command` — lines 246-353); anything else is accepted in silence, so a known-key set plus a refusal is a closed, mechanical change
  - instruction: Add a module-level frozenset of the 12 keys the parser reads; refuse any gate entry carrying another key, naming it and listing the valid set in the hint. Extend scripts/validate-examples.sh to run each example's matrix through that validation.
  - honesty: a matrix entry carrying a key the parser does not read is refused by name, and the refusal is exercised by scripts/validate-examples.sh rather than only by a live dispatch
- \#146: scripts/toolchain-baseline.sh:93-101 capture() redirects measure output straight into the baseline file, truncating it before the probe's status is known, and the loop swallows failure; temp-file-then-mv plus a per-host failure tally is ~10 lines in one function
  - instruction: In capture(), measure to a temp file, verify it parses as JSON, then mv -f into the baseline path; accumulate per-host failures and exit non-zero naming every host that could not be measured.
  - honesty: a capture against an unreachable host leaves the committed baseline byte-identical and exits non-zero, naming the host it could not measure
- \#131: CLAUDE.md's 'Mesh identity' section describes three doctor checks; `culture_nodes`/cli/`_commands`/doctor.py implements a fourth (`_userns_check`) and teken cli doctor --strict reports checks=4 — a one-sentence correction the owner already confirmed against live output in the issue
  - instruction: Edit CLAUDE.md's Mesh identity paragraph to name `unprivileged_userns` as the fourth check alongside the backend-to-prompt-file mapping, prompt-file-present and skills-present.
  - honesty: CLAUDE.md names the fourth check rather than only counting to four, and the number matches what 'uv run teken cli doctor . --strict' reports
- \#123: no scripts/lint-all.sh exists today (ls scripts/ shows 22 scripts, none of them); the three lint jobs live in .github/workflows/tests.yml, adapter-codex.yml and adapter-claude-code.yml, and CLAUDE.md already carries both invocation styles verbatim — a script that IS what CI calls is transcription, not design
  - instruction: Write scripts/lint-all.sh carrying all three lint jobs verbatim (root scope; adapter-codex from the repo root; adapter-claude-code from its own directory), then have each workflow invoke it with a job selector so CI and local run the same text.
  - honesty: the three lint workflows invoke scripts/lint-all.sh (or a per-job subset of it), so a green local run of that one script and a red CI lint job cannot coexist
- \#150: internal/api/server.go:424-431 registers exactly four undocumented inbound routes (POST inbound/poll, inbound/{id}/complete, inbound/credentials, inbound/credentials/revoke) while GET /v1alpha1/dial-in-presence at openapi.yaml:721 and its three schemas at :4471 are already specified — the write side is transcribed from Go types that exist
  - instruction: Add the four inbound operations to api/openapi/openapi.yaml beside the existing dial-in-presence path, transcribing schemas from internal/api/inboundcredentials.go and the inbound poll handler; pin the 204-empty-body idle response, both auth headers, and reveal-once explicitly.
  - honesty: all four inbound routes appear in openapi.yaml with the 204-empty-body idle response, both authentication headers, and issuance's reveal-once semantic stated explicitly — and the contract sweep that walks documented paths now walks them
- \#134: deploy/prod/install-secrets.sh:313 relays `DISCORD_WEBHOOK_URL` from the invoking environment; the owner decision (note-and-continue, no rotation) is already recorded on the issue, and the named residual is to scrub relay inputs in the test harness that already controls HOME per host
  - instruction: Unset the relay-input variables inside the install-secrets test harness itself (it already controls HOME per host), and add a test asserting a generated prod.env contains no value that was present only in the invoking environment.
  - honesty: running the install-secrets test harness with `DISCORD_WEBHOOK_URL` set in the invoking environment produces prod.env files that do not contain its value
- \#66 is ALREADY SHIPPED at HEAD and leaves the batch as a verify-then-close: internal/notifier/rundetail.go carries workflowLabel(), shortDigest() and workflowKeyCache; internal/notify/payload.go:109,119 omits an empty actor from both the description line and the embed field; and internal/notifier/`rundetail_test.go`:222 asserts the exact 'parallel-live-proof (8d4c768)' format the issue asked for. Close it citing the test — do not re-implement it
  - instruction: Close #66 with the test citation; add nothing.
  - honesty: \#66's closing comment cites internal/notifier/`rundetail_test.go`:222 by name, and no commit in the batch touches internal/notifier/ or internal/notify/
- \#151 is LARGELY SHIPPED at HEAD: deploy/prod/README.md:14-16 already orders install-secrets.sh before deploy.sh; install-secrets.sh:436-455 installs `NODES_INBOUND_ISSUANCE_TOKEN_SECRET` add-if-absent outside the `FORCE_PROD` block (exits 3 when present, citing #124 and #133); and deploy/prod/issue-dialin-credential.sh implements the re-issue step with a refusal that names the missing bearer. What remains is at most a named post-deploy step: re-issue whatever 'nodes actors dial-in --absent-only' lists
  - instruction: Re-read issue-dialin-credential.sh before closing; if it already covers step 3, close outright, otherwise close the ordering half and open a successor for the re-issue step.
  - honesty: \#151's closing comment cites deploy/prod/README.md:14-16 and install-secrets.sh:436-455, and states explicitly whether the post-deploy re-issue step still needs writing or was already covered by issue-dialin-credential.sh
- the selection rule gains a verification step it did not have: an issue enters the batch only after its defect is REPRODUCED at HEAD, never after its issue text and triage disposition are merely read. Two of the ten were already fixed, and the triage table's own verify-then-close bucket shows this failure mode was already known to the repo before this pass found it again
  - instruction: Reproduce first, fix second, per issue. Record the reproducing command in the PR body.
  - honesty: for each of the eight fixes, the batch can point at the command that reproduced the defect at HEAD before the fix landed — and for #123 and #131, where reproduction is a missing file and a wrong sentence, it says so rather than claiming a reproduction it did not run
- \#150 is not a doc-only change: internal/api/`openapi_json_test.go` requires api/openapi/openapi.json be regenerated from the YAML (go run ./internal/api/testdata/regen-openapi-json), and internal/api/`contract_test.go` walks the spec's path+method+operationId inventory against a live server with NO exemption list — so documenting the four inbound routes activates live contract assertions against four authenticated endpoints
  - instruction: Run go run ./internal/api/testdata/regen-openapi-json and commit openapi.json in the same change as the YAML.
  - honesty: after #150 lands, 'go test ./internal/api/...' passes with the regenerated openapi.json and the four new documented routes exercised by `contract_test.go` — not skipped, not exempted
- PR1 merges and is verified before PR2-4 open: it changes scripts/merge-gate.py, the program that gates the batch's own PRs, and c22 wants #153's fix proved on a real branch — a gate defect shipped in PR1 would otherwise block the remaining three
  - instruction: Open PR2 only after PR1 is merged and CI is green on main.
  - honesty: PR1 is merged and its gate change verified before PR2 opens; the batch can name the order the four PRs merged in
- the four PRs merge SEQUENTIALLY, not concurrently: each bumps the version in pyproject.toml and prepends a CHANGELOG.md entry, so four simultaneously open PRs collide on both files and the version-check job blocks whichever loses the race
  - instruction: One PR open at a time; bump the version as the last commit before opening.
  - honesty: no two batch PRs are open against main at the same time, and no PR in the batch is blocked by a version-check or CHANGELOG conflict with another batch PR
- BACKFILL 1 — #94: the capability surface reports `writable_paths` but not whether .git is writable. Verified unfixed at HEAD per c28: `HOST_KEYS` in the byte-identical preflight.py carries no `git_metadata_writable`, and the codex bridge's `writable_git` is a DISPATCH parameter (`async_runner.py`:147, `codex_cli.py`:101, server.py:600) that opts a session in — not a host fact the surface reports. The issue names the shape: three-valued supported / unsupported-by-sandbox / not-probed, measured by trying it rather than inferred from the sandbox mode
  - instruction: Add `git_metadata_writable` to `HOST_KEYS` in preflight.py with the three-valued vocabulary, probe it by attempting a write under .git, then copy the file byte-identically to all five adapters and re-run tests/lint/ (c17).
  - honesty: a dispatch whose .git is NOT writable and one whose .git IS writable report different `git_metadata_writable` values from the same bridge — the fact is measured per dispatch, not derived from the sandbox mode name
- BACKFILL 2 — #117: audit every place the ledger stamps an origin on a caller-supplied actor id. Verified unperformed at HEAD per c28: no audit record exists under docs/, and no OriginFor/deriveOrigin-style helper exists, so the audit's first job is establishing the site list. Routed to codex as read-only analysis under c25
  - instruction: Dispatch to codex-thor or codex-orin as read-only analysis (c25); the operator adjudicates the returned claims and lands either a fix or a docs/ record. Completion claims from the actor are claims, not evidence (PRD 10.4).
  - honesty: the audit names every origin-stamping site by file and line and states, per site, whether the actor id is caller-supplied and whether it is checked against the actor registry — a site list with no verdict per site is not an audit

## Honesty conditions

- none of the ten required an owner decision during execution: if any turns out to need a choice, it is pulled from the batch and recorded as a deviation, not resolved in-session
- \#152 is closed only for the explicit-paths half, and the structural half (refusing untracked paths / gating a clean export) is left on the record as an open successor rather than quietly dropped
- no command run in service of the batch writes to thor or orin, applies a migration, or restarts a bridge — the batch's whole verification story is 'git push and read CI'
- every PR in the batch carries a version bump, and any shared adapter bridge module it touches is byte-identical across all five adapters afterwards (tests/lint/ passes)
- an operator who did not run this batch can read the regenerated triage table plus the delivery record and tell which rows closed and which were deliberately left, without asking anyone
- the 56-row count and the net-positive backlog growth are both citable — the first from docs/triage/open-issues.md's own footer, the second from docs/deliveries/close-the-backlog-bootstrap-honesty.md
- each of the ten closes without an owner being asked a question during execution; if any needs one, that issue is the counterexample and the claim is weakened in the delivery record rather than restated
- if any selected issue turns out to need a prod touch to close, it leaves the batch and is recorded as a deviation rather than the boundary being widened to fit it
- the delivery record contains a sentence naming that thor's merge gate still runs the pre-fix pinned digest, and #152/#148's closing comments say the same
- \#134's closing comment uses the word contained and names install-secrets.sh:554 as the surviving relay, rather than claiming the relay was removed
- the delivery record names #50 as considered-and-rejected with the reason, so the next batch can see the check was run
- the regenerated triage table has twelve fewer rows, and the delivery record separates the ten fixed from the two found-already-fixed rather than presenting twelve closures as twelve pieces of work
- all four signals are measured, not asserted: the table is regenerated and committed, the `changes_required` verdict comes from a test that failed before the fix, the flake8 count is a command anyone can re-run, and the digest gap is stated in prose because no command can show it

## Success signals

- docs/triage/open-issues.md regenerates with twelve fewer rows; the merge gate's local aggregate returns `changes_required` rather than `measurement_incomplete` on a matrix ordered unavailable-before-failing (#153, proved by a test that fails first); 'uv run flake8 adapters/\*/src adapters/\*/tests' returns 0 findings; and the delivery record states that the gate on thor still runs the old pinned digest
  - instruction: Run each signal as a command and paste its output into the delivery record; a signal with no output is not a signal.

## Scope / boundaries

- \#152 has two fixes and this batch takes only the first: explicit paths in the pinned matrix. The structural version — refusing a command whose argument list contains a git-untracked path, or running gates against a clean export of the commit — is a design change to the gate's contract and stays open after the batch
  - instruction: When closing #152, state in the closing comment that only the explicit-paths half is done and open (or leave open) the structural half rather than closing the issue outright.
- nothing in the batch mutates production: #136 (five actors at a dead LAN address), #128 (redeploy thor and the codex bridges), #121 (apply migration 0036) all need a deploy or a live re-registration on thor/orin. Every selected issue is verifiable by the repo's own CI, which is what makes 'easy' true rather than merely 'small'
  - instruction: Verify by absence: no ssh, no deploy.sh, no migration apply, no bridge restart appears anywhere in the batch's command history.
- the batch is repo-only and CI-provable: no deploy, no live re-registration, no migration applied, no bridge restarted. An issue that can only be closed by touching thor or orin is out by construction, however small it looks
  - instruction: If a selected issue turns out to need a prod touch, stop and run /deviate rather than widening the boundary to fit it.
- \#152 and #148 edit examples/merge-gate/workflow.yaml's pinned matrix, which examples/merge-gate/README.md:169,191-195 states is part of the PUBLISHED workflow's content digest — so the repo fix does not change the gate running on thor until that workflow is republished, and republishing is a prod touch c16/c23 forbid. The batch fixes the source of truth and leaves production on the old digest; the delivery record must say exactly that rather than implying the gate is fixed everywhere
  - instruction: Write the sentence before closing either issue, not after.
- \#134's fix is CONTAINMENT, not elimination: scrubbing relay inputs in the test harness stops the harness reproducing the near-miss, but deploy/prod/install-secrets.sh:554 still relays `DISCORD_WEBHOOK_URL` from whatever environment invokes it, so a probe run outside the harness reproduces it exactly. The delivery record says 'contained in the harness', never 'fixed'
  - instruction: If the closing comment cannot honestly say 'fixed', it says 'contained' and the issue stays open or gets a successor.
- \#50 was considered as a backfill and rejected under c28: docs/deviations/2026-08-15-headspace-egress-allowlist.md already exists, so its 'record interim deviation' half is done and only the cross-repo tracking remains — it would have been a third stale pick, and the pass caught it before it entered
  - instruction: One line in the delivery record; no separate artifact.

## Non-goals

- no issue whose disposition still contains a choice enters this batch: #125 ('decide where repo comes from'), #129 ('decide whether the orphan sweep is a deliberate trade-off'), #133 ('either the rotation refreshes the URL, or the audit reports it'), #135 ('settle whether `env_has` or `env_get` is right'), #119 ('pick one of four options'), #140 (rename) — every one of those is a decision wearing a bug's clothes
- \#116 is excluded despite sitting in the bug tail: the issue says in its own words 'Observed, not diagnosed' and points at where the defect is probably NOT (actorstats.go:452-466 reads correctly), so it needs an investigation of how `started_at` is stamped on the async callback path before it needs a patch

## Assumptions

- the batch obeys the repo's two standing merge constraints without needing to rediscover them: every PR bumps the version (the version-check job blocks merge otherwise), and any edit to a shared adapter bridge module is formatted once and copied to all five, because tests/lint/ enforces byte-identity and per-adapter formatting is exactly how #123 happened

## Scope exploration

- `s1` — `docs/triage/open-issues.md (generated by scripts/triage-report.py)`: 56 open issues already carry a bucket and a disposition; the buckets separate the batch cleanly — 'bug tail' and 'verify-then-close' hold determined fixes, while 'owner decisions' and 'large bets' hold choices. 'Without triage' is satisfiable because triage is committed, not because it is skipped
  - seeds: `c2`, `c13`
- `s2` — `gh issue list --state open (57 issues) and the bodies of #66 #116 #123 #125 #131 #134 #146 #148 #150 #151 #152 #153`: 56 open issues (the count docs/triage/open-issues.md reports, verified against gh issue list); read the bodies of #66 #116 #123 #125 #131 #134 #146 #148 #150 #151 #152 #153. The qualifying issues state their own fix — #153 supplies the corrected function body, #152 names the replacement command, #146 names the temp-file-then-rename shape, #151 supplies the four-step deploy sequence. The excluded ones state a question instead
  - seeds: `c2`, `c13`
- `s3` — `scripts/merge-gate.py (550 lines; local_outcome at 375-394, gate key reads at 246-353)`: `local_outcome` returns from inside the loop so matrix order decides the verdict, and the twelve keys the parser actually reads are enumerable — both #153 and #148 are closed, local changes to one file with tests/`test_merge_gate.py` already present
  - seeds: `c3`, `c5`
- `s4` — `examples/merge-gate/workflow.yaml:315-350 (the pinned gate matrix) and .github/workflows/adapter-codex.yml`: the matrix pins 'uv run flake8 adapters' while the CI workflow names 'adapters/codex/src adapters/codex/tests'; the generalisation to a bare directory is the whole of #152's first fix. The matrix also proves `responsible_for` IS a real key, which #148's body got wrong — the unknown keys are 'tools' and 'threshold'
  - seeds: `c4`, `c5`
- `s5` — `uv run flake8 adapters/*/src adapters/*/tests (run read-only, exit 0)`: 7 real findings today, all in adapters/human-inbox (nudge.py F401 x2 + E402 x2, `test_nudge.py` F401 x3) — not the 12 the issue counted, so the batch fixes what is measured now rather than what was measured in August
  - seeds: `c4`
- `s6` — `scripts/toolchain-baseline.sh:93-101 (capture) and :114-122 (check)`: capture() redirects measure output directly into the baseline path inside a loop that swallows per-host failure; check() already has the drift-accumulator shape capture() lacks, so the fix has a model to copy inside the same file
  - seeds: `c6`
- `s7` — `internal/api/server.go:394-465 (every registered route) against api/openapi/openapi.yaml (4794 lines)`: exactly four inbound routes are registered and unspecified, and the read half of the same lane (GET /v1alpha1/dial-in-presence, openapi.yaml:721, schemas at :4471) is already written — so #150 is transcription against an existing house style, not schema design
  - seeds: `c9`
- `s8` — `scripts/ (22 scripts) and .github/workflows/ (8 workflows)`: no lint-all.sh exists; check-zero-runtime-deps.sh and check-vendored-skill-diff.py are the precedent #123 asks to copy — a guard CI calls so local and CI cannot drift. Three workflows lint Python in two invocation styles
  - seeds: `c8`
- `s9` — `internal/notifier/ (rundetail.go, lifecycle.go) and internal/notify/payload.go`: the digest-not-name and empty-actor problems are both in the payload/rundetail pair, and the issue fixes the target rendering format explicitly; the only open sub-question is cache shape for the digest→key lookup, which the issue itself calls safe because the mapping is immutable
  - seeds: `c10` (rejected)
- `s10` — `issue #134's owner decision (2026-08-16, note-and-continue) and deploy/prod/install-secrets.sh:313`: the credential question is already adjudicated on the issue, so what remains is the mechanical residual the owner named — scrub relay inputs in the harness rather than in each agent brief
  - seeds: `c11`
- `s11` — `CLAUDE.md 'Mesh identity' + culture_nodes/cli/_commands/doctor.py:108-141, cross-checked against #131's owner comment confirming live output`: the drift is one sentence and the correction is already verified against a live 'uv run nodes doctor' run in the issue thread — the cheapest close in the set
  - seeds: `c7`
- `s12` — `issues #136 #128 #121 (operator-lane / deploy-dependent) and #116 (bug tail, 'Observed, not diagnosed')`: these are the near-misses: small-sounding, but one needs a live prod mutation on thor/orin and the other needs a diagnosis before a patch exists to apply — neither is provable by this repo's CI, which is the line the batch draws
  - seeds: `c14`, `c16`
- `s13` — `CLAUDE.md 'Conventions and workflow' (version-bump rule, byte-identity rule for shared adapter modules, nodes dogfooding reflex, issue-per-operator-hand-turn rule)`: the batch inherits four standing constraints it must not rediscover: bump the version on every PR, format shared bridge modules once and copy, prefer assigning delegable scoped tasks to codex-thor/codex-orin over doing them in-session, and open or update an issue for anything done by hand
  - seeds: `c17`
- `s14` — `challenge pass / counter-evidence lens: internal/notifier/rundetail.go, rundetail_test.go, internal/notify/payload.go`: \#66 is already implemented AND tested at HEAD — the test asserts the exact format the issue requested. The frame's c10 described a defect that no longer exists; the pass found it only by reading the code the issue named rather than trusting the issue
  - seeds: `c26`
- `s15` — `challenge pass / counter-evidence lens: deploy/prod/README.md, install-secrets.sh, issue-dialin-credential.sh`: \#151's runbook ordering and idempotent add-if-absent issuance install already exist, along with a re-issue script whose refusal names the missing bearer — the same stale-premise failure as #66, found the same way
  - seeds: `c27`, `c28`
- `s16` — `challenge pass / adjacent-systems lens: examples/merge-gate/README.md:169,191-195 (digest-pinned matrix)`: the pinned matrix is part of the published workflow's content digest by design, so a repo-only fix to #152/#148 cannot reach the gate running on thor without a republish — which the batch's no-prod-touch boundary forbids. The fix and its effect are in different places
  - seeds: `c32`
- `s17` — `challenge pass / adjacent-systems lens: internal/api/contract_test.go, openapi_json_test.go, testdata/regen-openapi-json`: openapi.yaml has a generated JSON companion under test and a contract test that walks documented paths against a live server with no exemption list — documenting four authenticated routes activates assertions, so #150 is the batch's weakest 'easy' rating
  - seeds: `c29`
- `s18` — `challenge pass / self-gating + operations lens: scripts/merge-gate.py, pyproject.toml, CHANGELOG.md, the version-check job`: the batch modifies its own gate and bumps one version file four times; both force a sequential merge order that the four-themed-PR decision (c24) did not state
  - seeds: `c30`, `c31`
- `s19` — `challenge pass / security lens: deploy/prod/install-secrets.sh:554 (relay), :21 (its own comment naming the hazard)`: the relay is still live at HEAD and the script documents it; the proposed harness scrub contains recurrence in tests without removing the mechanism, so the honest claim is containment
  - seeds: `c33`
- `s20` — `challenge pass / concurrency lens: scripts/merge-gate.py, scripts/toolchain-baseline.sh`: CLEAN — both are single-process sequential programs; `local_outcome`'s order-dependence (#153) is a determinism bug, not a race, and capture()'s loop has no parallelism. Residual risk only if a gate ever runs two matrices against one repo concurrently, which nothing today does
- `s21` — `challenge pass / migration and data-loss lens: migrations/, docs/baselines/toolchains/`: CLEAN of migrations — the batch applies none (#121's 0036 is excluded by c16). The single data-loss surface is #146, whose own fix (temp-file-then-rename) is what removes it; the destructive behaviour is therefore in scope and being deleted, not inherited
- `s22` — `challenge pass / reversibility and rollback lens: the ten issues, git history, workflow publication`: every change is an ordinary revertable commit and every closure reopens, so rollback is cheap. The one hard-to-reverse act in the neighbourhood is publishing a new workflow version — which the batch deliberately does not do (c32), so it inherits no irreversible step
- `s23` — `challenge pass / provenance-decay lens: issue line citations vs HEAD`: issue #134 cites install-secrets.sh:313; the relay now lives at :554. Line numbers in issue bodies decay silently, which is a second, milder form of the same stale-premise failure c28 addresses — cite symbols and behaviour, verify line numbers at use
- `s24` — `challenge pass / reproduction-depth lens: #123 #131 #146 #148 #152 #153`: PARTIALLY EXAMINED — #152 was reproduced by running flake8; the other five were verified by READING the named code (`local_outcome`'s early returns, the 12-key parse set, capture()'s redirect, the absent lint-all.sh, CLAUDE.md's three-check sentence) and not by executing the failure. c28's reproduce-at-HEAD requirement is therefore not yet satisfied for five of the eight remaining issues
- `s25` — `challenge pass / overlooked-actors lens: docs/triage/open-issues.md consumers, docs/deliveries/`: the batch's readers include whoever regenerates the triage table next and whoever reads the delivery record; both are addressed by c21/c32/c33's honesty requirements. Not examined: whether any Culture mesh agent or sibling repo consumes this triage table, which would make the row-count change visible outside culture-nodes

## Decisions

- PR shape (resolved q1): four themed PRs — PR1 merge-gate correctness (#148 #152 #153), PR2 guards that do not lie (#123 #146), PR3 docs match the code (#131 #150), PR4 deploy and notifier (#134 #151 #66). Four version bumps, four CI runs, each PR reverts as a unit
- routing (resolved q2): codex-thor/codex-orin take #150 and #148 as READ-ONLY analysis; the operator applies every edit. The bridge write path stays unproven (#18), so no dispatch in this batch is allowed to be the thing that lands a patch

## Open parks

- [unknown_nonblocking] whether fixing the 7 human-inbox flake8 findings should come with a CI workflow for the three ungated adapters (colleague, human-inbox, notify), which #152 raises as a third item and which is a bigger commitment than the lint fix
- [unknown_nonblocking] whether #123's lint-all.sh should also become the merge gate's adapter-lint command, which would collapse #123 and #152 into one fix and one source of truth
- [unknown_nonblocking] whether scripts/triage-report.py should record the HEAD sha each disposition was last verified against, so the next batch cannot repeat this pass's already-fixed finding
- [unknown_nonblocking] whether #123's lint-all.sh should also cover go vet (go.yml, tests.yml:93) and web.yml's webglass job, or only the three jobs literally named 'lint' — #123 says 'every lint job CI runs' and the two readings differ
