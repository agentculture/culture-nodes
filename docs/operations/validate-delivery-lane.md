# The validate-delivery lane: evidence before anyone is asked for Done

`/validate-delivery` is the devague **execution-to-evidence** leg. It runs
the confirmed plan's behavioral tests agent-side after a wave merges and files
what it found — evidence for what passed *and* what failed, behavioral deltas
for what the run added, amended or removed — as record-only entries in the
plan's delivery ledger. The skill itself is vendored verbatim at
[`.claude/skills/validate-delivery/SKILL.md`](../../.claude/skills/validate-delivery/SKILL.md)
(origin `agentculture/devague`, see [`docs/skill-sources.md`](../skill-sources.md));
this page is the operator half for **this** repo: when the leg runs here,
what it files, and the one rule it exists to enforce.

The repo's behavioral-test convention — a dedicated `tests/behavioral/`
folder, `//go:build behavioral` for Go, the `behavioral` pytest marker for
Python — is declared in [`tests/behavioral/README.md`](../../tests/behavioral/README.md).

## The rule this lane enforces

**A merge never asks a human node for Done before evidence is filed.**

This is the login-from-anywhere cycle's decision c32: *Done on merge is a
human actor node — a merge raises a human decision after live validation, and
a ticket moving to Done is a possible result of that node, never an automatic
transition from the merge fact.* Read together with the vendored skill's hard
rules, the order of operations is fixed:

```text
assign-to-workforce merges a wave
    │
    ▼
validate-delivery  ── run tests/behavioral agent-side
    │                 ── devague evidence   (pass AND fail, one per obligation checked)
    │                 ── devague delta      (added / amended / removed, with provenance)
    ▼
summarize-delivery ── devague summary reads the ledger into Delivery Claims
    │
    ▼
human node "Ticket done?" ── the person decides with the evidence in front of them
    │
    ▼  (done outcome)
Jira transition → Done
```

The `pr.merged` fact freezes the ticket and raises the human task; it plans
no transition. The person at that node is being asked to *read filed
evidence*, not to take an agent's word that the work is finished — an agent
saying "done" is a completion claim, not verified evidence (PRD §10.4). If
the ledger holds no evidence for the claim the merge is supposed to have
delivered, the honest answer at that node is *not yet*, and the lane's job is
to make sure that situation is visible rather than possible to miss.

## When the leg runs

Three moments, in order:

1. **Before the first wave: file the obligations.** Every confirmed
   requirement claim on the frame becomes a `devague oblige <cN>` record
   whose `--behavior` is the claim's honesty-condition text (the frame's own
   statement of what would make the claim true), and every acceptance
   criterion of every confirmed task becomes a
   `devague plan oblige <tN> --criterion <N>` record. Obligations are filed
   once per cycle; a later text change on the claim or criterion shows as a
   drift marker in `--list`, which is the cue to re-read the obligation, not
   to silently keep it.
2. **After each wave merges, before `/summarize-delivery`.** Run the
   behavioral tests relevant to the obligations that wave touched and file
   evidence for each outcome. A partial or failed fan-out is still a valid
   input — validate whatever merged and report the rest as *not yet
   checkable*. There is no completion precondition.
3. **Before the delivery summary and before the "Ticket done?" node.**
   `devague summary` reads the filed evidence and deltas into the Delivery
   Claims table; the summary and the human node both consume that record.
   The success signals (`c29` in this cycle) must each have at least one
   evidence record behind them before the summary is written — that is
   claim c31's honesty condition h13, verbatim.

The leg is **not** a fourth standing human gate. The three gates (spec,
implementation split plan, final PR) are unchanged; this lane produces the
record those gates and the human node read.

## How evidence is filed

