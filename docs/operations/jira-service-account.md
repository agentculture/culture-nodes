# The Jira service-account token: mint, seal, verify, install, rotate

This is the runbook for the one credential the pr-upkeep sweep and the jira
bridge share, written so the token is never lost again (issue #273, plan
task t11). The short form is a CLI verb — `nodes jira-token mint`,
`nodes jira-token seal`, `nodes jira-token verify`,
`nodes jira-token install` — and this page is the long form the verb points
at. Every fact below was verified on 2026-09-02.

The operator's decision on the amendment: the token is **never written to a
plaintext file on spark**. It lives hidden in `grant`, the per-user secrets
manager, and reaches a process only through `grant run --inject`.

## What the account is

| Field | Value |
|---|---|
| Atlassian service account | `culture-nodes` |
| Email | `culture-spark-9lgwfn7mz2@serviceaccount.atlassian.com` |
| accountId | `712020:5e0ae915-ba1a-43ef-bce0-c0d5ff9bb615` |
| Site | `https://agentculture.atlassian.net` |
| Cloud id | `0610b05c-63f8-4935-bd7f-a30f907bba8c` |
| API gateway base (`JIRA_API_BASE`) | `https://api.atlassian.com/ex/jira/0610b05c-63f8-4935-bd7f-a30f907bba8c` |
| grant secret name | `JIRA_SERVICE_ACCOUNT_TOKEN` |

Everything the system writes to the board — sweep comments, transitions,
the bridge's four verbs — lands under this account. The sweep filters that
account's own comments as self-echo by `jira_bot_account_id`, which is why
the accountId is part of the runner grant (step 5) and not only of the
credential.

