# nodes-culture-dev hand-turns (#273)

> nodes.culture.dev is live: 0.47.0 runs on thor and orin, the tunnel unit on thor serves the loopback Access listener, the Access app pins an 8h session to ori's email, ticket-page links carry the public origin, and the sweep and jira bridge act as the dedicated service account so the operator's own Jira comments count as human facts — every host step ticked on #273 with its output
> instruction: Apply the recipe docs/operations/nodes-culture-dev.md in the order the frame fixes (stash clones, deploy pair, cloudflared, setup, prod.env Access keys, token file, unit, UI base URL re-grant, verification pair, Jira token install + re-grant), tick each on #273 with output, then run the t18 second sitting

## Audience

- The operator applying the hand-turns today, and whoever rotates the Jira service-account token or re-provisions the tunnel later without this session's context

## Before → After

- Before: Thor serves 15fefdef, no loopback listener, nodes.culture.dev unresolved, ticket links point at <http://thor:18080>, the sweep still acts as the operator's own Jira user so every operator comment is self-echo, and the token minted on 2026-09-02 exists only on the operator's clipboard
- After: Both prod hosts run 857cb49; <https://nodes.culture.dev> answers through the tunnel with the same revision as the LAN port; the Access app allows `<operator-email>` with an 8h session and its AUD is pinned in thor's prod.env; every Jira page link reads <https://nodes.culture.dev/tickets/>`KEY`; the sweep and jira bridge authenticate as the service account via the gateway base with its accountId granted as `jira_bot_account_id`; every step is ticked on #273 with the output the recipe asks for; the token runbook is a nodes verb and a doc

## Requirements

- Deploy 0.47.0 (main 857cb49) to thor then orin before any tunnel step: thor's /v1alpha1/version reports revision 15fefdef, nothing listens on 127.0.0.1:18081, and prod.env has no `NODES_ACCESS_`\* key; deploy/prod/compose.thor.yml on main already publishes 127.0.0.1:18081:8081 and reads `NODES_ACCESS_LISTEN`/`TEAM_DOMAIN`/AUD from prod.env
  - instruction: ssh culture-codex@thor 'cd git/culture-nodes-agent && git stash push -m #273-leftovers' and the same on orin; then deploy/prod/deploy.sh thor, deploy/prod/deploy.sh orin; verify curl <http://thor:18080/v1alpha1/version> and orin's worker docker inspect revision
  - honesty: Both hosts' /v1alpha1/version report revision 857cb49 after deploy.sh thor then deploy.sh orin, and the sweep is not left paused (deploy exits 0 on both)
- Clean the culture-codex engine clones first or deploy.sh refuses at provision (lanes/preflight.sh:42, lanes/unix-user.sh:339): thor's clone has 19 modified tracked files on top of the merged t8 commit, orin's 7 on top of merged t16 — codex leftovers whose handover refs were already harvested; stash them as the account (reversible) rather than discard, and count it as a hand-turn
  - instruction: git stash push -m '#273 codex leftovers' as culture-codex on each host; git stash list afterwards; paste both on #273
  - honesty: git stash list as culture-codex on thor and orin each shows one new entry dated today, and the clones are clean at the deploy's preflight
- Install cloudflared 2026.8.3 cloudflared-linux-arm64 (thor is aarch64, sudo is NOPASSWD) at /usr/local/bin/cloudflared, the absolute path deploy/prod/cloudflared-nodes.service execs; record the version on #273
  - instruction: ssh thor: curl -fsSL -o /tmp/cloudflared <https://github.com/cloudflare/cloudflared/releases/download/2026.8.3/cloudflared-linux-arm64> && sudo install -m 0755 /tmp/cloudflared /usr/local/bin/cloudflared && /usr/local/bin/cloudflared --version
  - honesty: /usr/local/bin/cloudflared --version prints 2026.8.3 on thor and the version is written on #273
