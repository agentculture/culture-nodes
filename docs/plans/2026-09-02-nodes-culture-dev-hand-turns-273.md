# Build Plan — nodes-culture-dev hand-turns (#273)

slug: `nodes-culture-dev-hand-turns-273` · status: `exported` · from frame: `nodes-culture-dev-hand-turns-273`

> nodes.culture.dev is live: 0.47.0 runs on thor and orin, the tunnel unit on thor serves the loopback Access listener, the Access app pins an 8h session to ori's email, ticket-page links carry the public origin, and the sweep and jira bridge act as the dedicated service account so the operator's own Jira comments count as human facts — every host step ticked on #273 with its output

## Tasks

### t1 — Stash the codex engine-clone leftovers on thor and orin (hand-turn)

- covers: c3, h2
- acceptance:
  - git stash push -m '#273 codex leftovers' run as culture-codex on both hosts; git status --short is empty and git stash list shows the new entry on each; both outputs pasted on #273

### t2 — Deploy 0.47.0 (857cb49) to thor then orin (hand-turn)

- depends on: t1
- covers: c2, h1
- acceptance:
  - deploy/prod/deploy.sh thor and deploy/prod/deploy.sh orin both exit 0 from a clean main checkout at 857cb49
  - curl <http://thor:18080/v1alpha1/version> reports revision 857cb49 and orin's worker docker inspect env shows the same revision; the sweep is not left paused

### t3 — Install cloudflared 2026.8.3 arm64 at /usr/local/bin/cloudflared on thor (hand-turn)

- covers: c4, h3
- acceptance:
  - /usr/local/bin/cloudflared --version prints 2026.8.3 on thor; the version line is pasted on #273

### t4 — Provision the tunnel, CNAME, Access app and allow policy with cultureflare from thor (hand-turn, once)

- covers: c5, h4
- acceptance:
  - cultureflare remote-login setup --hostname nodes.culture.dev --service <http://127.0.0.1:18081> --allow `ori.nachum@gmail.com` --session-duration 8h is run over ssh -t thor bash -ic, first without --apply then with it
  - the Access apps list (domain, aud, `session_duration` only) shows nodes.culture.dev with 8h and its policy names only `ori.nachum@gmail.com`; the tunnel is listed; dig nodes.culture.dev resolves; no Cloudflare credential appears in any transcript
  - the AUD tag and the tunnel token are captured from the terminal for t5/t6 and the AUD (not the token) is pasted on #273

### t5 — Pin the Access tuple in thor's prod.env and restart the api (hand-turn)

- depends on: t2, t4
- covers: c6, h5
- acceptance:
  - prod.env on thor gains exactly `NODES_ACCESS_LISTEN`=:8081, `NODES_ACCESS_TEAM_DOMAIN`=agentculture.cloudflareaccess.com, `NODES_ACCESS_AUD`=`aud`, written under umask 077; docker compose up -d api recreates the container
  - ss -ltn '( sport = :18081 )' on thor shows 127.0.0.1:18081 only, and curl <http://127.0.0.1:18081/v1alpha1/version> answers with revision 857cb49; both pasted on #273

### t6 — Re-grant `NODES_UI_BASE_URL` to the public origin on both hosts and redeploy (hand-turn)

- depends on: t5
- covers: c7, h6
- acceptance:
  - remove-secret.sh `NODES_UI_BASE_URL` --yes thor orin, then install-secrets.sh with the Jira pair unset (log line: defaulted to the public SSO origin), then deploy.sh thor and deploy.sh orin
  - grep ^`NODES_UI_BASE_URL`= on both hosts prints <https://nodes.culture.dev>; orin's worker docker inspect env agrees; runner-secrets.env on both hosts still lists `JIRA_ACCOUNT_EMAIL`, `JIRA_API_TOKEN`, `GITHUB_TOKEN`, `SONAR_TOKEN`, `NODES_EVENT_TOKEN`

### t7 — Install the Jira service-account trio on thor and orin and re-grant `jira_bot_account_id` (hand-turn)

- depends on: t2, t6
- covers: c8, h7
- acceptance:
  - in one spark shell `JIRA_ACCOUNT_EMAIL`, `JIRA_API_TOKEN` and `JIRA_API_BASE` are exported; the myself check prints 712020:5e0ae915-ba1a-43ef-bce0-c0d5ff9bb615 before any install runs
  - install-secrets.sh's Jira lane rewrites runner-secrets.env on both hosts keeping all five refs and adding `JIRA_API_BASE`; deploy.sh thor's `deploy_jira` merges the three keys into jira-bridge-jira.env while jira-bridge has no session in flight, then jira-bridge is restarted
  - lanes/runner-env-write.sh re-grants `PR_UPKEEP_REPOSITORIES` with `jira_bot_account_id` set on both hosts and nodes-runner is restarted on both; the next pr-upkeep-sweep-cycle run is green (read via nodes run `id`)

