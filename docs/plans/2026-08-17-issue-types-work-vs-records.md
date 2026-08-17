# Build Plan — issue-types-work-vs-records

slug: `issue-types-work-vs-records` · status: `exported` · from frame: `issue-types-work-vs-records`

> Every AgentCulture issue declares what it is — bug, feature, task, deviation, or record — so the backlog reports outstanding work and accumulated history as two different numbers instead of one.

## Tasks

### t1 — scripts/backfill-issue-types.py: dry-run, pre-state snapshot, resumable GraphQL writer

- instruction: Stdlib only, no third-party imports — the runtime package is zero-dependency and scripts/ follows the same discipline. Model it on scripts/triage-report.py: same `GH_ATTEMPTS`/`GH_BACKOFF_SECONDS` retry, same GitHubUnreachable-to-exit-2 shape. gh 2.45.0 cannot set a type, so shell out to 'gh api graphql' with updateIssue(input:{id, issueTypeId}); issue node ids come from repository.issues.nodes.id. Verified working under the plain 'repo' scope.
- covers: c27, h23, c7, h9
- acceptance:
  - --dry-run prints the full issue-to-type mapping and performs zero mutations, asserted by a test that fails if any write is attempted
  - The script writes a pre-state snapshot of every target issue's current type BEFORE the first mutation, to a file path it prints
  - A preflight resolves type names from organization.issueTypes and exits non-zero on an unknown name, with a test covering a bogus name
  - A run interrupted partway is resumable from the snapshot without re-writing already-correct issues, covered by a test
  - The script refuses to start if the Record type does not exist, with one clear error rather than one error per issue

### t2 — Evidence-driven type assignment for all 46 open issues -> docs/triage/issue-types.csv

- instruction: This is the judgement half and it cannot be done from titles. For each open issue decide: is the defect still live (Bug), does the thing not exist yet (Feature), is it scoped work that is neither (Task), or is it history that is already true (Record)? #66 and #151 are the worked warnings — both read as live defects and both were already shipped with tests. When the evidence does not settle it, write UNDETERMINED and say what would settle it. Do not let an actor's 'already fixed' claim stand without a file, test or commit behind it.
- covers: c24, h19, c2, h4, c3, h5, c14, h14
- acceptance:
  - Every one of the 46 open issues appears exactly once with a type and an evidence pointer explaining why that type, or appears with type=UNDETERMINED and the reason it could not be decided
  - No type is assigned from the title alone: each row cites a file, test, commit, or issue comment that establishes whether the defect is live
  - The output records both counts separately — issues typed, and issues examined but deliberately left untyped
  - At least one currently-open issue is identified by reading it as history that is already true, and is cited in the spec as the worked example
  - The open-issue count and the no:type count are re-measured at the time this file is produced and written into it as `N (query, date)`

### t3 — Owner creates the Record issue type in the agentculture org

- instruction: Supply the operator the exact command rather than a settings-UI walkthrough, so the hand-turn is citable: gh api graphql with mutation createIssueType against organization agentculture, name Record, plus a description saying complete-when-written and closed-on-read. Needs an admin:org token, which the normal session token lacks. File the culture-nodes issue recording the hand-turn either way.
- covers: c4, h6
- acceptance:
  - GraphQL organization.issueTypes returns four enabled types: Task, Bug, Feature, Record
  - The creation is performed as a citable command (createIssueType) or a settings change recorded in an issue, not an uncited click
  - A culture-nodes issue records the hand-turn per CLAUDE.md's every-operator-step rule, and names who ran it

### t4 — scripts/triage-report.py groups by type: live GraphQL read, open plus recently-closed blocks

- instruction: The type read is a second GitHub call — isolate it in one small function so a future 'gitculture issue list --json issueType' replaces it in one edit (c31). Never build a search query from a type string: search fails open on a bad name and lags writes. Keep dispositions.csv's header untouched; if a column feels necessary, that is the live-read decision being abandoned and needs a deviation record, not a quiet edit.
- covers: c6, h8, c7, h9, c8, h10, c20, h17, c26, h20, c29, h24, c25, h22
- acceptance:
  - Types are read per-issue via GraphQL issueType, never via the search type:/no:type qualifiers, and a test pins that no search query is constructed
  - Type names are resolved from organization.issueTypes at run time; an unknown name exits non-zero rather than counting zero, covered by a test
  - A failing or forbidden type read returns exit code 2 (could not measure), never 1 (finding), covered by a test that simulates the failure
  - docs/triage/dispositions.csv keeps its exact four-column header; git diff shows no change to scripts/cycle-accounting.py or tests/`test_cycle_accounting.py`
  - The rendered table carries two blocks — open issues by type, and issues closed since the previous cycle by type — and the closed block is demonstrated non-empty for at least one Record
  - Every per-type count names the date typing began, so a pre-adoption type-cut cannot be read as complete
  - The existing `GH_ATTEMPTS` retry and GitHubUnreachable path are reused for the new call rather than duplicated

### t5 — scripts/close-issue.sh gains --artifact: a third evidence shape for Records

