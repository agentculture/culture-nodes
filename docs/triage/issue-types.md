# Issue-type assignment — provenance

Companion to `docs/triage/issue-types.csv`, which is the machine-readable input
to `scripts/backfill-issue-types.py`. This file records how those rows were
produced and what was measured when, because a type assigned without evidence is
the failure the whole exercise exists to prevent (task t2, issue #157).

## Measurements

Every count names the query that produced it and the date it ran — a bare number
is not a measurement.

| What | Value | Query | Date |
|---|---:|---|---|
| Open issues at dispatch | 46 | `gh issue list --state open --limit 100 --json number` | 2026-08-17 |
| Open issues carrying no type, at dispatch | 46 | `search/issues q='… is:open no:type'` | 2026-08-17 |
| Open issues after #161 was opened | 47 | `search/issues q='… is:open'` | 2026-08-17 |
| `no:type` after #157 and #161 were typed | **47** | `search/issues q='… is:open no:type'` | 2026-08-17 |

That last row is not an error. Both #157 and #161 carry a type — confirmed
per-issue through GraphQL — and the search index still counts them as untyped.
This is the third live reproduction today of the lag recorded as claim c20, and
it is why the triage report reads types per-issue via GraphQL and never counts
them with a search query.

For the record, #157's own body opens by stating "56 of 56 open issues have no
type assigned." Two hours later the figure was 46. The issue arguing that
undifferentiated counts mislead was itself quoting a count that had already
moved.

## How the rows were produced

The 46-issue evidence pass ran as a Culture Nodes dispatch on the `codex-thor`
actor — run `01M08G4V41YK3JR05W4JRBJD58`, `read-only` sandbox, category `audit`.
The instruction supplied each issue's number, title and a 500-character body
excerpt, and required a concrete evidence pointer per row: a file path, a test,
or a command and its result. "Reads like a bug" was explicitly rejected as a
pointer.

The dispatch returned all 46 rows. It also volunteered a fact nobody asked for:

> `gh` was installed but unavailable: its configured token was invalid and network
> access to `api.github.com` failed. Classification therefore used the supplied
> excerpts and the latest locally available `origin/main` ref.

That is the honest-instrument behaviour this plan depends on, and it is a live
operational finding about the fleet in its own right.

## Verification — what the review changed

The dispatch's output is a **proposed completion claim, not evidence** (PRD
§10.4). Every row was checked mechanically before it became this file:

- **All 46 cited file paths exist** in the tree. Verified by resolving each path
  from the row text.
- **Two cited test symbols do not exist anywhere in the repository**:
  - #61 cited `test_failed_check_run_becomes_finding` — no such test. The real
    coverage is `tests/test_pr_upkeep_sweep.py:536`, class
    `TestCheckRunWorkItems`, whose docstring names issue #61, and the test
    `test_recorded_fixture_yields_the_failed_lint_check`.
  - #104 cited `TestVersionReportsControlPlaneRevision` — no such test. The real
    coverage is `internal/api/version_test.go:33`,
    `TestVersionReportsTheRevisionTheBuildWasStampedWith`.

  In both cases the **verdict survived** — the behaviour genuinely does exist,
  and both issues are correctly typed `Record` — but the pointer was invented.
  Both rows carry the corrected citation and say so inline.

This is precisely the risk recorded as `r1` on task t2: asserting "already
fixed" costs one sentence, proving it costs a file read. Two of forty-six
citations were plausible and false. Sampling three rows by hand would have had
roughly even odds of missing both; checking every symbol mechanically found them
in seconds. Any future evidence pass should verify citations by resolution, not
by reading a sample.

## Results

| Type | Count |
|---|---:|
| Feature | 14 |
| Task | 13 |
| **Record** | **12** |
| Bug | 6 |
| UNDETERMINED | 1 |

**45 typed, 1 examined and deliberately left untyped.** The untyped one is #136:
settling it needs a live actor-registry read and a connectivity or dispatch probe
against the five spark actors, which repository contents cannot establish. That
is a respectable answer and it is recorded as one rather than guessed.

### The worked example the plan asked for

**#104** is history that is already true. It reports that the control plane has
no way to say which revision is answering, and establishes this with a probe that
returned `405`. `internal/api/version.go` now documents and implements exactly
that surface, and `internal/api/version_test.go` covers it. Nothing is
outstanding; the issue is a record of a state that has since changed. Typed
`Record`, closes on read.

### The finding that matters most

Eleven issues — #13, #54, #61, #62, #67, #71, #80, #100, #101, #102, #104 —
describe missing behaviour that is implemented and covered in `origin/main`.
Under an undifferentiated count all eleven read as outstanding work. They are
history. That is roughly a quarter of the open backlog, and it is the concrete
answer to #157's question about how much of the tracker is work versus record.

One partial contradiction is recorded rather than smoothed over: **#125** had its
original `input.instruction` failure fixed, but the changelog explicitly
preserves the multi-lane `repo` failure — so it stays a `Bug`.
