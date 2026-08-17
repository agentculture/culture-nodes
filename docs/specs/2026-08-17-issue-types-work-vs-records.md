# issue-types-work-vs-records

> Every AgentCulture issue declares what it is — bug, feature, task, deviation, or record — so the backlog reports outstanding work and accumulated history as two different numbers instead of one.
> instruction: Backfill and creation are two deliverables. Treat a backfill-only change as incomplete.

## Audience

- The operator reading the backlog to decide what to do next, the owner deciding whether the project is in trouble, and every agent that opens an issue under CLAUDE.md's every-hand-turn rule. Because types are org-level, the audience is also the other 95 agentculture repos, which inherit the vocabulary whether or not they adopt the practice.
  - instruction: State in the spec which repos adopt the practice and which are only offered the type.

## Before → After

- Before: The agentculture org defines exactly three issue types — Task, Bug, Feature — all enabled, and no open culture-nodes issue carries any of them: 46 of 46 open issues match the 'no:type' search qualifier.
  - instruction: Re-run both measurements at build time: organization.issueTypes via GraphQL, and the open/no:type counts. Record both with the date they ran.
- After: Every open culture-nodes issue carries a type, and the generated triage table reports counts per type alongside the existing per-bucket dispositions — so 'how many real bugs are open' is a query with an answer, and a record can be closed on read while staying findable and countable as a record.
  - instruction: Both halves ship in the same cycle — the backfill AND the type-grouped report. A delivery claim carrying only one is rejected.

## Why it matters

- An undifferentiated count misreads the project's state in both directions. It inflates: a large share of the 46 open issues are history that is already true. And it hides staleness: the 2026-08-17 easy-pickings batch picked ten issues whose fix was already written down and found two of them (#66, #151) already shipped, tests included — caught only by reading the code, because nothing in the tracker distinguished a live defect from a record of a state that had since changed.
  - instruction: Identify at least one currently-open issue, by reading it, that is history already true, and cite it in the spec as the worked example.

## Requirements

- The census in issue #157's body is already stale and must be re-measured, not cited: the body says 56 of 56 open issues are untyped; two hours later the live counts are 46 open and 46 'no:type'. Any adoption claim states the count it measured and when.
  - instruction: Write every count as `N (query, date)`. No bare numbers reach the spec, the issue, or the delivery summary.
  - honesty: Every count that reaches the spec, the issue, or the delivery summary names the query that produced it and the date it ran.
- Grouping the triage report by type forces a change of data source: the installed gh is 2.45.0 and its 'gh issue list --json' has no issueType field at all, so type is reachable only through GraphQL (repository.issues.nodes.issueType) or the search qualifiers 'type:' / 'no:type'.
  - instruction: Verify before building: run 'gh issue list --repo agentculture/culture-nodes --json issueType' and confirm it still errors with 'Unknown JSON field'. If gh has been upgraded, the simpler --json path is available and this requirement is moot.
  - honesty: Re-checked at build time. If gh has been upgraded since 2.45.0 and now carries issueType, the GraphQL path is unnecessary complexity and the simpler --json is used instead.
- Adding a type column to the triage data is a three-file change with a schema assertion in the way: triage-report.py rejects any header that is not exactly {issue, bucket, disposition, `evidence_pointer`} (set equality, not a superset check), cycle-accounting.py reads the same file, and tests/`test_cycle_accounting.py` writes that exact header in its fixture.
  - instruction: Do not touch dispositions.csv's header. cycle-accounting.py and tests/`test_cycle_accounting.py` stay unmodified; a diff to either means the live-read decision was abandoned.
  - honesty: dispositions.csv's four-column header is unchanged by this work. A task proposing a fifth column is a deviation from the live-read decision, not an implementation detail.