- instruction: Pure bash, matching the existing style. Validate the artifact path with 'git ls-files --error-unmatch' so an untracked file is refused — a path that only exists on the author's disk is not evidence. Keep the three evidence shapes mutually exclusive with the same explicit refusal the script already uses for run-id plus test-path.
- covers: c12, h12, c11, h11, h3
- acceptance:
  - --artifact PATH is accepted as a third alternative alongside --run-id and --test-path/--test-command, and the three remain mutually exclusive
  - A closure naming a path that does not exist, or exists but is untracked by git, is refused with a non-zero exit, covered by tests for both cases
  - reason=completed is unchanged and a bare close is still impossible: a test asserts a closure with no evidence of any of the three shapes is refused
  - docs/triage/closing-comment-template.md documents the artifact field with the same fixed-label format as the run-id and test-path shapes
  - The closing comment for an artifact closure renders the artifact path so a reader can open the record from the closed issue

### t6 — scripts/open-issue.sh: a thin, deletable template-plus-type wrapper around agtag issue post

- instruction: Thin means thin: render {{PLACEHOLDER}} substitutions into a template, validate the type, call 'agtag issue post', then set the type on the returned issue. Do not reimplement signing or auth — agtag resolves the nick from culture.yaml already. The vendored .claude/skills/communicate/scripts/post-issue.sh is not edited under any circumstance. Write it so deleting it after agtag#19 lands is a one-file removal.
- covers: h1
- acceptance:
  - The wrapper renders a body template with {{PLACEHOLDER}} substitution and sets the issue type at creation, in one command
  - The type is validated against organization.issueTypes before posting; an unknown name fails before any issue is created, covered by a test
  - git diff shows no change to .claude/skills/communicate/scripts/post-issue.sh or anything else under .claude/skills/ — the wrapper is a first-party script beside the vendored one
  - The wrapper delegates posting, signing and auth to agtag rather than reimplementing them; a reviewer can see the delegation in under 20 lines of its body
  - At least one body template exists for the Record type and names the committed-artifact field the issue must point at

### t7 — Document the convention: types, Record semantics, and which repos adopt

- instruction: Update CLAUDE.md's Conventions section, keeping the every-operator-hand-turn rule intact and adding the type as what stops those issues reading as workload. State plainly that culture-nodes adopts and other repos are offered, so nobody reads this as an org mandate. Run markdownlint-cli2 before finishing.
- covers: c5, h7, c13, h13
- acceptance:
  - CLAUDE.md states the four types, what Record means (complete when written, closed on read via --artifact), and that scripts/open-issue.sh is how an issue is opened here
  - The docs state explicitly that culture-nodes adopts the practice and every other agentculture repo is offered the vocabulary, not enrolled by this work
  - The every-operator-hand-turn rule is restated as kept, with the type named as what stops those issues reading as outstanding workload
  - markdownlint-cli2 passes on every touched markdown file

### t8 — Run the backfill and land the typed backlog

- instruction: Dry-run first and read the output, do not skim it. This is the only irreversible step in the plan and its reversibility depends entirely on the snapshot t1 writes — confirm the snapshot exists and is non-empty before the first write. Report the UNDETERMINED count as a number in the delivery; rounding it away is the failure c24 exists to prevent.
- depends on: t1, t2, t3
- acceptance:
  - A --dry-run is executed and its output reviewed before any write, and the run is cited in the delivery
  - Every open issue that docs/triage/issue-types.csv assigns a type receives exactly that type, verified per-issue through GraphQL issueType
  - Issues marked UNDETERMINED are left untyped, and the count of them is reported rather than quietly rounded to zero
  - The pre-state snapshot is committed or archived at a path named in the delivery, not left only in terminal output

### t9 — Verify the after-state and regenerate the triage table

- instruction: Regenerate docs/triage/open-issues.md and run --check; it is stale on main right now (reports 50 open against a live 46), so expect a real diff. Measure typed coverage per-issue through GraphQL and report the search no:type figure separately, labelled as lagging. Run scripts/lint-all.sh, not the individual linters. Bump the version and add a CHANGELOG entry.
- depends on: t4, t8, t6
- covers: c1, c15, h15, c16, h16, h2
- acceptance:
  - python3 scripts/triage-report.py regenerates docs/triage/open-issues.md and --check then passes, so the committed table matches live GitHub
  - Both halves are demonstrated in the same delivery: issues typed AND the report grouped by type; a delivery claim carrying only one is rejected
  - The typed-coverage measurement is taken per-issue through GraphQL, and the search no:type count is reported separately as the lagging figure it is
  - scripts/lint-all.sh passes, including the triage step, on the final tree
  - The version is bumped and a CHANGELOG entry added, per the repo rule that every PR bumps the version

## Risks

- [unknown_nonblocking] The evidence pass is judgement work an actor can fake cheaply: asserting 'already fixed' is one sentence, proving it means reading code. Prior experience in this repo is explicit that a codex actor's 'already delivered' claim must never be taken at face value. (task t2)
- [unknown_blocking] t3 is a human gate with an external dependency (org-owner privileges this session does not hold), and t8 cannot start until it lands. Wave 2 is blocked on a person, not on compute. (task t3)
- [follow_up] Whether to also backfill the 81 closed issues. Deferring keeps a permanent before/after seam in any per-type history; doing it means 81 more judgement calls on settled work nobody will re-read.
- [unknown_nonblocking] deleteIssueType exists but its behaviour against issues already carrying the type was deliberately not probed — the probe is destructive across 96 repos. The rollback story for the org-level half is therefore documented, not demonstrated. (task t3)
- [follow_up] agtag#19 or gitculture-cli#17 could land mid-flight and make t6 redundant. That is a good outcome, not a blocker — t6 is written to be deleted — but a wave that has already fanned out will still build it. (task t6)
