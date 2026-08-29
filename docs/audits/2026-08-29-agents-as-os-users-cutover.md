# Cutover audit — agents as OS users (#243, plan task t9)

Date: 2026-08-29. Operator lane, from spark. Every line below is a pasted
command output; nothing is asserted from a deploy log alone (spec h17).
Spec: `docs/specs/2026-08-29-agents-as-os-users-243.md`. Deviation record:
`docs/deviations/2026-08-29-agents-as-os-users.md`.

## Sequence

1. `deploy/prod/bootstrap-accounts.sh spark|orin|thor` — typed by the operator
   (three hand-turns, each a comment on #243). Created `culture-claude` +
   `culture-qwen` (spark), `culture-codex` (orin, thor).
2. `deploy/prod/install-secrets.sh thor orin` twice: once plain (mirrors
   `codex-bridge.env` into the codex accounts), once with `GITHUB_TOKEN_WORKER`
   in its environment (relays `bridge-push.env` into both codex accounts and
   `culture-claude@localhost`).
3. `deploy.sh thor` — first run refused at the session check with a phantom
   session: the pgrep pattern matched its own ssh shell (fixed in `bc8dd5a`,
   bracket idiom + a real-pgrep regression test). Second run: codex-bridge cut
   over to `culture-codex@thor`, control plane on `bc8dd5a`, exit 3 (parity
   pending orin — the two-host sequence's normal first-host exit).
4. `deploy.sh orin` — codex-bridge cut over to `culture-codex@orin`, parity
   holds, sweep resumed, exit 0.
5. Six spark actors re-registered at spark's tailscale address (#248 — the
   restart moved spark's DHCP lease from `192.168.1.157` to `.118`; not #243
   work, recorded as its own Bug with the hand-turn).
6. `deploy.sh spark` — five bridges cut over to `culture-claude` /
   `culture-qwen`, exit 0.

## Honesty conditions, by pasted output

Probe run from spark as the login user (`ssh <host>`) and as each account
(`ssh culture-<engine>@<host>`, `@localhost` on spark):

```text
== spark login
### spark-f8a9 as spark
h3 login-user unit culture-nodes-claude-developer: failed / disabled
h3 login-user unit culture-nodes-claude-planner: failed / disabled
h3 login-user unit culture-nodes-claude-verifier: failed / disabled
h3 login-user unit culture-nodes-claude-intake: failed / disabled
h3 login-user unit culture-nodes-qwen-developer: failed / disabled
h12 culture-claude groups=[culture-claude]
h28 /home/culture-claude mode=750
h12 culture-qwen groups=[culture-qwen]
h28 /home/culture-qwen mode=750
h13 as login: ls /home/culture-*/ -> ls: cannot open directory '/home/culture-claude/': Permission denied
== culture-claude@localhost
### spark-f8a9 as culture-claude uid=1001
h3 account unit culture-nodes-claude-developer: active pid-user=culture-claude
h3 account unit culture-nodes-claude-intake: active pid-user=culture-claude
h3 account unit culture-nodes-claude-planner: active pid-user=culture-claude
h3 account unit culture-nodes-claude-verifier: active pid-user=culture-claude
h13 ls /home/spark/git/culture-nodes -> ls: cannot access '/home/spark/git/culture-nodes': Permissio
h28 envs: bridge-push.env=600 developer.env=600 
h29 inventory ~/.culture-nodes: bridge-push.env dialin 
h29 prod.env keys anywhere: 0
h32 NODES_HUMAN_DECISION_TOKEN under ~/.config ~/.culture-nodes: 0
h23 claude 2.1.251 (Claude Code)
h14 claude auth: {
h21 clone /home/culture-claude/git/culture-nodes-developer/ gitdir=.git HEAD=67032ce
h21 clone /home/culture-claude/git/culture-nodes-intake/ gitdir=.git HEAD=67032ce
h21 clone /home/culture-claude/git/culture-nodes-planner/ gitdir=.git HEAD=67032ce
h21 clone /home/culture-claude/git/culture-nodes-verifier/ gitdir=.git HEAD=67032ce
h5 developer.json :8088 confinement=unix-user:culture-claude: none: `claude -p` runs with this bridge process's own privileges
h5 intake.json :8086 confinement=unix-user:culture-claude: none: `claude -p` runs with this bridge process's own privileges
h5 planner.json :8087 confinement=unix-user:culture-claude: none: `claude -p` runs with this bridge process's own privileges
h5 verifier.json :8089 confinement=unix-user:culture-claude: none: `claude -p` runs with this bridge process's own privileges
== culture-qwen@localhost
### spark-f8a9 as culture-qwen uid=1002
h3 account unit culture-nodes-qwen-developer: active pid-user=culture-qwen
h13 ls /home/spark/git/culture-nodes -> ls: cannot access '/home/spark/git/culture-nodes': Permissio
h28 envs: 
h29 inventory ~/.culture-nodes: 
h29 prod.env keys anywhere: 0
h32 NODES_HUMAN_DECISION_TOKEN under ~/.config ~/.culture-nodes: 0
h23 qwen 0.22.0
h21 clone /home/culture-qwen/git/culture-nodes-qwen-developer/ gitdir=.git HEAD=67032ce
h5 qwen-developer.json :8092 confinement=unix-user:culture-qwen: qwen-code runs its own tools in-process as the bridge user (measur
== orin login
### orin as orin
h3 login-user unit codex-bridge: inactive / disabled
h25 old checkout /home/orin/git/culture-nodes-agent HEAD=05b216e branch=jira-flow/wp-j dirty=0
h12 culture-codex groups=[culture-codex]
h28 /home/culture-codex mode=750
h13 as login: ls /home/culture-*/ -> ls: cannot open directory '/home/culture-codex/': Permission denied
== culture-codex@orin
### orin as culture-codex uid=2002
h3 account unit codex-bridge: active pid-user=culture-codex
h13 ls /home/spark/git/culture-nodes -> ls: cannot access '/home/spark/git/culture-nodes': No such f
h28 envs: codex-bridge.env=600 bridge-push.env=600 
h29 inventory ~/.culture-nodes: bin bridge-push.env codex-bridge.env codex-bridge.json codex-bridge-state 
h29 prod.env keys anywhere: 0
h32 NODES_HUMAN_DECISION_TOKEN under ~/.config ~/.culture-nodes: 0
h23 codex codex-cli 0.147.0
h14 codex login: Logged in using ChatGPT
h21 clone /home/culture-codex/git/culture-nodes-agent/ gitdir=.git HEAD=67032ce
h5 codex-bridge.json :8086 confinement=unix-user:culture-codex: codex enforces --sandbox with a bubblewrap helper backed by unpri
== thor login
### thor as thor
h3 login-user unit codex-bridge: inactive / disabled
h25 old checkout /home/thor/git/culture-nodes-agent HEAD=2f2778f branch=jira-flow/wp-i dirty=0
h12 culture-codex groups=[culture-codex]
h28 /home/culture-codex mode=750
h13 as login: ls /home/culture-*/ -> ls: cannot open directory '/home/culture-codex/': Permission denied
== culture-codex@thor
### thor as culture-codex uid=2002
h3 account unit codex-bridge: active pid-user=culture-codex
h13 ls /home/spark/git/culture-nodes -> ls: cannot access '/home/spark/git/culture-nodes': No such f
h28 envs: bridge-push.env=600 codex-bridge.env=600 
h29 inventory ~/.culture-nodes: bin bridge-push.env codex-bridge.env codex-bridge.json codex-bridge-state 
h29 prod.env keys anywhere: 0
h32 NODES_HUMAN_DECISION_TOKEN under ~/.config ~/.culture-nodes: 0
h23 codex codex-cli 0.147.0
h14 codex login: Logged in using ChatGPT
h21 clone /home/culture-codex/git/culture-nodes-agent/ gitdir=.git HEAD=67032ce
h5 codex-bridge.json :8086 confinement=unix-user:culture-codex: codex enforces --sandbox with a bubblewrap helper backed by unpri
== h15 passwordless: culture-claude@localhost=ok culture-qwen@localhost=ok culture-codex@orin=ok culture-codex@thor=ok 
== h7 registry os_user
company/codex-orin http://192.168.1.138:8086 os_user= culture-codex rev 2
company/codex-thor http://192.168.1.146:8086 os_user= culture-codex rev 2
company/developer http://100.127.105.72:8088 os_user= culture-claude rev 3
company/intake http://100.127.105.72:8086 os_user= culture-claude rev 2
company/jira-comment http://192.168.1.146:8089 os_user= None rev 1
company/notify-discord http://192.168.1.146:8088 os_user= None rev 1
company/planner http://100.127.105.72:8087 os_user= culture-claude rev 2
company/qwen-developer http://100.127.105.72:8092 os_user= culture-qwen rev 2
company/verifier http://100.127.105.72:8089 os_user= culture-claude rev 2
```

Readings:

- **h3** — every account unit `active` with `pid-user` = the account; every
  login-user unit `inactive`/`disabled` (orin, thor) or `failed`/`disabled`
  (spark: the claude/qwen bridges exit 143 on SIGTERM, which systemd records
  as `failed`; they are stopped and disabled all the same).
- **h5** — every advertising bridge's `confinement` begins
  `unix-user:culture-<engine>:` (read live from each bridge with its own
  bearer).
- **h7** — registry: `os_user=culture-codex` on codex-thor/codex-orin (rev 2),
  `culture-claude` on developer (rev 3) / intake / planner / verifier (rev 2),
  `culture-qwen` on qwen-developer (rev 2).
- **h11** — `culture-codex` on thor: uid 2002, groups `[culture-codex]`, home
  750, codex-bridge active under it.
- **h12** — `id -nG` for every account lists only the account; nodes-runner and
  compose untouched (orin deploy: `runner: active`, parity + sweep resumed).
- **h13** — `ls /home/spark/git/culture-nodes` as `culture-claude` and
  `culture-qwen`: `Permission denied`; the login user cannot list
  `/home/culture-*` either.
- **h14** — `codex login status` as culture-codex on orin and thor: `Logged in
  using ChatGPT`; `claude auth status` as culture-claude: `"loggedIn": true,
  "authMethod": "claude.ai"`.
- **h15** — passwordless `ssh` to all four account targets: ok.
- **h21** — every account clone's `git rev-parse --git-dir` is `.git` inside
  the account home (no worktree pointer into the operator checkout).
- **h23** — codex-cli 0.147.0, claude 2.1.251, qwen 0.22.0 as the accounts.
- **h25** — old login-user checkouts untouched: orin `05b216e` on
  `jira-flow/wp-j`, thor `2f2778f` on `jira-flow/wp-i`, both clean, same HEADs
  as before the cutover.
- **h27** — rollback pair exercised once on orin: account unit stopped →
  login-user unit started (`pid-user=orin`, bridge answering on :8086) →
  reversed (`pid-user=culture-codex`). No deploy needed either way:

```text
account unit after stop: inactive
login unit after start: active pid-user=orin
bridge answers on :8086 -> HTTP 401
-- reverse (back to the account)
login unit after stop: inactive
account unit after start: active pid-user=culture-codex
```

- **h28** — `/home/culture-*` mode 750 on all hosts; every `*.env` under an
  account is 600.
- **h29** — account inventories: codex `bin bridge-push.env codex-bridge.env
  codex-bridge.json codex-bridge-state`; culture-claude `bridge-push.env
  dialin`; culture-qwen empty. No `prod.env` key anywhere under an account.
- **h32** — `NODES_HUMAN_DECISION_TOKEN` under `~/.config` and
  `~/.culture-nodes` of culture-claude: 0 files.
- **h26** (refusal path, live) — the first `deploy.sh thor` stopped at the
  session check before any `systemctl stop` (see step 3): the refusal fires
  where it should; its pattern was wrong, not its placement.

## Left open by this audit

- The human-inbox pair on spark still runs as `spark` at the previous
  revision: `deploy.sh thor`'s human-inbox lane could not reach spark at its
  stale address (#248). A re-run of `deploy.sh thor` now redeploys it.
- The proof dispatches (h1, h4, h9, h20) are plan task t10, not this audit.
