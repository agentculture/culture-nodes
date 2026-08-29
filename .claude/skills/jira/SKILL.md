---
name: jira
description: >-
  Comment on, create, and read Jira issues from the operator lane, using the
  Jira credential already granted to the sweep on thor (the credential never
  leaves that host — the script runs its API calls there over ssh). Verbs:
  `show <ISSUE>` reads summary, status and the last five comments with ids
  and author account ids; `comment <ISSUE> "<text>"` posts a comment and
  prints its id; `create --project KEY --summary "<text>" [--type Task]
  [--description "<text>"]` files an issue and prints its key. Everything it
  writes lands UNDER THE SYSTEM'S JIRA ACCOUNT, so the sweep filters those
  comments as self-echo by account id
  (`examples/pr-upkeep/pr_upkeep_jira.py` `jira_comment_is_self_echo`): this
  is the operator's own hand on the board — for creating proof tickets and
  reading what the sweep will see — NOT a way to produce a human's fact. The
  system's own board moves go through the jira bridge actor's allowlisted
  verbs, never through this. Use when the operator says "comment on
  SCRUM-5", "open a proof ticket", "what does SCRUM-N look like". For status
  reads and transitions use the sibling `jira-status` skill.
type: command
---

# jira — operator-lane Jira comment / create / show

```bash
bash .claude/skills/jira/scripts/jira.sh show SCRUM-5
bash .claude/skills/jira/scripts/jira.sh comment SCRUM-5 "t13 proof: run 01M... finished"
bash .claude/skills/jira/scripts/jira.sh create --project SCRUM --summary "t13 proof ticket" \
  --type Task --description "Created from the operator lane."
```

## What it does

- `show <ISSUE>` — prints the summary, status, issue type, the account id the
  credential resolves to (the id the sweep treats as *self*), and the last
  five comments with `id`, author `accountId`, display name and created
  time. A comment authored by that same account is tagged `[self-echo]`, so
  the operator can read exactly what the sweep will and will not turn into
  a fact.
- `comment <ISSUE> "<text>"` — `POST /rest/api/3/issue/{key}/comment` with
  an ADF document body (blank-line separated paragraphs, single newlines as
  hard breaks); prints the new comment id.
- `create --project KEY --summary "<text>" [--type Task] [--description "<text>"]`
  — `POST /rest/api/3/issue`; prints the new key. `--type` defaults to
  `Task`.

Issue keys are validated locally against `^[A-Z][A-Z0-9_]*-[1-9][0-9]*$`
and project keys against `^[A-Z][A-Z0-9_]*$` **before any ssh**; a refusal
exits non-zero with `error:` / `hint:` lines on stderr. Results go to
stdout only.

Status reads and transitions are deliberately not here — use
`jira-status` (`status <ISSUE>` / `move <ISSUE> "<target>"`).

## Whose voice this is

Every write this skill makes is authored by the **system's** Jira account —
the same identity the jira bridge actor and the sweep use. The sweep's
`jira_comment_is_self_echo` (`examples/pr-upkeep/pr_upkeep_jira.py`) drops
the newest comment when its author account id equals the configured bot
account id, so a comment posted here is filtered as the system talking to
itself. That is the correct behaviour, and it is why this skill is not a
way to manufacture a human's answer: an operator who wants their words to
become an engine fact must speak on the board as a *distinct* identity
(issue #197, gap 3). What this skill is good for is the operator's own
hand-work — creating a proof ticket, seeding a description, reading state.

The system's own board moves are not this either: workflow runs comment
and transition through the jira bridge actor (`adapters/jira`), whose verbs
are bridge-allowlisted to one project prefix and named transitions
(decision c19). This skill does not widen or bypass that custody; it is the
same act as the operator typing on the board, scripted.

## Custody

Identical to `jira-status`: the Jira credential pair lives in
`~/.culture-nodes/runner-secrets.env` on thor (granted to the sweep; reuse
approved by the operator 2026-08-18). The script sshes to thor and performs
the REST calls there inside a quoted python heredoc; the pair is read from
that file on that host and never appears on the local machine, in argv, or
in output. `JIRA_STATUS_HOST` / `JIRA_STATUS_SITE` override the host and
site exactly as they do for `jira-status`.

## Why it exists

Issue #197 measured two gaps on the SCRUM-3 live probe: **ticket creation
had no lane verb** (gap 2 — creating SCRUM-3 needed an ad-hoc widening of
the jira-status ssh custody), and **the operator's board voice is filtered
as self-echo** (gap 3). The t8 proof ticket SCRUM-5 for the #230 closeout
was then created the same way — an ad-hoc script scp'd to thor — which is a
counted hand-turn. This skill is the declared, tested custody for that
create/comment/read work, so the next proof ticket is a verb and not a
widening. Gap 3 stays open by design: this skill names the self-echo, it
does not paper over it.
