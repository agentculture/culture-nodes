# Stage-1 verification lanes

Status: **proposed, pre-dispatch**. This file records the lane decision before
any stage-1 verification dispatch. It does not claim that a verification ran.

## Source and reconciliation

`python3 -c 'import csv; print([r["issue"] for r in
csv.DictReader(open("docs/triage/dispositions.csv")) if
r["bucket"] == "verify-then-close"])'` returns the authoritative bucket-A
membership: `['8', '9', '10', '13', '28', '48', '54', '61', '62', '98']`.
The same command is the completeness boundary for this split.

The plan text also names #17 in its Go/shell list, but
`rg -n '^17,' docs/triage/dispositions.csv` returns #17 as `bug tail`. It is
therefore not a stage-1 verification and is not silently inserted into bucket
A. This is a plan/table discrepancy for the operator to decide.

**Operator decision: the table is right and t5's instruction text was wrong.**
The same plan assigns #17 to task t12, whose instruction reads "#17 needs
reproduction against current HEAD before it is fixed or closed" — a
reproduction is bug-tail work, not a verify-then-close verification. #17 stays
in the bug tail and out of every stage-1 lane. Recorded here rather than
corrected in place because a lane the split got wrong is a miss worth keeping
visible, per this task's own third criterion.

## Exclusive lanes

| Lane | Issues | Evidence need | Execution location |
|---|---|---|---|
| Spark Go/shell | #8, #9, #13, #28, #48 | Go tests, shell checks, or executable repository checks | spark |
| Spark Claude Python | #10, #62, #98 | Python-side tests or executable local inspection | a Claude bridge on spark |
| Spark network/API | #54, #61 | GitHub/API state in addition to local Python behavior | spark, using a network-capable verification posture |

The network/API lane moves #54 and #61 out of the Python lane. The command
`rg -n '#54 looks satisfied|#61 by the live control plane'
docs/specs/2026-08-15-close-the-backlog.md` identifies their external merge
observation and live CI-check-run claims. The command
`UV_CACHE_DIR=/tmp/culture-nodes-uv-cache uv run nodes doctor` failed while
resolving `files.pythonhosted.org` with `Temporary failure in name
resolution`; this cycle therefore cannot treat a codex agent host as a source
of evidence requiring GitHub, PyPI, or another network API.

## Dispatch and evidence rules

- Every executable-evidence artifact must name the exact command and state
  that it ran on spark. A test result from an agent host is not stage-1
  evidence.
- Every verification uses `scripts/render-verification-brief.py`; bespoke
  briefs are a gate failure.
- Every verification dispatch is read-only. Writable-roots widening,
  `workspace-write`, or a handover ref is a gate failure.
- If the assigned lane cannot produce the required evidence, record a lane
  miss using the exact result `cannot verify here` plus the reason. Do not
  silently re-route the issue; a later dispatch requires an explicit amended
  lane record.

`python3 scripts/check-stage1-lanes.py` checks that the three issue lists are
disjoint and equal to the current `verify-then-close` rows in
`docs/triage/dispositions.csv`.