`JIRA_ACCOUNT_EMAIL` and `JIRA_API_BASE` are not secrets: `verify` defaults
them to the values above, and the install steps export them in the clear.
Only the token is sealed. `JIRA_API_BASE` is nonetheless pinned to the
gateway rather than merely defaulted — see [Verify](#verify).

## Where a token is minted (and re-minted)

Only in the Atlassian admin UI:

`admin.atlassian.com -> Directory -> Service accounts -> culture-nodes -> API tokens -> Create`

No API mints a service-account token, so the CLI cannot mint one either:
`nodes jira-token mint` prints this path, the gateway base, the accountId to
expect, and the seal/inject commands below. A token is shown once at
creation; paste it straight into `nodes jira-token seal`.

## Seal the token in grant

```bash
nodes jira-token seal
```

On a terminal this prompts without echo. In a pipeline it reads one line
from stdin, so `printf %s "$TOKEN" | nodes jira-token seal` works too. It
then runs

```bash
grant set JIRA_SERVICE_ACCOUNT_TOKEN - --hidden --purpose "..." --rotate-howto "..."
```

with the token on stdin — never in an argv, never logged, never in a file.
*Hidden* means the store refuses to print the value: `grant get` and
`grant env` are refused, and the only way to consume it is
`grant run --inject JIRA_API_TOKEN=JIRA_SERVICE_ACCOUNT_TOKEN -- <cmd>`,
which sets the variable for that one process. `grant show
JIRA_SERVICE_ACCOUNT_TOKEN` prints its metadata (purpose, rotation how-to,
timestamps — never the value) and `grant list` prints names only, so "is it
sealed?" is answerable without exposing anything. Sealing again overwrites.

An empty token is exit `1`; `grant` missing from PATH (it is
`~/.local/bin/grant` on spark, 0.9.0) or a failing `grant set` is exit `2`
with grant's stderr quoted, token scrubbed. Secret names on that release
must match `^[A-Z_][A-Z0-9_]{0,63}$`, which is why the name is upper-case.

`JIRA_API_BASE` is not optional for this account. A service-account token
authenticates **only** against the API gateway base: the site URL
`https://agentculture.atlassian.net` answers 401 for it and the gateway
answers 200. That is the single most confusing symptom on this path, and
`verify`'s hint names it.

### Why grant, not a file

A `0600` env file is still plaintext: every process running as the user can
read it, every backup or home-directory copy carries it, and `set -a; .
file` puts it in the environment of the whole shell and everything the
shell starts. grant's hidden secrets are consumable only by `grant run`,
for one named process, and the CLI contract refuses to print them
(`grant explain hidden` is honest that the store itself is a `0600` file —
the gain is that nothing *sources* it and nothing prints it). Rotation is
one `seal`, not an edit to a file that three other steps then re-read.

## Verify

```bash
grant run --inject JIRA_API_TOKEN=JIRA_SERVICE_ACCOUNT_TOKEN -- nodes jira-token verify
```

`verify` calls `GET $JIRA_API_BASE/rest/api/3/myself` with Basic auth
(`email:token`) using the standard library only, and must print

```text
accountId: 712020:5e0ae915-ba1a-43ef-bce0-c0d5ff9bb615
```

That id is checked, not merely printed. A 200 says the token is *valid*;
it does not say it is *ours* — an operator's own token, or a second service
account, authenticates at this gateway just as well and would install
cleanly through the remaining steps. The sweep filters its own Jira
comments by `jira_bot_account_id`, which is exactly this id, so a pair
installed under the wrong account would make the bot's comments read back
as human facts. `verify` therefore exits `2` on any other `accountId`,
naming both, and the mismatch stops at step 2 rather than at step 5.

Email defaults to the constant above and may be overridden by exporting
`JIRA_ACCOUNT_EMAIL`; overriding it does not widen what passes, because the
answered account is compared either way. The base is different:
`JIRA_API_BASE` may only ever name the gateway above. Any other value —
another host, another cloud id, a `http://` spelling — is exit `1` with a
hint, and no request is built.

That is deliberate, and it is a security property rather than a
convenience. `verify` attaches the service-account email and token to
whatever base it is handed, so a base read from the environment is a choice
of *who receives the credential*. The whole point of sealing the token in
grant is that a process which can run `verify` under `grant run --inject`
still cannot read the secret out; letting that same process point
`JIRA_API_BASE` at a host of its own would hand it the token anyway. Since
this account's token authenticates at exactly one address, pinning costs
nothing.

Without a token in the environment, a terminal is prompted with `getpass`
(no echo) and a non-terminal exits `2` with the `grant run` line above as
its hint. Exit `2` with an `error:`/`hint:` pair on a 401/403, a wrong
account, or a network failure. The token value never appears in any output,
`--json` included.

## Install — five operator hand-turns, run from spark

Each step is a hand-turn and therefore an issue comment (CLAUDE.md,
"Every piece of operator work opens or updates an issue"). `nodes jira-token
install` prints exactly this sequence; it runs nothing. No step sources a
file.

1. Seal the token in grant (once):

   ```bash
   nodes jira-token seal
   ```

   Skip when `grant show JIRA_SERVICE_ACCOUNT_TOKEN` already succeeds.

2. Verify the token against the gateway base:

   ```bash
   grant run --inject JIRA_API_TOKEN=JIRA_SERVICE_ACCOUNT_TOKEN -- nodes jira-token verify
   ```

   Must print `accountId: 712020:5e0ae915-ba1a-43ef-bce0-c0d5ff9bb615`.
   Stop here on anything else.

3. Land the pair in the runner secrets on thor and orin:

   ```bash
   export JIRA_ACCOUNT_EMAIL=culture-spark-9lgwfn7mz2@serviceaccount.atlassian.com JIRA_API_BASE=https://api.atlassian.com/ex/jira/0610b05c-63f8-4935-bd7f-a30f907bba8c
   grant run --inject JIRA_API_TOKEN=JIRA_SERVICE_ACCOUNT_TOKEN -- deploy/prod/install-secrets.sh
   ```

   The lane reads the three from its environment: email and base are
   non-secret exports, the token is injected for that one process. Its Jira
   lane rewrites `~/.culture-nodes/runner-secrets.env` on both hosts with
   the pair plus `JIRA_API_BASE`. It refuses when only one of email/token is
   set, and when all three are unset it leaves the file untouched (it never
   clears a grant by omission).

4. Merge the keys into the jira bridge env and restart it (thor only).
   First check that no jira bridge session is in flight — never restart a
   bridge mid-session:

   ```bash
   ssh thor "pgrep -af '[j]ira'"
   grant run --inject JIRA_API_TOKEN=JIRA_SERVICE_ACCOUNT_TOKEN -- deploy/prod/deploy.sh thor
   ```

   `deploy_jira` merges `JIRA_API_BASE` (exported in step 3, with the
   transition keys) into `~/.culture-nodes/jira-bridge-jira.env` and
   restarts `jira-bridge`. The pair in that file is written by **no lane**:
   it is a hand edit on thor (`umask 077`, mode stays `0600`). The token
   reaches that file by the operator pasting it once, or — if it is also
   sealed on thor's grant — by writing the line under `grant run` there.
   On a rotation do that edit before `deploy.sh`, so the restart picks up
   the new token.

5. Re-grant the bot account id to the sweep and restart the runners.
   `deploy/prod/lanes/runner-env-write.sh` reads `HOST`, `REVISION`,
   `NODES_API_URL`, `PR_UPKEEP_REPOSITORIES` and the `PR_UPKEEP_SWEEP_*`
   overrides from the shell and retains whatever it is not given. Every
   repository entry carries `jira_bot_account_id`:

   ```bash
   export HOST=thor REVISION=$(git rev-parse HEAD) NODES_API_URL=http://thor:18080
   export PR_UPKEEP_REPOSITORIES='{"cycle":0,"repositories":[{"github_repo":"agentculture/culture-nodes","sonar_component":"agentculture_culture-nodes","jira_site":"agentculture.atlassian.net","jira_project":"SCRUM","jira_bot_account_id":"712020:5e0ae915-ba1a-43ef-bce0-c0d5ff9bb615"}]}'
   bash deploy/prod/lanes/runner-env-write.sh   # not executable; run it through bash
   HOST=orin bash deploy/prod/lanes/runner-env-write.sh
   ssh thor "systemctl --user restart nodes-runner"
   ssh orin "systemctl --user restart nodes-runner"
   ```

   The lane is normally `source`d by `deploy.sh`, which brings `set -euo
   pipefail`, `say` and the timestamped-backup helper with it. Run standalone
   it supplies all three itself, so this hand-turn takes the same
   `runner.env.bak-<UTC>` on each host, prints the restore command, and exits
   `0` when the re-grant lands and non-zero only when it refuses. (Before
   0.47.4 the two helpers were simply missing here: the backup was skipped and
   a successful re-grant exited `127` — PR #282 review.)

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

Mint a new token in the admin UI, revoke the old one there, run
`nodes jira-token seal` again (it overwrites the sealed secret), and repeat
steps 2–5. The only other copies are `runner-secrets.env` on thor and orin
(written by step 3) and `jira-bridge-jira.env` on thor (the hand edit in
step 4).