- Run cultureflare remote-login setup over ssh thor bash -ic: cultureflare 0.15.0 lives in thor's ~/.local/bin and the Cloudflare credentials exist only in its interactive rc (never printed); pass --allow `<operator-email>` (the identity chat.agentculture.org-allow already uses) and --session-duration 8h at creation, because people.md says only a created app takes the flag; today the account has one Access app (chat.agentculture.org, 24h) and two tunnels, and nodes.culture.dev does not resolve
  - instruction: ssh -t thor bash -ic 'cultureflare remote-login setup --hostname nodes.culture.dev --service <http://127.0.0.1:18081> --allow `<operator-email>` --session-duration 8h' first without --apply to read the plan, then with --apply; capture the printed AUD and tunnel token from the terminal only
  - honesty: The Access apps list shows nodes.culture.dev with `session_duration` 8h and one allow policy naming `<operator-email>`; a tunnel named for nodes.culture.dev is healthy; no Cloudflare credential is printed in any transcript
- After setup, write `NODES_ACCESS_LISTEN`=:8081, `NODES_ACCESS_TEAM_DOMAIN`=agentculture.cloudflareaccess.com and `NODES_ACCESS_AUD`=<the new app's aud> into thor's prod.env by hand and restart the api container: install-secrets.sh writes no `NODES_ACCESS_`\* key, and the server refuses a partial tuple
  - instruction: On thor append `NODES_ACCESS_LISTEN`=:8081, `NODES_ACCESS_TEAM_DOMAIN`=agentculture.cloudflareaccess.com, `NODES_ACCESS_AUD`=`aud` to ~/.culture-nodes/prod.env (umask 077), then docker compose -f deploy/prod/compose.thor.yml up -d api; ss -ltn '( sport = :18081 )'
  - honesty: thor's prod.env carries exactly the three `NODES_ACCESS_`\* keys, the api container restarted, and ss shows 127.0.0.1:18081 only