- The backfill types from evidence, not from titles. Deciding Bug versus Record for an existing issue requires knowing whether the defect still reproduces — the 2026-08-17 easy-pickings batch read ten issues that stated their own fix and found two (#66, #151) already shipped, tests included, catching it only by reading the code. A title-driven backfill would stamp already-fixed issues as Bug and give #157's exact defect the authority of a structured field.
  - instruction: An issue whose live status cannot be determined from evidence gets NO type rather than a guessed one. Untyped-because-unverified and untyped-because-unvisited must be distinguishable — record which issues were examined.
  - honesty: The number of issues typed is reported alongside the number examined-but-left-untyped. A backfill that reports 46 of 46 typed without saying how each was verified is the failure this claim names.
- The type-grouped report covers open AND recently-closed issues, not open alone. A Record is closed on write — that is its entire point — so an open-only breakdown will show approximately zero Records and display the payoff nowhere. The question #157 actually asks ('how much of what we did was work and how much was history') is answered over closed issues, not open ones.
  - instruction: Report two blocks: open issues by type (what is outstanding) and issues closed since the last cycle by type (what the period actually produced). The second block is the one that answers the issue.
  - honesty: The report is checked against a period in which at least one Record was opened and closed, so the Records column is demonstrated non-empty rather than assumed.
- The backfill is dry-runnable and undoable: it prints the issue-to-type mapping without writing under --dry-run, and it records each issue's pre-existing type before writing. Rollback is trivial only while every issue is untyped (clear to null); the moment a second pass runs over already-typed issues, an unrecorded pre-state is unrecoverable.
  - instruction: Write the pre-state snapshot to a file under docs/triage/ or the scratchpad before the first mutation. 46 GraphQL mutations have no transaction — a loop that dies at issue 23 must be resumable and reversible from that snapshot.
  - honesty: The dry-run is exercised before the real run, and the pre-state snapshot is committed or archived rather than left in a shell scrollback — a rollback that depends on a terminal buffer is not a rollback.
- The type read inherits the exit-2 discipline exactly: tests.yml's lint job runs scripts/lint-all.sh root with permissions contents:read and `GH_TOKEN`=github.token, and lint-all.sh exits 2 when a step is unrunnable — which fails the job. Adding a second GitHub call to triage-report.py therefore doubles the surface on which a 503 or a permissions change turns lint red, and every one of those must exit 2 (could not measure), never 1 (finding).
  - instruction: Reuse triage-report.py's existing `GH_ATTEMPTS`/`GH_BACKOFF_SECONDS` retry and its GitHubUnreachable path for the type read. Add a test that a failing type read returns 2 and not 1.
  - honesty: A test asserts the failing-type-read path returns 2, not 1. Without it the distinction between 'could not measure' and 'the table is wrong' survives only as a comment.

## Honesty conditions

- The type is assigned at creation, not only backfilled once. A one-time backfill of 46 issues decays the moment the next issue is opened, and nothing in this repo sets a type at creation: there are no issue templates, and the agtag wrapper agents post through has no type flag.
- The counts the report prints come from the org's real type list, not from a hand-written string: 'type:NotARealType' returns 0 rather than erroring, so an unvalidated query reports a clean backlog for a typo.
- A record closed as a record is still findable. If closing it makes it invisible, the change has traded one loss of information for another — the trail CLAUDE.md's rule exists to keep is the thing being closed.
- The count is re-measured at build time, never carried forward from this frame. It moved from 56 to 46 in the two hours between #157's body and this survey.
- The owner's type-creation happens before any backfill task starts. A backfill against a type that does not exist yet fails 46 times, not once, and each failure looks like a bug in the backfill.
- Adding a type to the org menu is not adopting the practice. The spec names which repos adopt, so the other 95 are offered a vocabulary rather than committed to a convention.
- The type names the report queries are resolved from organization.issueTypes at run time. An unknown name exits non-zero; it never counts zero.
- Every Record issue names the committed artifact it points at. A Record whose only content lives in GitHub is precisely the failure this claim exists to prevent.
- close-issue.sh still refuses a bare closure after the change. The artifact shape is a third alternative added to the evidence gate, not a hole in it.
- The spec names which repos adopt the practice, so 'the audience is the org' cannot quietly become 'someone else's problem'.
- The both-directions claim stays testable: at least one currently-open issue is identified, by reading it, as history that is already true.
- Both halves land. Issues typed with the report unchanged is not this after-state — it is a backfill that decays.
- The 'no:type reaches 0' signal is measured per-issue through GraphQL, not through the search qualifier, because search lags the write.
- The lag is re-observed rather than assumed permanent. Even if search catches up, the GraphQL read is kept for the validation reason in c7.
- Nothing in this work waits on gitculture-cli#17, and the type read/write stays isolated enough that swapping it for a gitculture verb later is a one-function change.
- The before/after seam is stated in the report itself, not discovered later: any per-type count names the date typing began, so nobody reads a type-cut of pre-adoption history as complete.
- No plan task is sized or sequenced against an upstream delivery. If a task's acceptance criteria mention agtag#19 or gitculture-cli#17 as a precondition, that is this claim being violated.

## Success signals

- Two measurements, both re-runnable: the 'no:type' open count for culture-nodes is 0 (it is 46 today), and docs/triage/open-issues.md's summary line reports a per-type breakdown instead of a single 'Open issues with dispositions: N'. A third, softer signal: the next cycle's delivery summary quotes a work-only backlog number and says what it excluded.
  - instruction: Verify with: gh api -X GET search/issues -f q='repo:agentculture/culture-nodes is:issue is:open no:type' --jq .`total_count` -> expect 0; and read the summary line of docs/triage/open-issues.md.

## Scope / boundaries

- Creating an org issue type is an org-owner action that no agent in this repo can perform: the session token carries scopes 'gist, read:org, repo, workflow' and no admin:org, so Deviation and Audit/Record must be added by the human owner in org settings before any backfill can run.
  - instruction: Ask the org owner to create the Record type in org settings before any backfill task starts. The backfill verifies against organization.issueTypes first and fails fast with one clear error if it is absent, rather than 46 per-issue errors.
- Issue types are org-level, so adding two of them lands across all 96 agentculture repositories and the 605 open issues in them — culture-nodes' 46 are 7.6% of what the change touches. The decision belongs to the org owner, not to this repo.
  - instruction: Name the adopting repos explicitly: culture-nodes adopts; every other repo is offered the vocabulary through gitculture-cli#17, not enrolled by this work.
- A type-counting script must validate type names against the org's own list: a misspelled qualifier fails open, not loud — 'type:NotARealType' returns 0 results rather than an error, so a typo would report zero bugs and read as good news.
  - instruction: Resolve a type-name-to-id map from organization.issueTypes at run time and exit non-zero on an unknown name. Never build a query from a hard-coded type string.
- A Deviation or Audit type labels a pointer, not a home: durable records already live in the tree — docs/deviations/ (2 records, written by devague deviate), docs/audits/, docs/decisions/, docs/adr/ (10 ADRs), docs/deliveries/. Adopting the type must not become an excuse to write the record only into an issue.
  - instruction: Every Record issue body names the committed artifact path it points at, and close-issue.sh --artifact validates that the path exists and is tracked by git.
- The closing contract has no shape for a record: scripts/close-issue.sh always closes reason=completed and refuses a closure that lacks a disposition, a reason, and either a Culture Nodes run id or a test path plus its command. A record that is complete when written has no run and no test, so adopting Deviation/Audit means either a fourth evidence shape (the artifact it points at) or an explicit exemption.
  - instruction: Add --artifact as a third mutually-exclusive evidence option, keeping reason=completed and the refusal of a bare close. Test that a missing or untracked path is refused.
- The report must read types through GraphQL per issue, not through the search API's type:/no:type qualifiers: search is an index and it lags. Immediately after #157 was typed Task (confirmed by GraphQL in the mutation response), 'no:type' still returned 46 — the same count as before the write. A search-backed report would have reported the backfill as not having happened.
  - instruction: Measure backfill completion per-issue through GraphQL issueType, never through the search no:type count.
- Typing covers the 46 open issues; the 81 closed ones stay untyped. Any per-type history is therefore uncomputable for everything before adoption — including a type-cut of the net-positive-every-day table #155 leans on, which reads closed issues via cycle-accounting.py's state=all query.

## Non-goals

- Types do not replace buckets. The six triage buckets — verify-then-close, operator-lane enablers, bug tail, finish work, owner decisions, large bets — are dispositions (what to do next) and stay exactly as they are; the type answers a different question (what this thing is), and the report carries both.
- The CLAUDE.md rule that every piece of operator work opens or updates an issue is kept, not weakened. It is what makes un-automated hand-turns countable; the type is what stops those counts reading as outstanding workload.

## Assumptions

- The tooling gap is filed, not solved: agentculture/gitculture-cli#17 requests read/write issue-type support (type list/get/set/clear, --json issueType, name validation against the org list, GraphQL-not-search reads). Until it ships, culture-nodes writes its own GraphQL — so nothing here may wait on that issue, and whatever this repo builds should be replaceable by the CLI verb later.
  - instruction: Write the type read/write as one small, isolated helper in triage-report.py so swapping it for a gitculture verb is a one-function change.
- The org type set is changeable by API, not only by the settings UI: the GraphQL schema exposes createIssueType, updateIssueType, deleteIssueType and updateIssueIssueType. So the owner's hand-turn can be a scripted, reviewable command rather than a click nobody can cite — but it still needs admin:org, which this session's token lacks.
- Two upstream requests now carry the general capability, with a proposed split: agtag#19 owns creating an issue well (template rendering, signing, type at creation) and gitculture-cli#17 owns lifecycle and reporting (evidence-gated closes, type-grouped counts). Neither is a dependency of this work — the local wrapper ships regardless and is written to be deleted.
  - instruction: If either upstream declines the split, the local wrapper stays; nothing here changes. Do not size any task against an upstream delivery date.

## Scope exploration

- `s1` — `agentculture org issue types (GraphQL organization.issueTypes)`: The org already defines Task/Bug/Feature (ids `IT_kwDOEI9FZ84B9t67`/68/69), all isEnabled=true. Nothing needs inventing; Deviation and Audit/Record are the only additions the issue asks for.
  - seeds: `c2`
- `s2` — `live issue census (search API: is:open -> 46, no:type -> 46; GraphQL repository.issues totalCount -> 46)`: The backlog moved by 10 between #157's body (56) and this survey (46) — recent closures. A stale count inside an issue arguing about counting is itself evidence for the issue's thesis.
  - seeds: `c3`
- `s3` — `gh auth status (token scopes)`: Scopes are gist, read:org, repo, workflow. read:org can list issue types but not create them; the create step is a human hand-turn and, per CLAUDE.md's rule, itself an issue-worthy operator step.
  - seeds: `c4`
- `s4` — `org-wide repository census (GraphQL organization.repositories, 96 repos)`: 605 open issues org-wide; largest holders reachy-mini-cli 93, embodiment 65, colleague 59, lobes-cli 45, culture 36, devague 31, guildmaster 27, steward 22. agentculture/org exists but its 6 open issues are website work, so it is not an obvious home for an org-governance decision.
  - seeds: `c5`
- `s5` — `scripts/triage-report.py (open_issue_numbers) + installed gh 2.45.0`: The script reads open issues with 'gh issue list --json number'. gh 2.45.0 rejects issueType as an unknown JSON field (available fields listed: assignees..url). Adding a type column means adding a GraphQL or search call, plus retry handling, to a script whose `GH_ATTEMPTS` retry logic exists because GitHub 503s often — which it did repeatedly during this very survey.
  - seeds: `c6`
- `s6` — `GitHub search qualifier probe (is:open 46, no:type 46, type:Bug 0, type:NotARealType 0)`: 'no:type' returning exactly the open count is the positive control proving the qualifier is real and everything is untyped. 'type:NotARealType' returning 0 rather than erroring is the trap: silent zero is indistinguishable from a clean backlog.
  - seeds: `c7`
- `s7` — `docs/triage/dispositions.csv and its two consumers`: 84 rows, header issue,bucket,disposition,`evidence_pointer`. triage-report.py:76-78 does 'set(rows\[0\]) != required' — a strict equality that refuses an added column outright. scripts/cycle-accounting.py:55 loads the same file and tests/`test_cycle_accounting.py`:20 writes the four-column header. Type could alternatively be read live from GitHub and never enter the CSV at all.
  - seeds: `c8`
- `s8` — `scripts/triage-report.py BUCKETS + docs/triage/open-issues.md`: BUCKETS is a closed set of six validated per row; the rendered table is Issue/Bucket/Disposition/Evidence pointer. Nothing in it says what an issue IS, which is exactly #157's complaint — a record and a defect can share the bucket 'verify-then-close'.
  - seeds: `c9`
- `s9` — `CLAUDE.md 'Conventions and workflow' (every hand-turn opens an issue)`: The rule exists for measurement — the prior cycle ended with fourteen of fourteen operator steps still manual, and the rule is the precondition for #118 ever closing. It is also the direct cause of records accumulating in the same list as defects, which is the defect #157 names.
  - seeds: `c10`
- `s10` — `docs/ record homes (deviations, audits, decisions, adr, deliveries)`: The tree already has a first-class place for each record kind, and the devague chain writes to them. So the tracker holds records that duplicate or point at committed artifacts — #155 §9 found six such issues in one cycle, all closable with no code change because their content was already durable elsewhere.
  - seeds: `c11`
- `s11` — `.github/ (workflows only, no ISSUE_TEMPLATE)`: There are eight workflows and no issue templates or forms, so nothing sets a type at creation time. Every new issue would be typed by hand — including the ones agents open via agtag issue post, whose wrapper (.claude/skills/communicate/scripts/post-issue.sh) has no type flag and is vendored, so it must not be edited here.
  - seeds: `c3`
- `s12` — `docs/triage/open-issues.md freshness (python3 scripts/triage-report.py --check -> exit 1)`: The committed table is stale as of this survey: it reports 50 open with dispositions against a live 46. Pre-existing, not caused by #157 — but it means the lint job's triage step is red on main right now, and any change to the report's format has to land on a regenerated table.
  - seeds: `c6`
- `s13` — `scripts/close-issue.sh + docs/triage/closing-comment-template.md`: Closure is gated on checkability: disposition, reason, and run id OR test path plus command, sent as one gh issue close --reason completed. Never a bare close. A record satisfies none of the three evidence shapes, and 'completed' is the wrong state reason for something that was complete on arrival.
  - seeds: `c12`
- `s14` — `issue #155 section 9 (why this shrinks the backlog)`: Of 15 issues one cycle opened, 6 were schedulable work; the rest were the run's own history — 4 deviation records, an answered prioritisation proposal, counted hand-turns, a deploy-ordering hazard — all closed with no code change because the content was already durable elsewhere. Its rule: a record attaches to the run, an issue is opened only for residual work that outlives it. That is the same defect #157 names, answered structurally instead of taxonomically.
  - seeds: `c11`
- `s15` — `GraphQL updateIssue write probe on issue #157 (set Task -> unset -> re-set Task)`: The write path works with the token's existing 'repo' scope and no admin:org: updateIssue(input:{id, issueTypeId}) set the type, issueTypeId:null cleared it, and a re-set restored it. gh 2.45.0 has no CLI verb, so a backfill is a raw GraphQL loop over 46 node ids. Search census immediately afterwards still read 46 untyped, exposing index lag.
  - seeds: `c20`
- `s16` — `agentculture/gitculture-cli (GitHub CLI and agent - AgentCulture manager)`: No issue-type support anywhere in the AgentCulture toolchain; the gap is org-wide, not culture-nodes-specific. Filed as gitculture-cli#17 with the three verified traps (search fails open on a bad type name, search lags writes, create-vs-assign need different scopes).
  - seeds: `c21`
- `s17` — `agentculture/gitculture-cli#17 (widened by comment 5318476413)`: The original filing asked only for type plumbing. Widened at the user's direction to ask that the practice be locked into the CLI: no bare closes, a third evidence shape for Records, a type required at creation, and type-grouped reporting. The four culture-nodes artifacts that implement this today are offered as citable references.
  - seeds: `c23`
- `s18` — `challenge pass / unstated-assumption lens: docs/specs/2026-08-17-easy-pickings-batch.md + issues #66, #151`: Typing is a judgement requiring code reading, not a mechanical sweep. The batch's own evidence is the counter-example: 2 of 10 issues that read as live defects were already shipped. Seeded the evidence-not-titles requirement.
  - seeds: `c24`
- `s19` — `challenge pass / lifecycle lens: closed-issue census (search is:closed -> 81) + scripts/cycle-accounting.py load_issues(state=all)`: 127 issues exist, 46 open and 81 closed. cycle-accounting.py already reads state=all, so it is the surface where the missing history would show up. Adoption on open issues only creates a permanent before/after seam.
  - seeds: `c25`
- `s20` — `challenge pass / data-flow lens: c15 after-state + c16 success signal versus the closed-on-read semantics of a Record`: c16's 'no:type reaches 0 on open issues' measures typing COVERAGE, not the work-versus-history split the issue is about. Records leave the open set immediately, so an open-only report is structurally blind to them. The success signal and the after-state both need the closed-issue half.
  - seeds: `c26`
- `s21` — `challenge pass / containment lens: GraphQL updateIssue write probe (no batch, no transaction) + rate_limit (graphql 5000/hr, core 5000/hr, search 30/min)`: Rate limits are not a constraint at 46 or even 127 mutations. The absent safety is transactionality: each issue is its own mutation, so a partial backfill is the normal failure mode and needs a snapshot plus resume, not a bigger budget.
  - seeds: `c27`
- `s22` — `challenge pass / reversibility lens: GraphQL Mutation schema introspection`: createIssueType / updateIssueType / deleteIssueType / updateIssueIssueType all exist, so creating Record is scriptable and the change is nominally reversible. The behaviour of a delete against issues already carrying the type was deliberately NOT probed — destructive, org-wide, and unnecessary to answer now.
  - seeds: `c28`
- `s23` — `challenge pass / operations lens: .github/workflows/tests.yml lint job + scripts/lint-all.sh exit policy`: Examined and clean today: the repo is PUBLIC, so issue listing works under the job's contents:read-only token and the triage step is genuinely running in CI rather than silently skipped. The residual exposure is that an unrunnable step exits 2 and fails the job, so a second GitHub call is a second way for lint to go red for reasons unrelated to the diff.
  - seeds: `c29`
- `s24` — `challenge pass / overlooked-actor lens: .claude/skills/communicate/scripts/post-issue.sh + agtag issue post --help + absent .github/ISSUE_TEMPLATE`: Every path that opens an issue here is typeless: agtag has post/fetch/reply and no type option, its wrapper is vendored and unmodifiable, and no issue form exists to carry a default. The type therefore decays from day one unless something types issues after creation.
  - seeds: `c3`
- `s25` — `challenge pass / adjacent-systems lens: docs/triage/after-state-queries.md + scripts/cycle-accounting.py + examples/pr-upkeep/sweep.py`: Three adjacent readers. after-state-queries.md holds canned gh issue list queries that would need type-aware siblings; cycle-accounting.py reads state=all and is the natural home for a net-of-records count; pr-upkeep/sweep.py touches GitHub only for check-runs and Sonar findings, not issues, so it is unaffected.
  - seeds: `c26`
- `s26` — `challenge pass / security lens: token scopes, mutation surface, credential handling`: Examined, clean. The work adds no credential, no new secret, and no egress: it reads and writes issue metadata with the operator's existing token. The only privilege question is the admin:org needed to CREATE a type, already captured as c4, and the backfill needs nothing beyond the repo scope the probe verified.
  - seeds: `c4`
- `s27` — `challenge pass / adjacent-systems lens: .claude/skills/communicate/scripts/templates/ (skill-new-brief.md, skill-update-brief.md)`: Templated issue bodies already exist in the ecosystem as a private convention: two {{PLACEHOLDER}} templates rendered by steward-cli's announce-skill-update and posted through agtag. So the capability is being reinvented per-caller today, which is the argument for agtag owning both templates and types rather than each repo wrapping it.
  - seeds: `c30`
- `s28` — `challenge pass / hidden-dependency lens: agentculture/agtag#19 and gitculture-cli#17`: Filed both so the capability has an owner beyond this repo, and stated the split explicitly on the agtag issue so the two do not each grow a half-implementation. Recorded as an assumption rather than a requirement because culture-nodes must not wait on either.
  - seeds: `c31`

## Decisions

- Adopt issue types now, complementary to #155 rather than superseded by it: even once records move to the run ledger, issues still need bug/feature/task separation, so the typing work survives the structural fix.
  - instruction: State the relationship explicitly in the issue and the spec: types answer 'what is this issue', #155 answers 'should this have been an issue at all'.
- One new type, not two: 'Record'. Deviation and Audit/Record behave identically — complete when written, closed on read — and origin (something we did versus something we found) is already carried by the title and body. One type is a smaller commitment across the 95 other repos that inherit it.
  - instruction: The org type set becomes Task, Bug, Feature, Record. Do not add a separate Deviation type.
- scripts/triage-report.py groups by type, reading the type live from GitHub — never from a column in dispositions.csv. The type names it queries are validated against organization.issueTypes so an unknown name errors instead of silently returning zero, and an unreadable GitHub still exits 2 (could not measure), never 1 (finding).
  - instruction: Keep dispositions.csv's four-column header untouched, so cycle-accounting.py and tests/`test_cycle_accounting.py` are unaffected. Add the type read as a second GitHub call under the same `GH_ATTEMPTS` retry policy.
- A Record closes through a fourth evidence shape: scripts/close-issue.sh gains an --artifact PATH option accepting the committed record the issue points at (docs/deviations/\*.md, docs/audits/\*.md, docs/decisions/\*.md, docs/adr/\*.md, docs/deliveries/\*.md). The checkability contract is preserved — every closure still names something a reader can open — and the pointer-is-not-a-home rule becomes enforceable by the script instead of by discipline.
  - instruction: Keep reason=completed and keep the existing run-id and test-path shapes. Add --artifact as a third mutually-exclusive alternative, validate that the path exists and is tracked by git, and refuse a closure that names an untracked or missing file.
- The issue discipline itself belongs in gitculture-cli, not only in this repo: the evidence-gated close (run id / test path / committed artifact), the pointer-is-not-a-home rule for Records, typing at creation, and grouping the backlog report by type are org-wide conventions that currently exist as one shell script, a CLAUDE.md paragraph, a closing template and a triage generator inside culture-nodes. Locked into the org CLI they become verbs every repo already has; copied, they are four artifacts nobody else inherits.
  - instruction: Offer close-issue.sh, docs/triage/closing-comment-template.md and scripts/triage-report.py to gitculture-cli as reference implementations to cite from. Do not block culture-nodes' own work on that adoption (c21).
- culture-nodes adds a thin repo-local wrapper — around agtag today, gitculture later — that opens an issue from a body template AND sets its type at creation, so the type stops decaying from day one. The same capability is filed upstream in agtag so the wrapper is a stopgap with a deletion path, not a permanent fork of issue creation.
  - instruction: Keep the wrapper thin enough to delete: it renders a template and adds one --type argument on top of agtag issue post. Do not reimplement agtag's posting, signing, or auth. The vendored .claude/skills/communicate/scripts/post-issue.sh is NOT edited — the wrapper is a first-party script beside it.

## Hard questions

- Does typing 46 issues change any decision anyone would actually make, or does it change how the backlog reads? If the honest answer is the second, the value is real but smaller than the org-wide blast radius suggests — and #155's structural fix is the one that changes decisions.
- risk: Adopting two new org types commits 95 other repos to a vocabulary none of them asked for, and the practice will not propagate — 605 open issues org-wide, and nothing broadcasts a convention except the steward/guildmaster skill lane. The likely outcome is culture-nodes typed and everything else untyped, which is fine unless the types were justified by org-wide value.

## Open parks

- [unknown_nonblocking] Whether to backfill the 81 closed issues as well. It is the only way per-type history becomes computable, and it is 81 more judgement calls on issues nobody will re-read — the F1 evidence bar applied to settled history.
- [unknown_nonblocking] What happens to issues carrying a type when deleteIssueType removes it — silently untyped, or refused while in use. Not probed: the probe is destructive and would land across 96 repos. This is the reversibility question for a decision whose blast radius is org-wide.

## Resolved vagueness

- [unknown_blocking] Assigning a type to an existing issue was not verified: gh 2.45.0 has no CLI verb for it, so it needs a GraphQL mutation, and scope exploration is read-only — no mutation was attempted. Whether the current 'repo' scope suffices to set issueType on 46 issues is untested. — resolved: Verified by write probe on #157 (user-authorised, 2026-08-17): updateIssue with issueTypeId works under the existing 'repo' scope; null clears it. An agent CAN backfill types. Only CREATING a new type still needs the org owner (c4).
- [unknown_nonblocking] Whether types are the right answer at all, or a stopgap: issue #155 §9 proposes the structural fix — a record attaches to the run's ledger and an issue is opened only for residual work that outlives the run — and shows 15 cycle issues collapsing to 8 under that rule. Types make records countable in GitHub; #155 makes most of them stop being issues. Complementary or superseding is undecided. — resolved: Decided complementary, not superseding: types answer 'what is this issue', #155 answers 'should this have been an issue at all'. The bug/feature/task separation survives the structural fix (user decision, 2026-08-17).
