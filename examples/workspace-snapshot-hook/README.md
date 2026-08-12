# workspace-snapshot-hook

The standard `post_run` workspace-snapshot pattern (task t12, spec claim c15,
honesty condition h10), as a stand-alone, runnable example rather than a unit
test fixture.

## What this shows

`workflow.yaml` declares one agent node, `review`, with one ordinary
`post_run` hook — a policy check refusing a workspace that still contains a
`TODO` marker. Authoring it is exactly the shape every other hook uses
(`schemas/workflow/workflow.schema.json`'s `#/$defs/codeOperation`, pinned to
a digest, dispatched through the runner boundary); there is nothing
snapshot-specific in the YAML at all.

What makes the *pattern* a workspace-snapshot hook lives entirely on the
worker side, and applies to every hook a workflow declares — not just this
one: `internal/worker/hooks.go`'s `buildHookOperation` asks the runner for a
workspace comparison on every hook dispatch
(`evidence.snapshot_before`/`snapshot_after`, `pre_run` and `post_run`
alike). A runner that can honour that request reports the changed files, a
diff digest, and artifact refs on its `Result` with
`observations.changed_paths` measured; `internal/runners/dispatch.go`'s
`buildEvidence` then surfaces those straight into the same observed evidence
record every other runner-measured fact lands in — **never** through this
node's own `ledger.propose` delta, because an agent node can never declare
`observe` at all (`internal/compiler/ledger.go`'s `checkLedgerDelta`: only a
code node's own dispatch earns that authority).

## Why the evidence is empty today, and how it stops being empty

Neither runner this build ships can honour the workspace-comparison request
yet, and both say so honestly rather than fabricating an answer:

- `internal/runners/headspace`: headspace-cli 0.11.0 has no snapshot/diff
  verb (see that package's own doc comment, "changes.* ... NOT measured").
- `internal/runners/lambda`: a managed function has no workspace to compare
  at all (`internal/runners/lambda/doc.go`, `internal/runners/lambda/
  evidence.go`'s `workspaceSnapshotObservation`).

Run this workflow against either runner today and `review`'s post_run
evidence record carries no `changed_paths`, `snapshot_digest`, or
`artifact_refs` field — `observations.changed_paths` stays
`{measured: false, complete: false}`, and `covered_scope` names it as
unmeasured rather than leaving the absence to be inferred. That is the
correct, honest answer for the runners that exist today.

The point of shipping the request now rather than waiting for a capable
runner: once ANY runner (headspace's own future release, or a new adapter)
learns to compare a dispatched workspace before and after a hook runs, this
exact workflow — unmodified — starts producing that evidence. Nothing about
the workflow, the hook's authoring, or the worker's dispatch changes; only
the runner's own honesty declaration does.

## Proof at the worker/ledger layer

`internal/worker/hooks_test.go`'s
`TestPostRunWorkspaceSnapshotEvidenceIsAppendedNotAgentDelta` proves the half
of this pattern that does not depend on a real runner existing yet: against
an in-process fake runner shaped the way a snapshot-capable runner would
answer, it asserts that the changed files, diff digest, and artifact refs
land in one observed, runner-origin ledger record — and that the agent's own
proposed `claim` record, appended through the ordinary §13.2 path in the
same run, carries none of those fields. `internal/runners/dispatch_test.go`'s
`TestBuildCompletionSurfacesWorkspaceSnapshotEvidenceWhenMeasured` and
`TestDiffArtifactRefStaysOutWhenTheWorkspaceComparisonIsUnmeasured` prove the
narrower evidence-building seam both of those tests, and every runner
adapter, share.

## Offline validation

```bash
go run ./cmd/nodes validate examples/workspace-snapshot-hook/workflow.yaml
```

compiles clean with zero errors and zero warnings — no network, no runner
required.