- Re-grant `NODES_UI_BASE_URL` on both hosts (orin's worker env still reads <http://thor:18080>): remove-secret.sh `NODES_UI_BASE_URL` --yes thor orin, then install-secrets.sh, then redeploy; install-secrets.sh:790 leaves runner-secrets.env untouched when the Jira pair is unset in the shell, so this re-run cannot repeat the #253 clobber
  - instruction: From spark: deploy/prod/remove-secret.sh `NODES_UI_BASE_URL` --yes thor orin; deploy/prod/install-secrets.sh (Jira pair unset); deploy.sh thor; deploy.sh orin; grep ^`NODES_UI_BASE_URL`= on both hosts
  - honesty: grep `NODES_UI_BASE_URL` on thor and orin prints <https://nodes.culture.dev> and the orin worker's docker inspect env agrees; runner-secrets.env still lists all five sweep refs afterwards
- Install the Jira service account with `JIRA_ACCOUNT_EMAIL`, `JIRA_API_TOKEN` and `JIRA_API_BASE`=<https://api.atlassian.com/ex/jira/>`cloud-id` all exported in the deploying shell: runner-secrets.env on thor and orin carries the old pair and no `JIRA_API_BASE`, jira-bridge-jira.env on thor likewise; deploy.sh `deploy_jira` (line 670) merges `JIRA_API_BASE` only when exported; then re-grant `PR_UPKEEP_REPOSITORIES` with `jira_bot_account_id`=712020:5e0ae915-ba1a-43ef-bce0-c0d5ff9bb615 via lanes/runner-env-write.sh and restart nodes-runner on both hosts and jira-bridge on thor between sessions
  - instruction: In one spark shell export `JIRA_ACCOUNT_EMAIL`, `JIRA_API_TOKEN`, `JIRA_API_BASE`; run the c15 verify (myself prints the accountId); deploy/prod/install-secrets.sh (Jira lane) + deploy.sh thor (`deploy_jira`) with jira-bridge idle; runner-env-write.sh with `PR_UPKEEP_REPOSITORIES` gaining `jira_bot_account_id`; systemctl --user restart nodes-runner on both hosts; wait one sweep interval and read the run
  - honesty: runner-secrets.env on both hosts and jira-bridge-jira.env on thor carry `JIRA_API_BASE`; the runner grant carries `jira_bot_account_id`; the next sweep run is green
- Two hand-turns from the recipe's ledger are missing from #273's checklist and get added as a comment: the path-scoped Access Bypass policy for /v1alpha1/webhooks/jira and the Jira system webhook registration (with which auth mode the tenant supplies)
  - instruction: gh issue comment 273 with two new checklist lines: Bypass policy on /v1alpha1/webhooks/jira, Jira system webhook registration + auth mode observed
  - honesty: A comment on #273 lists the Bypass policy and the Jira webhook as steps 10 and 11 with tick boxes
- A nodes CLI verb (new `_commands` module, e.g. nodes jira-token with mint|verify|install subverbs, catalog entry included) plus docs/operations/jira-service-account.md carry the token runbook: where in Atlassian admin the service account culture-nodes lives and how a token is minted or re-minted (the UI is the only place that mints; the CLI cannot), the gateway base <https://api.atlassian.com/ex/jira/>`cloud-id` and why site-URL auth 401s for a service account, the myself check that must print the accountId, then the exact install sequence (install-secrets.sh Jira lane on thor+orin with all three exported, `deploy_jira`/bridge restart, runner-env-write.sh re-grant with `jira_bot_account_id`, nodes-runner restarts). verify runs the myself check; install runs the lane; nothing in the verb ever prints or stores the token
  - instruction: New `culture_nodes`/cli/`_commands`/`jira_token.py` registered in `_build_parser` with subverbs mint (prints the runbook), verify (curl myself via `JIRA_API_BASE` from env, prints accountId, never the token) and install (prints or runs the install sequence); catalog entry in explain/catalog.py; tests; docs/operations/jira-service-account.md; version bump
  - honesty: uv run nodes jira-token --help lists mint, verify and install; teken cli doctor --strict stays green with the new catalog entry; the doc names the Atlassian admin path and the gateway base; no test or doc contains a token value

## Honesty conditions

- Every clause of the announcement is backed by a ticked #273 step with pasted output, or is listed on #273 as not done; no clause is claimed from a plan
- The human-fact proof on #273 cites a Jira comment id whose author accountId is the operator's own (557058:dbaf9fdd-…), posted from the browser, and the sweep log shows it raised a human fact
- remove-secret.sh `NODES_HUMAN_DECISION_TOKEN_SECRET` is not run in this cycle unless #273 already carries the break-glass mint digest and a whoami output naming provider nodes-inbound-credential
- git diff main after the cycle touches only `culture_nodes`/cli/`_commands`/`new module`.py, `culture_nodes`/explain/catalog.py, tests for it, docs/operations/jira-service-account.md, CHANGELOG.md and pyproject.toml; no deploy/, compose or unit file changes
- The t18 audit gains a second-sitting section dated after #273 items 1-6 are ticked, and this cycle claims none of rows a, b, d as measured
- systemctl --user show cloudflared-nodes on thor reports User= empty (the login user) and the token file is under /home/thor/.config/cloudflared with mode 600
- docs/operations/jira-service-account.md can be followed by someone without this session: it names the admin path, the gateway base, the accountId, and the install commands verbatim
- Each clause of the after-state maps to one ticked #273 line whose pasted output shows it; a clause without output stays unticked
- The before-state facts are the ones the /scope entries s1-s11 recorded on 2026-09-02, and the delivery summary quotes them as the starting point
- Every success signal is a command whose output is pasted on #273; a signal that cannot be run (e.g. the tunnel curl without a service token or cookie) is recorded as unmeasured, not assumed

## Success signals

- curl of /v1alpha1/version through the tunnel (with the Access cookie or a service token) and on the LAN return identical revision bodies; ss shows 18081 bound on 127.0.0.1 only; grep `NODES_UI_BASE_URL` on both hosts prints the public origin; myself via the gateway prints the accountId; one sweep after the re-grant is green and a comment posted from the operator's own browser login is recorded as a human fact, not self-echo; nodes jira-token verify exits 0 on thor

## Scope / boundaries

- The operator's jira skill (.claude/skills/jira/scripts/jira.sh:93-110) reads thor's runner-secrets.env, so once the bot pair is installed it posts AS the bot; the final proof that an operator comment is a human fact must be a comment from the operator's own Jira login in the browser, never via the skill
- `NODES_HUMAN_DECISION_TOKEN_SECRET` stays in thor's prod.env until the break-glass credential exists: people.md break-glass step 1 (issue-dialin-credential.sh company/`handle` thor) needs a registered human actor (register-actor.sh + scripts/bind-identity.sh) first, and step 2's whoami must answer provider nodes-inbound-credential before remove-secret.sh runs (h25/c48)
- The t18 second measurement sitting (docs/audits/2026-09-02-login-from-anywhere-measurement-sitting.md rows a, b, d unmeasured) is triggered by these hand-turns, not part of them; it runs after #273 items 1-6 and appends to the audit

## Non-goals

- Repo changes in this cycle are limited to one thing: the nodes CLI runbook verb + operations doc for the Jira service-account token (c15). Machine-facing `NODES_API_URL` and `NODES_CALLBACK_BASE_URL` stay LAN addresses (spec c26); the Cloudflare API token and the tunnel token never enter the tree, prod.env, a unit or a compose file (recipe steps 2-3, finding s8); deploy.sh keeps not installing the tunnel unit

## Assumptions

- The tunnel unit runs under thor's login user: it owns prod.env and the control plane, Linger is already yes, and ~/.config/cloudflared does not exist yet

## Scope exploration

- `s1` — `thor /v1alpha1/version + ss -ltn + prod.env key names + deploy/prod/compose.thor.yml`: thor serves 15fefdef not 857cb49; no 18081 listener; no `NODES_ACCESS_`\* in prod.env; compose on main already publishes 127.0.0.1:18081:8081 — deploy is the first step
  - seeds: `c2`, `c6`
- `s2` — `culture-codex@thor and culture-codex@orin git/culture-nodes-agent`: 19 and 7 modified tracked files left by codex after merged t8/t16 (refs harvested); deploy.sh preflight refuses a dirty clone
  - seeds: `c3`
- `s3` — `thor uname/sudo + cloudflare/cloudflared releases/latest`: aarch64, sudo NOPASSWD, no cloudflared anywhere; latest release 2026.8.3 ships cloudflared-linux-arm64
  - seeds: `c4`
- `s4` — `thor bash -ic cultureflare + Cloudflare Access apps/tunnels/policies (names only)`: cultureflare 0.15.0 in ~/.local/bin, creds only in the interactive rc; one Access app (chat.agentculture.org, 24h, allow `<operator-email>`), tunnels chat + vllm only; nodes.culture.dev unresolved
  - seeds: `c5`, `c14`
- `s5` — `deploy/prod/install-secrets.sh + orin docker inspect worker env`: writes no `NODES_ACCESS_`\* key; `NODES_UI_BASE_URL` add-if-absent (orin worker still <http://thor:18080>); line 790 refuses to rewrite runner-secrets.env without the Jira pair
  - seeds: `c6`, `c7`
- `s6` — `thor/orin runner-secrets.env + jira-bridge-jira.env key names + deploy.sh deploy_jira`: old pair present, `JIRA_API_BASE` absent on both hosts and in the bridge env; `deploy_jira` merges it only when exported
  - seeds: `c8`
- `s7` — `.claude/skills/jira/scripts/jira.sh`: reads runner-secrets.env on thor — posts as whatever account the sweep holds
  - seeds: `c9`
- `s8` — `docs/operations/people.md (break-glass, session policy 8h)`: credential minted by issue-dialin-credential.sh for a registered+bound human actor; whoami must answer nodes-inbound-credential before the shared secret is removed; session duration 8h set at creation
  - seeds: `c10`, `c5`
- `s9` — `docs/operations/nodes-culture-dev.md ledger vs issue #273 body`: ledger items 9-10 (Bypass policy, Jira webhook) absent from the issue checklist
  - seeds: `c12`
- `s10` — `docs/audits/2026-09-02-login-from-anywhere-measurement-sitting.md + docs/deliveries remaining work`: rows a, b, d unmeasured pending deploy + #273 items 1-6; sitting is a follow-on
  - seeds: `c13`
- `s11` — `docs/specs/2026-09-01-login-from-anywhere (c26, c43, s8) + deploy/prod/cloudflared-nodes.service`: machine addresses unchanged; tokens never on disk except the per-tunnel token file; unit is token-mode, not deploy-managed
  - seeds: `c11`

## Open parks

- [unknown_nonblocking] First in-container JWKS fetch from prod-api-1 to agentculture.cloudflareaccess.com is proven only at the first login after deploy (plan risk carried over)
- [unknown_nonblocking] Whether this Jira tenant supplies X-Hub-Signature webhook secrets or only the URL token is unknown until the webhook is registered
