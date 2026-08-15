# Package P10 design: workspace provisioning and handover validation

## t16 — workspace ownership decision

Decision: workspace-holding bridges mint worktrees locally beneath an
operator-configured scoped root. The engine supplies a stable writer name and
repository identity, not a worktree path.

Engine-side path handing was rejected because it recreates issue #74's
path-portability defect: a path selected on the control-plane host need not
exist on the actor host. The bridge is the component that can resolve its
local checkout and atomically ask that checkout's Git metadata to create a
worktree.

The permitted layout follows `CLAUDE.md`:
`../.worktrees.culture-nodes/<writer-name>/`. Provisioning fails closed when
the target already exists or Git already knows it as a worktree; it never
adopts a pre-existing directory. A scoped-prefix allowlist authorizes minted
children without enumerating them. Exact repository entries remain supported
for compatibility.

Containment is checked independently of allowlist membership. A worktree
target must not be inside any other writer-accessible exact root. Thus the
current `culture-nodes/.claude/worktrees/web-ux-quick-wins` layout is invalid:
the allowlisted `culture-nodes` root contains it. Sibling worktree roots avoid
that reachability relation.

## t18 — deterministic handover validator (analysis only)

### Contract and changed-file basis

The validator is a deterministic node kind, not an agent prompt. Its input
pins: the base and candidate Git object IDs, the changed-file list computed as
`git diff --name-only <base>..<candidate>`, the applicable gate matrix, every
threshold, tool image/digest and tool version. These values are part of the
published workflow version, so a threshold cannot be selected after seeing
the result. The node rejects an input whose supplied changed-file list differs
from its own Git calculation.

The output contains one result per declared gate:

```json
{
  "gate": "coverage",
  "status": "passed|failed|not_applicable",
  "value": 81.4,
  "unit": "percent",
  "threshold": {"minimum": 80},
  "changed_files_considered": ["culture_nodes/cli/__init__.py"],
  "instrument": "coverage.py",
  "instrument_version": "...",
  "reason": null
}
```

`value` is numeric for every applicable gate. A not-applicable result has
`value: null`, names the uncovered files/tree and gives a machine-readable
reason; it is never converted to a passing result. The aggregate also reports
`applicable_gate_count`, `passed_gate_count`, `failed_gate_count`, and
`not_applicable_gate_count`, preventing an empty scan from looking green.

### Measurements

- Tests: select the repository-declared suite(s) for every changed tree and
  run them against the candidate. Report at least `tests_executed` and
  `tests_failed` (the gate's primary number is failures, threshold zero), plus
  exit code and suite identities. A tree with no declared suite is
  `not_applicable/no_test_instrument`, not zero failures. Test selection is by
  changed tree; it does not claim that every test maps to one source file.
- Coverage: consume a coverage report produced by the selected suite, verify
  that every changed source file expected to be covered appears in that
  report, then compute covered/coverable lines for changed lines and report
  the percentage. Today this is applicable only to changed files under
  `culture_nodes`; the configured coverage run and report do not reach Go,
  adapters, or web. Those trees are `not_applicable/instrument_not_reaching_tree`
  until issue #88 supplies and baselines instruments for them.
- Cognitive complexity: read per-file/per-function numeric measures from the
  Sonar analysis pinned to the candidate, filter to the changed files, and
  report the maximum (and retain sum/count as supporting values). The node
  verifies every applicable changed file has a measure. Today
  `sonar.sources=culture_nodes`, so `internal`, adapters, and web are
  `not_applicable/instrument_not_reaching_tree`. Sonar's server-side
  changed-file scoping is not reused as the validator's authority; the node
  applies its own pinned changed-file set to returned measures. If no Sonar
  analysis exists for the candidate, that is
  `not_applicable/instrument_unavailable`, never complexity zero.
- File length: read each changed source file at the candidate object and count
  physical lines exactly as `tests/lint/filelength_test.go` does, comments and
  a final unterminated line included. Report the maximum line count, with the
  current hard maximum of 1000 and per-file values. This instrument reaches
  all source extensions in that repo-wide test. A docs/config-only change is
  `not_applicable/no_source_files`, not a pass. The 300-line target may be
  reported as advisory metadata but is not a failing threshold.

The present applicability matrix therefore makes a change in `internal/` or
`adapters/` explicitly **tests-only plus file-length**: coverage and cognitive
complexity remain not-applicable until #88 widens and baselines them. That
label is emitted in the result; the aggregate must not say that all four gates
passed. A workflow may allow this declared transitional profile, but cannot
reinterpret the two missing measurements as green.

### Domain routing and bounded continuation

The validator has domain outcomes such as `gates_passed`,
`changes_required`, and `measurement_incomplete`. A numeric threshold miss is
`changes_required`; missing expected coverage is `measurement_incomplete`.
Both are ordinary routable outcomes, not failed attempts or engine failures.
Tool crashes and malformed pinned inputs may still be technical failures,
because no trustworthy domain measurement was produced.

The workflow author routes `changes_required` to the writer's continuation
and declares `continue.while`, `bounds.maxContinuations`,
`bounds.maxWallClock`, `bounds.maxSessions`, and `onExhausted`. The condition
reads validator output (for example `failed_gate_count > 0`), not an agent's
opinion. This matches `Node.DecideContinuation`: a true CEL condition
continues, while any spent bound produces the declared `onExhausted` domain
outcome. The compiler must also require an edge for that outcome, as pinned by
`TestExhaustedOutcomeMustBeRouted`. A test with an intentionally unreachable
coverage threshold must reach `onExhausted`, proving the loop terminates.

### Ledger authority

For each gate the deterministic validator appends an immutable ledger record
with `authority=derived`, validator origin, workflow/candidate digests,
instrument identity/version, changed-file set, numeric result or explicit
not-applicable reason, and the threshold declaration it evaluated. The
aggregate outcome is another derived computation over those records. No
agent-authored `proposed` completion claim is promoted or used as the gate
result. This follows PRD section 10.4: deterministic validators derive;
agents only propose; a human remains the authority that confirms or rejects a
completion claim and decides merge.
