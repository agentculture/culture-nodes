# Closing-comment template

Every issue closed during the backlog cycle must use `scripts/close-issue.sh`.
The helper refuses a closure without all three checkability fields and sends
the comment with the close operation; do not use a bare `gh issue close`.

```text
Disposition: <closed-with-evidence | closed-with-reason | scheduled-in-batch>

Reason: <why this disposition is correct>

Culture Nodes run id: `<resolvable run id>`
```

When no Culture Nodes run produced the evidence, replace the final field with:

```text
Test path: `<repository-relative test path>`

Command: `<exact command that runs the test>`
```

Worked-example reference: culture-nodes issue #59. Its closing comment shows
the expected explanatory depth; this template adds fixed field labels so the
minimum checkability contract is machine- and reader-visible.
