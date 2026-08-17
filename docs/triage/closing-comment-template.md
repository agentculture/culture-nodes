# Closing-comment template

Every issue closed during the backlog cycle must use `scripts/close-issue.sh`.
The helper refuses a closure without all three checkability fields and sends
the comment with the close operation; do not use a bare `gh issue close`.

```text
Disposition: <closed-with-evidence | closed-with-reason | scheduled-in-batch>

Reason: <why this disposition is correct>

Culture Nodes run id: `<resolvable run id>`
```

The evidence field is one of three shapes, and exactly one — the helper
refuses a closure that names several, and refuses one that names none.

When no Culture Nodes run produced the evidence, replace the final field with:

```text
Test path: `<repository-relative test path>`

Command: `<exact command that runs the test>`
```

When the issue is a **Record** — a deviation, an audit snapshot, a counted
operator hand-turn: complete when it was written, closed on read — there is
no run and no test to cite. Its evidence is the committed artifact it points
at, so replace the final field with:

```text
Artifact: `<repository-relative path to the committed record>`
```

Pass it as `scripts/close-issue.sh <issue> <disposition> <reason> --artifact
<path>`. The helper refuses a path that does not exist, and refuses one that
exists but is untracked by git (`git ls-files --error-unmatch`) — a record
that lives only on the author's disk is not evidence, and reads identically
to a committed one from inside the closing shell. The issue is a pointer,
never the home: the record itself belongs in `docs/deviations/`,
`docs/audits/`, `docs/decisions/`, `docs/adr/` or `docs/deliveries/`.

Worked-example reference: culture-nodes issue #59. Its closing comment shows
the expected explanatory depth; this template adds fixed field labels so the
minimum checkability contract is machine- and reader-visible.
