# Close-the-backlog bootstrap honesty

Status: proposed delivery record. This document does not confirm its own claims.

## Cycle accounting

The cycle begins at `2026-08-15T17:29:21+03:00`, the committer timestamp of
the cycle's first commit, `1e6a532`. The offline issue snapshot supplied at
cycle close produces these four numbers; each command prints only the number
shown, so the announcement is a rerunnable query rather than copied arithmetic.

| Measure | Number | Exact query |
|---|---:|---|
| Issues opened during the cycle | 10 | `python3 scripts/cycle-accounting.py --issues-json .issues-snapshot.json --metric opened` |
| Issues closed during the cycle | 14 | `python3 scripts/cycle-accounting.py --issues-json .issues-snapshot.json --metric closed` |
| Closed minus opened (delta) | 4 | `python3 scripts/cycle-accounting.py --issues-json .issues-snapshot.json --metric delta` |
| Opened-by-cycle issues undispositioned at cycle close | 0 | `python3 scripts/cycle-accounting.py --issues-json .issues-snapshot.json --metric undispositioned` |

`scripts/cycle-accounting.py` derives the boundary from Git on every run. For
the last row it selects issues opened after that boundary which remain open,
then checks their numbers against `docs/triage/dispositions.csv`. Closed issues
carry their disposition in their GitHub closure; open issues must have a row in
the disposition authority.

Omit `--issues-json .issues-snapshot.json` to query all issue states from
GitHub with `gh`. That live path could not run in the network-denied dispatch
that wrote this (issue #119); it was exercised in the operator lane at merge
and works, returning **11 opened, 14 closed, delta 3, undispositioned 0**.

The live numbers differ from the snapshot numbers above by one, and that
difference is the point rather than a discrepancy: #120 was filed between the
snapshot being taken and the merge. A script that returned the same numbers
either way would not be querying anything.

## The fourteen old-way operator steps

The inventory below preserves all fourteen rows from section 11 of
`2026-08-15-own-the-work-end-to-end-STATE.md`. “Automated” is deliberately
narrow: a helpful command or UI is not an automated node.

At merge the count became **fourteen of fourteen still-manual**. The single
row this document originally marked automated — collecting an agent's changes
— was corrected against evidence gathered the same day; see row 6. That is the
honest number, and it is the number #118 exists to change.

| # | Operator step | Cycle-close status | Repository evidence or remaining gap |
|---:|---|---|---|
| 1 | Refresh the agent checkout before dispatch | **still-manual** | Merge `b96b129` generates a brief, but its own commit record says the sandbox cannot pull and the operator still seeds the checkout over SSH. |
| 2 | Compose the brief by hand in a heredoc | **still-manual** | `b96b129` adds deterministic brief rendering, but no merged node invokes it as part of dispatch. |
| 3 | Choose actor and sandbox vocabulary, then background blocking `assign` | **still-manual** | No merged node owns routing and dispatch; `nodes-op assign` remains an operator command. |
| 4 | Poll the run to terminal | **still-manual** | `9f7f57c` adds `nodes-op running`, which improves the read surface but does not make a node wait and route on terminal state. |
| 5 | Read the full claim | **still-manual** | `9f7f57c` removes truncation, but a human must still read and judge the claim. |
| 6 | Collect changes with `ssh … git diff HEAD --binary` | **still-manual** | Corrected at merge from the "automated" this row was first given. `d5e4b35` has the control plane fetch and measure a handed-over ref — but that is the *measurement* path, not the *collection* path: the operator still turns a run into a mergeable diff by hand, and did so for this very package. Two further facts landed after the row was written. The deployed bridges predated the ref-minting code, so three dispatches reported `handover=true` and created nothing (#120), and `internal/handover`'s correct "no fetchable ref, no record" rule made that indistinguishable from an honest refusal. And the collector the row would need is task t11, dispatched but not merged at the time of writing. |
| 7 | Stage changes with `git worktree add` and `git apply --3way` | **still-manual** | `d5e4b35` fetches and measures a ref, but no merged merge node stages that ref into an integration worktree. |
| 8 | Run every suite before and after merge | **still-manual** | The log records operator-run gates; planned gate-node task t11 is not merged. |
| 9 | Repair what the gate found in the operator's window | **still-manual** | Issue #102 remains open and no repair-loop node is merged. |
| 10 | Write the commit message and run `git merge --no-ff` | **still-manual** | Commit `e22ae43` records fifteen hand merges and files #118; an issue proposing a combining node is not automation. |
| 11 | Decide the agent's proposed claim | **still-manual** | `fa30bec` supplies the decision surface and enforces a human decider, but ledger promotion intentionally remains a human action. |
| 12 | Run `git worktree remove` | **still-manual** | The repository contains lifecycle design, but no cycle commit merges a cleanup node into this bootstrap path. |
| 13 | Push the integration branch with the credential dance | **still-manual** | Handover refs use fleet SSH transport, but integration-branch publication is still performed outside a node. |
| 14 | Deploy the reviewed revision | **still-manual** | Issue #104 remains open and the planned gated deploy node is not merged. |

### Stage-2 operator shell transcript

This checkout does not contain one canonical transcript file. The cycle's
commits and `.devague/deliveries/close-the-backlog.json` do record the following
operator shell commands, so they are reported rather than omitted:

```text
ssh <agent-host> 'cd <checkout> && git add -N . >/dev/null 2>&1; git diff HEAD --binary'
git worktree add <integration-worktree> <base>
git apply --3way <harvested-patch>
go build ./...
go vet ./...
go test ./...
python3 -m pytest tests/ -q
git merge --no-ff <package-ref>
git worktree remove <integration-worktree>
git push ssh://thor/home/thor/git/culture-nodes-agent HEAD:refs/heads/ctb-base
git ls-remote ssh://orin/<checkout>
nodes-op assign --handover
```

These are historical commands reported from repository evidence, not commands
run while writing this document. Their presence means the stage-2 exit gate —
dispatch to merge with no operator shell command — was not met.

## Last cycle's three actionable NOT MET signals

The preceding delivery summary has four NOT MET headings, while its opening
tally and the close-the-backlog spec name three actionable failures. Signal 6
explains signal 5's missing record rather than adding a fourth mapping. This
table follows the spec's named trio and does not hide that distinction.

| Last-cycle failure | Issue that flips it | Why closure flips the signal |
|---|---|---|
| The two-carrier fix-to-review handoff did not complete across two machines without a path or operator copy. | #90 | #90 may close only after the git-ref transport is exercised in the same cross-machine graph as the artifact carrier, so that closure supplies exactly the live observation the signal requires. |
| Cancelling a run was not shown to end the actor session. | #87 | #87 is the still-open umbrella whose acceptance explicitly requires observing the actor session end; closing it on that evidence, rather than run status, directly changes this verdict. |
| Nine of nine proposed claims had no recorded decision. | #99 | #99's merged decision surface plus a closure check that the cycle's pending-decision query is empty changes the measured count from undecided claims to recorded human verdicts. |

Issue #21 is closed and concerns bridge concurrency latency, not the missing
live cancellation observation, so it is not forced into the second row merely
because the close-the-backlog spec called that failure “#21/#9-adjacent.”

## The next-cycle companion-document fork

This close-the-backlog cycle closes with this delivery document and without a
new `-STATE.md` companion. The next cycle therefore starts without writing a
`-STATE.md`. If a state companion is nevertheless needed later, its delivery
summary must mark the companion-document signal NOT MET again; treating the
file as neutral scaffolding is not an allowed third outcome.