The agent runs the tests; the CLI only records. `devague` never executes a
test itself (devague issue #20).

```bash
# 1. Run the behavioral tests agent-side, read-only against the tree
go test -tags behavioral ./tests/behavioral/...
uv run pytest -m behavioral tests/behavioral

# 2. File one evidence record per obligation checked — failing ones included
devague evidence --obligation o3 \
  --test tests/behavioral/access_jwt_test.go::TestWrongAudienceRefuses \
  --behavior "a token with the wrong aud is refused with reason bad_audience" \
  --contract "<the claim or criterion text, snapshotted at filing>" \
  --type automated --strength execution \
  --basis "ran at HEAD in this worktree; go test output attached to the PR" \
  --outcome pass \
  --run-commit "$(git rev-parse HEAD)" \
  --run-timestamp "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --origin llm
```

Field by field:

| Flag | What it carries |
|---|---|
| `--obligation oN` | the obligation this evidence satisfies (from `devague oblige --list` or `devague plan oblige --list`) |
| `--test <ref>` | the test that asserted the behavior — a Go test name or a pytest node id, under `tests/behavioral/` |
| `--behavior` | what the named test *actually* asserts, quoted — not a paraphrase of the claim |
| `--contract` | a snapshot of the claim / criterion text at filing time, so later edits cannot rewrite what was promised |
| `--type` | `automated`, `integration`, `manual` or `observation` |
| `--strength` | one rung of the ladder below |
| `--basis` | why that strength was earned, verbatim |
| `--outcome` | `pass` or `fail`, recorded verbatim — a failure is filed exactly like a pass |
| `--run-commit` / `--run-timestamp` | required at `execution` strength and above: the SHA and time the test ran against |

`--origin llm` lands the record `proposed`; only the human's
`devague evidence --confirm eN` (or `--reject`) moves it. An agent's own
filing never self-confirms — the same anti-fabrication contract as
`deviate` and `lapse`. A `user`-origin filing auto-approves.

**Unmet is unmet.** An obligation with no behavioral test yet is reported as
*unchecked*, and a failing test is filed with `--outcome fail`. Neither is
folded into a passing tally, softened in wording, or left out of the report.
`/summarize-delivery` then shows that claim as unverified or failing, never
rounded up.

## The strength ladder

Strength is the confidence vocabulary `devague summary` uses in the Delivery
Claims table. Each rung says what the evidence *proves*, and `--basis` says
why the rung was earned:

| Rung | What it establishes | Typical basis |
|---|---|---|
| `coverage` | a test exists that names this behavior | the test is present in the tree and cites the obligation |
| `fidelity` | the test asserts the promised behavior at the promised seam, not a proxy for it | the assertion reads the same seam the obligation names |
| `execution` | the test ran and produced this outcome against a named commit | `--run-commit` + `--run-timestamp` from an actual run |
| `sensitivity` | the test is known to fail when the behavior is absent | a mutation or negative case was observed to flip the outcome |

A run reference is mandatory at `execution` and `sensitivity`; the CLI
refuses those rungs without one. Claim the lowest rung the basis supports —
a test that exists but was not run in this worktree is `coverage` or
`fidelity`, not `execution`. Any approved lapse on a claim caps its confidence
in the summary the same way it always has.

## How deltas are filed

When merged behavior diverges from the plan — a behavior was added, an
existing one amended, or a promised one removed — file a delta with
provenance both ways: back to the claim, approved deviation or prior delta it
traces to, and forward to the evidence that backs it.

```bash
devague delta --kind amended \
  --behavior "empty description renders as an empty line, not the literal 'null'" \
  --caused-by c14 --evidence e7 --origin llm
```

- `--caused-by` is repeatable and required; refs are validated by shape
  (claim ids against the live frame, deviation ids against approved
  deviations, delta ids against existing deltas).
- `--evidence` is repeatable and optional at filing — evidence often lands
  later; add it when it does.
- Records are never edited. `--supersede <ref> [--replacement <ref>]` appends
  a supersession event; `--retract <ref>` appends a retraction. An
  untraceable delta is not filed.

A delta is not a deviation: `/deviate` stops a run to get approval for
diverging from the confirmed plan *before* resuming; a delta records what the
merged code turned out to do. A delta whose cause is a mid-run divergence
should cite the approved `dN`, not just a claim.

## Reading the ledger

```bash
devague oblige --list            # frame obligations, with drift markers
devague plan oblige --list       # per-criterion obligations
devague evidence --list          # every evidence record, outcome and strength
devague delta --list             # deltas plus the supersession-event log
devague summary                  # the Delivery Claims table /summarize-delivery starts from
```

Read these before the "Ticket done?" node is decided and before the delivery
summary is written. If a success signal has no evidence row, the summary says
so, and the person at the node has what they need to answer *not yet*.
