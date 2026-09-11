# Upstream ask: one-transaction bulk confirm in devague

**Repository:** `agentculture/devague`
**Type:** `Feature`
**Status:** DRAFT — not yet posted. This file is the text; the operator posts
it (the `communicate` skill's issue lane), then records the resulting URL in
[`docs/operations/pr-upkeep-lane.md`](../operations/pr-upkeep-lane.md)'s
"The readiness block" section and in the loop-closure spec's non-goal.

Filed as the sibling-repo half of culture-nodes #317 (plan
`loop-closure-claude-codex`, task t18, spec decision **c14**): the merge-gate
readiness collector counts un-confirmed devague records, and counting them is
what makes the missing bulk confirm measurable. Building the confirm here
would be building devague's CLI inside culture-nodes, so it is a non-goal in
that spec and an issue here instead.

---

## Title

`review`: confirm every proposed record of a plan in one transaction

## Body

### What is missing

`devague 0.24.0` has no way to confirm more than one record at a time:

- `review` has no `--confirm-all`;
- `--from-review` exists only on `confirm`, and only for claims and honesty
  conditions;
- `deviate`, `evidence`, `delta`, `lapse` and `oblige` each take a single
  `--confirm ID`.

So the only way to clear a plan's proposed records is one CLI invocation per
record, per noun.

### Why it matters, with the number

culture-nodes' own committed `.devague` tree currently carries **236 proposed
records** across five nouns:

| Noun | Proposed |
| --- | --- |
| `obligations` | 101 |
| `evidence` | 80 |
| `deviations` | 35 |
| `deltas` | 13 |
| `tasks` | 7 |

That is 236 hand invocations to adjudicate one cycle's records, which is why
in practice they are not adjudicated at all — they accumulate, and a
`proposed` record that nobody ever decides is indistinguishable from one
nobody has got to yet. The authority model (an agent may only propose; a human
confirms or rejects) is sound; the *ergonomics* of the human half are what
have failed, and the backlog above is the measurement.

This is now visible from outside devague: culture-nodes' merge-gate readiness
collector (`examples/pr-upkeep/readiness.py`) reads the checkout's `.devague`
documents and puts the proposed-record count in the block a maintainer reads
before every merge. Every merge decision in that loop now shows this number.

### What would close it

A bulk confirm that stays **one human decision, one transaction** — not a
loop that hides 236 decisions behind one command:

- `devague review --confirm-all --plan <slug>` (and/or `--frame <slug>`),
  which prints the full set it is about to confirm, requires an explicit
  confirmation, and writes them in one append;
- `--from-review` extended past claims and honesty conditions to the other
  nouns, so the same review pass that produced the verdicts can apply them;
- scoping flags that make a partial decision expressible rather than
  all-or-nothing: `--kind evidence`, `--since <date>`, `--task <id>`.

Two properties that should not be lost:

1. **A confirm stays a human act.** The ask is to stop making the human type
   236 commands, not to let an agent confirm its own proposals.
2. **Records stay immutable and append-only.** A bulk confirm appends 236
   decisions; it does not mutate 236 records in place.

### Non-goals for this issue

- Any change to the authority model itself.
- A hand-turn or record-only noun inside devague (culture-nodes tracks
  hand-turns in its own ledger; that is not devague's job).

---

### Grounding for the numbers above

Recount with:

```bash
python3 - <<'PY'
import json, glob
from collections import Counter
c = Counter()
for f in sorted(glob.glob(".devague/*/*.json")):
    d = json.load(open(f))
    for k in ("claims", "tasks", "evidence", "deviations", "deltas", "obligations"):
        for e in d.get(k, []):
            if e.get("status") == "proposed":
                c[k] += 1
print(c, sum(c.values()))
PY
```