### t8 — Prove an operator comment is a human fact under the service account (hand-turn)

- depends on: t7
- covers: c9, h11
- acceptance:
  - a comment posted on SCRUM-8 from the operator's own browser login (author accountId 557058:dbaf9fdd-…), not via the jira skill, is picked up by the next sweep as a human fact rather than self-echo; the comment id and the run id are pasted on #273

### t9 — Place the tunnel token file and enable cloudflared-nodes.service under thor's login user (hand-turn)

- depends on: t3, t4
- acceptance:
  - ~/.config/cloudflared/nodes-culture-dev.token exists on thor with mode 600 under /home/thor, containing only the token; install -D of deploy/prod/cloudflared-nodes.service, daemon-reload, enable --now; loginctl show-user thor -p Linger prints yes
  - systemctl --user is-active cloudflared-nodes prints active, show reports no User= override, and journalctl shows Registered tunnel connection lines; pasted on #273

### t10 — Run the verification pair through the tunnel and on the LAN (hand-turn)

- depends on: t5, t6, t9
- covers: c19, h19
- acceptance:
  - curl -sI <https://nodes.culture.dev/> returns a 302 to agentculture.cloudflareaccess.com, never a 502/1033
  - curl <https://nodes.culture.dev/v1alpha1/version> with the `CF_Authorization` cookie from a browser sign-in and curl <http://192.168.1.146:18080/v1alpha1/version> return identical bodies; both bodies pasted on #273, or the tunnel half is recorded as unmeasured with the reason

### t11 — Add the nodes jira-token verb (mint | verify | install) and docs/operations/jira-service-account.md (repo change, PR)

- covers: c15, h9, c16, h16
- acceptance:
  - `culture_nodes`/cli/`_commands`/`jira_token.py` registers jira-token with subverbs mint (prints the Atlassian admin runbook: service account culture-nodes, where a token is minted or re-minted, the gateway base and why site-URL auth 401s), verify (reads `JIRA_ACCOUNT_EMAIL`/`JIRA_API_TOKEN`/`JIRA_API_BASE` from env, calls /rest/api/3/myself via urllib, prints the accountId and exits 0, exits 2 with a hint otherwise, never prints the token) and install (prints the exact install-secrets/`deploy_jira`/runner-env-write/restart sequence); --json on all three
  - explain/catalog.py has entries for the new paths; tests cover the parser, verify with a stubbed urlopen, and that no output contains the token; uv run pytest -n auto, scripts/lint-all.sh root and uv run teken cli doctor . --strict are green
  - docs/operations/jira-service-account.md names the admin path, the gateway base <https://api.atlassian.com/ex/jira/>`cloud-id`, the accountId 712020:5e0ae915-…, and the install sequence verbatim; git diff main touches only the new module, the catalog, tests, the doc, CHANGELOG.md and pyproject.toml (patch bump)

### t12 — Close the #273 ledger: tick every step with output, add the two missing hand-turns, record the boundaries (hand-turn)

- depends on: t10, t11, t8
- covers: c1, h10, c12, h8, c10, h12, c13, h14, c17, h17, c18, h18
- acceptance:
  - every #273 checklist line is ticked with the date and the pasted output from t1-t11, or left unticked with the reason
  - a comment on #273 adds two checklist lines: the path-scoped Access Bypass policy for /v1alpha1/webhooks/jira and the Jira system webhook registration with the auth mode observed
  - the same comment states that `NODES_HUMAN_DECISION_TOKEN_SECRET` was not removed (break-glass not yet minted), that the t18 second sitting is next and rows a, b, d stay unmeasured until it runs, and quotes the before-state facts from the spec's scope entries; SCRUM-8 gets one status comment via the jira skill

## Risks

- [unknown_nonblocking] Stashing the codex leftovers could hide work that was never harvested: the diffs on thor (t8 files) and orin (t16 files) were not compared against main before stashing; the stash keeps them recoverable (task t1)
- [unknown_nonblocking] `deploy_jira` restarts the jira bridge; a restart with a session in flight kills it (exit 143) and leaves the run running — t10 must check pgrep '\[j\]ira' first (task t7)
- [follow_up] The Jira tenant's webhook auth mode (X-Hub-Signature vs URL token) is unknown until the webhook is registered, which is outside this plan (added to #273 as a further hand-turn)
- [unknown_nonblocking] `deploy_jira` restarts the jira bridge; a restart with a session in flight kills it (exit 143) and leaves the run running — t7 must check pgrep '\[j\]ira' first (task t7)
- [follow_up] The Jira tenant's webhook auth mode (X-Hub-Signature vs URL token) is unknown until the webhook is registered, which is outside this plan (added to #273 as a further hand-turn)
- [unknown_nonblocking] prod-api-1's first JWKS fetch to agentculture.cloudflareaccess.com is proven only at the first browser login after t5; if container egress fails, the tunnel half of the verification is unmeasured and the fix is a follow-up (task t10)
