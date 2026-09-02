# The Jira service-account token: mint, verify, install, rotate

This is the runbook for the one credential the pr-upkeep sweep and the jira
bridge share, written so the token is never lost again (issue #273, plan
task t11). The short form is a CLI verb — `nodes jira-token mint`,
`nodes jira-token verify`, `nodes jira-token install` — and this page is
the long form the verb points at. Every fact below was verified on
2026-09-02.

## What the account is

| Field | Value |
|---|---|
| Atlassian service account | `culture-nodes` |
| Email | `culture-spark-9lgwfn7mz2@serviceaccount.atlassian.com` |
| accountId | `712020:5e0ae915-ba1a-43ef-bce0-c0d5ff9bb615` |
| Site | `https://agentculture.atlassian.net` |
| Cloud id | `0610b05c-63f8-4935-bd7f-a30f907bba8c` |
| API gateway base (`JIRA_API_BASE`) | `https://api.atlassian.com/ex/jira/0610b05c-63f8-4935-bd7f-a30f907bba8c` |

Everything the system writes to the board — sweep comments, transitions,
the bridge's four verbs — lands under this account. The sweep filters that
account's own comments as self-echo by `jira_bot_account_id`, which is why
the accountId is part of the runner grant (step 5) and not only of the
credential.

## Where a token is minted (and re-minted)

Only in the Atlassian admin UI:

`admin.atlassian.com -> Directory -> Service accounts -> culture-nodes -> API tokens -> Create`

No API mints a service-account token, so the CLI cannot mint one either:
`nodes jira-token mint` prints this path, the gateway base, the accountId to
expect, and the env-file shape below. A token is shown once at creation;
paste it straight into the env file.

## The env file (0600)

`~/.config/agent/jira-service-account.env`, mode `0600`, on spark:

```text
JIRA_ACCOUNT_EMAIL=culture-spark-9lgwfn7mz2@serviceaccount.atlassian.com
JIRA_API_TOKEN=<paste from the admin UI>
JIRA_API_BASE=https://api.atlassian.com/ex/jira/0610b05c-63f8-4935-bd7f-a30f907bba8c
```

`JIRA_API_BASE` is not optional for this account. A service-account token
authenticates **only** against the API gateway base: the site URL
`https://agentculture.atlassian.net` answers 401 for it and the gateway
answers 200. That is the single most confusing symptom on this path, and
`verify`'s hint names it.

## Verify

```bash
set -a; . ~/.config/agent/jira-service-account.env; set +a
nodes jira-token verify
```

`verify` calls `GET $JIRA_API_BASE/rest/api/3/myself` with Basic auth
(`email:token`) using the standard library only, and must print

```text
accountId: 712020:5e0ae915-ba1a-43ef-bce0-c0d5ff9bb615
```

Exit `2` with an `error:`/`hint:` pair on a missing variable (the hint names
the missing names), a 401/403, or a network failure; exit `1` on a non-https
base. The token value never appears in any output, `--json` included.

## Install — five operator hand-turns, run from spark in ONE shell

Each step is a hand-turn and therefore an issue comment (CLAUDE.md,
"Every piece of operator work opens or updates an issue"). `nodes jira-token
install` prints exactly this sequence; it runs nothing.

1. Export the pair and the gateway base in one shell:

   ```bash
   set -a; . ~/.config/agent/jira-service-account.env; set +a
   ```

   Every later step runs from this same shell.

2. Verify the token against the gateway base:

   ```bash
   nodes jira-token verify
   ```

   Must print `accountId: 712020:5e0ae915-ba1a-43ef-bce0-c0d5ff9bb615`.
   Stop here on anything else.

3. Land the pair in the runner secrets on thor and orin:

   ```bash
   deploy/prod/install-secrets.sh
   ```

   Its Jira lane rewrites `~/.culture-nodes/runner-secrets.env` on both
   hosts with the pair plus `JIRA_API_BASE`. It refuses when only one of
   email/token is set, and when all three are unset it leaves the file
   untouched (it never clears a grant by omission).

4. Merge the keys into the jira bridge env and restart it (thor only).
   First check that no jira bridge session is in flight — never restart a
   bridge mid-session:

   ```bash
   ssh thor "pgrep -af '[j]ira'"
   deploy/prod/deploy.sh thor
   ```

   `deploy_jira` merges `JIRA_API_BASE` (with the transition keys) into
   `~/.culture-nodes/jira-bridge-jira.env` and restarts `jira-bridge`. The
   pair in that file is written by **no lane**: on a rotation, edit its
   `JIRA_ACCOUNT_EMAIL` / `JIRA_API_TOKEN` lines by hand on thor (`umask
   077`, mode stays `0600`) before running `deploy.sh`, so the restart
   picks up the new token.

5. Re-grant the bot account id to the sweep and restart the runners.
   `deploy/prod/lanes/runner-env-write.sh` reads `HOST`, `REVISION`,
   `NODES_API_URL`, `PR_UPKEEP_REPOSITORIES` and the `PR_UPKEEP_SWEEP_*`
   overrides from the shell and retains whatever it is not given. Every
   repository entry carries `jira_bot_account_id`:

   ```bash
   export HOST=thor REVISION=$(git rev-parse HEAD) NODES_API_URL=http://thor:18080
   export PR_UPKEEP_REPOSITORIES='{"cycle":0,"repositories":[{"github_repo":"agentculture/culture-nodes","sonar_component":"agentculture_culture-nodes","jira_site":"agentculture.atlassian.net","jira_project":"SCRUM","jira_bot_account_id":"712020:5e0ae915-ba1a-43ef-bce0-c0d5ff9bb615"}]}'
   deploy/prod/lanes/runner-env-write.sh
   HOST=orin deploy/prod/lanes/runner-env-write.sh
   ssh thor "systemctl --user restart nodes-runner"
   ssh orin "systemctl --user restart nodes-runner"
   ```

Then: one sweep interval later the `pr-upkeep-sweep-cycle` run is green,
and a comment posted from the **operator's own browser login** is recorded
as a human fact, not self-echo. That is the proof the grant took.

## The self-echo caveat

Do not post that proof comment through the `jira` skill. The skill reads
`runner-secrets.env` on thor and therefore posts **as the bot** — which the
sweep filters as self-echo by the very accountId step 5 granted. A human
fact needs a human account: the operator's own browser session on the
board.

## Rotation

Mint a new token in the admin UI, revoke the old one there, update the env
file, and repeat steps 1–5. Nothing else holds the token: `runner-secrets.env`
on thor and orin (written by step 3) and `jira-bridge-jira.env` on thor (the
hand edit in step 4) are the only copies.
