<!-- markdownlint-disable MD041 -->
<!-- Body template for a `Record` issue, rendered by scripts/open-issue.sh. -->
<!-- It posts as an issue body where GitHub supplies the title, so there is -->
<!-- no H1 here on purpose. Placeholders use double-brace syntax, matching -->
<!-- the convention in .claude/skills/communicate/scripts/templates/. -->

## What this records

{{SUMMARY}}

## Committed artifact

`{{ARTIFACT_PATH}}`

This issue is a **pointer, not a home**. The record itself lives in the tree
and is what a reader should open — under `docs/deviations/`, `docs/audits/`,
`docs/decisions/`, `docs/adr/` or `docs/deliveries/`. A Record whose only
content is this issue body is the failure the type exists to prevent.

The path above must exist and be tracked by git: `scripts/close-issue.sh`
refuses to close this issue with `--artifact` otherwise.

## Why this is a Record and not a Task

{{WHY_RECORD}}

A Record is **complete when it was written** and **closed on read**. It has
no run to cite and no test to run, because there is no outstanding work in
it — a deviation, an audit snapshot, or a counted operator hand-turn is a
fact, not a plan. Types keep those facts countable without letting them read
as outstanding workload.

## Context

{{CONTEXT}}

## How this closes

```bash
scripts/close-issue.sh <issue> closed-with-evidence \
  "record, complete when written" --artifact {{ARTIFACT_PATH}}
```
