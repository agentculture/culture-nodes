# nodes.culture.dev — the tunnel, the loopback origin, and the UI base URL

Task t19 of the login-from-anywhere cycle
(`docs/specs/2026-09-01-login-from-anywhere-sso-identity-permissions-jira.md`,
claims c2, c7, c23, c43, c44, c46). This is the operator recipe for putting
the control plane behind Cloudflare Access at `https://nodes.culture.dev`,
and the ledger of what is a hand-turn in it. It changes nothing about how
machines on the LAN reach the API.

**Every step below that a person applies on a host is a hand-turn and is
counted on an issue** — a new one or a comment on the cycle's issue — per
the repo rule in `CLAUDE.md` ("Every piece of operator work opens or updates
an issue"). The list at the end is the checklist to file against.

## Topology

```text
browser ──HTTPS──▶ Cloudflare edge (Access SSO, team agentculture.cloudflareaccess.com)
                        │  tunnel (outbound from thor; token-mode cloudflared user unit)
                        ▼
thor: cloudflared-nodes.service ──HTTP──▶ 127.0.0.1:18081   (NODES_ACCESS_LISTEN, task t8)
                                                    │
                                                    ▼
                                            prod-api-1 (control plane)
                                                    ▲
LAN / tailscale (sweep, workers, bridges) ─HTTP─▶ 0.0.0.0:18080  (unchanged)
```

Two listeners, on purpose (spec c43 / finding s35):

| Listener | Who reaches it | Honours `Cf-Access-Jwt-Assertion`? |
|---|---|---|
| `127.0.0.1:18081` on thor (loopback) | only `cloudflared` on the same host, i.e. traffic that came through Cloudflare Access | yes — this is the only place an Access JWT means anything |
| `0.0.0.0:18080` on thor (LAN, published by `compose.thor.yml` as `18080:8080`) | the sweep, both workers, the bridges, and anyone on the LAN/tailnet | no — a JWT captured on the plaintext LAN cannot be replayed here |

The loopback port is configured by task t8 with
`NODES_ACCESS_LISTEN=127.0.0.1:8081` inside the API container.
`compose.thor.yml` publishes it as `127.0.0.1:18081:8081` (a loopback-only
publish, never `18081:8081`, which would put it on every interface). Set
`NODES_ACCESS_TEAM_DOMAIN` and `NODES_ACCESS_AUD` in the same `prod.env`
change; the server refuses a partial three-variable tuple, while all three
unset leaves the Access feature off and preserves the LAN-only behaviour.

TLS terminates at Cloudflare's edge. Nothing here adds `ListenAndServeTLS`,
certificates or a reverse proxy to `cmd/nodes/serve.go` (spec c26).

## Prerequisites

- thor has `cultureflare` on PATH with an active Cloudflare API token in its
  shell environment (`CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`; finding
  s25). That token is account-wide. **It never enters this repo, `prod.env`,
  a unit file, or a compose file** — it exists only in the operator's shell
  for the length of the provisioning run. The deploy scripts do not read it.
- `cloudflared` is **not** installed on thor today (finding s7). Installing it
  is step 1 and is the reason acceptance criterion 3 of t19 cannot be met
  in-repo.
- The operator's own email is the first Access allow-policy entry. It is a
  placeholder everywhere below (`<operator-email>`); no real address is
  written into the tree.

## Step 1 — install cloudflared on thor (hand-turn)

The unit's `ExecStart` is `/usr/local/bin/cloudflared` — an absolute path,
because systemd does not consult `PATH`. Install the binary there, whichever
way you fetch it:

```bash
# on thor
# either: the release binary for this architecture, placed directly
sudo install -m 0755 ./cloudflared /usr/local/bin/cloudflared
# or: the apt package, which lands in /usr/bin — link it to the unit's path
sudo ln -s /usr/bin/cloudflared /usr/local/bin/cloudflared
/usr/local/bin/cloudflared --version
```

Record the version the unit runs against on the hand-turn issue; a
`--no-autoupdate` unit never moves it on its own.

## Step 2 — provision the tunnel, DNS, Access app and policy (hand-turn, once)

Run from a shell on thor where the Cloudflare API credentials are exported:

```bash
# on thor, in a shell where CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID are exported
cultureflare remote-login setup \
  --hostname nodes.culture.dev \
  --service http://127.0.0.1:18081 \
  --allow <operator-email> \
  --apply
```

One run, idempotent, and it creates four things in the Cloudflare account
(the same shape that fronts `chat.agentculture.org`, finding s1):

1. a named tunnel whose **remote-managed ingress** maps
   `nodes.culture.dev` → `http://127.0.0.1:18081` — the loopback origin
   above. There is no `config.yml` on thor; the ingress lives in Cloudflare
   and the unit only presents the tunnel token;
2. the `nodes.culture.dev` CNAME in the `culture.dev` zone, pointed at the
   tunnel — the first `culture.dev` hostname **with** Access
   (`vllm.culture.dev` is tunnel-only, `--no-access`; finding s6);
3. the Access application for the hostname;
4. an allow-by-email policy listing `<operator-email>`. Additional people
   are added to this policy as the first of the three onboarding places
   spec c46 names (policy, registered human actor revision,
   `actor_identities` binding); the recipe for the other two is task t13's.

`--allow` takes an email only; run it without `--apply` first to see the plan
if you want a dry read.

### Read the Access application's AUD tag

The control plane pins the JWT audience to the Access app's AUD tag (spec
c3), so that value has to reach `prod.env` on thor. The setup run prints the
application it created; the tag is also always readable afterwards in the
Zero Trust dashboard under **Access → Applications → nodes.culture.dev →
Overview → Application Audience (AUD) Tag**, or through the API:

```bash
curl -s -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
  "https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/access/apps" \
  | python3 -c 'import json,sys; [print(a["domain"], a["aud"]) for a in json.load(sys.stdin)["result"]]'
```

The AUD tag is **not a secret** (it is a public identifier of the app) but it
is deployment state: it is written into thor's `prod.env` under the variable
name task t8 gives the audience setting, by hand, and that write is a
hand-turn. The team domain the issuer is pinned to is
`agentculture.cloudflareaccess.com` (spec c2).

### Set the session duration explicitly

cultureflare's `setup` leaves the Access session at Cloudflare's 24 h default
(finding s38). Spec c46 requires the duration to be **set explicitly**, not
inherited. The value is **chosen in task t13 and recorded there**; this
document does not pick it. Once t13 names it, set it on the application
(dashboard: the application's **Session Duration**, or the same
`access/apps/<id>` API object's `session_duration` field) and note the
change on the hand-turn issue. Offboarding — revoking a person's Access
session and retiring their binding — is likewise t13's recipe; this document
only makes sure the session length is a decision rather than a default.

## Step 3 — place the tunnel token file (hand-turn)

The unit reads `TUNNEL_TOKEN_FILE=%h/.config/cloudflared/nodes-culture-dev.token`
(`%h` is the unit user's home on thor). The setup run emits the tunnel's
connector token; put exactly that token, and nothing else, in the file:

```bash
# on thor, as the user that will run the unit
install -d -m 0700 ~/.config/cloudflared
umask 077
printf '%s' '<tunnel token from the setup run>' > ~/.config/cloudflared/nodes-culture-dev.token
chmod 0600 ~/.config/cloudflared/nodes-culture-dev.token
stat -c '%a %n' ~/.config/cloudflared/nodes-culture-dev.token   # 600
```

This per-tunnel token is the **only** Cloudflare credential that lives on
thor's disk. It can run this one tunnel and nothing else — which is why the
account-wide API token can stay in the shell that ran step 2 (finding s8).
The token file is never committed, never copied into `prod.env`, and never
read by any deploy script; `audit-credentials.sh` does not see it because
nothing in compose consumes it.

## Step 4 — install and enable the unit (hand-turn)

```bash
# on thor, from a checkout of this repo
install -D -m 0644 deploy/prod/cloudflared-nodes.service \
  ~/.config/systemd/user/cloudflared-nodes.service
systemctl --user daemon-reload
systemctl --user enable --now cloudflared-nodes
loginctl enable-linger "$USER"          # so the user unit survives logout / reboot
systemctl --user status cloudflared-nodes
journalctl --user -u cloudflared-nodes -n 50
```

`loginctl enable-linger` is the step people forget: without it every `--user`
unit on thor stops the moment the last session closes, and the tunnel goes
with it. It is already set for the accounts that run the other user units on
thor (`nodes-runner`, `codex-bridge`, the bridges), so if the tunnel runs as
one of those it is a no-op — check with `loginctl show-user "$USER" -p Linger`.

The unit is **not** installed by `deploy/prod/deploy.sh` today. That is
deliberate for this cycle: the deploy lanes install things whose inputs the
scripts hold, and this unit's only input is a token the scripts must never
hold. Folding it into a deploy lane is a follow-up; until then, installing it
is one of the hand-turns below.

## Step 5 — the UI base URL is the public name now (repo change + one hand-turn)

`NODES_UI_BASE_URL` is the origin the engine puts in front of every ticket
page link it posts to Jira, every human-task fan-out option, and every
Discord notification (spec c44, finding s36). Task t19 changes it to
`https://nodes.culture.dev` **everywhere it is written**:

- `deploy/prod/compose.thor.yml` (api, scheduler, worker) and
  `deploy/prod/compose.orin.yml` (worker) default it to
  `${NODES_UI_BASE_URL:-https://nodes.culture.dev}` — the same value on both
  hosts, because whichever worker mints a run renders the link;
- `deploy/prod/install-secrets.sh` writes `https://nodes.culture.dev` into
  both hosts' `prod.env` unless an operator exports another value;
- `tests/deploy/nodesculturedev_test.go` pins that both compose files agree
  on the value and that the LAN publish `18080:8080` is unchanged.

**The hand-turn:** `install-secrets.sh` is add-if-absent. A host that
already carries the LAN value from the presentable-floor cycle keeps it
until the key is removed and the script re-run:

```bash
# from spark
deploy/prod/remove-secret.sh NODES_UI_BASE_URL --yes thor
deploy/prod/remove-secret.sh NODES_UI_BASE_URL --yes orin
deploy/prod/install-secrets.sh          # logs "defaulted to the public SSO origin"
```

then redeploy so the containers pick up the new env. Verify on both hosts:

```bash
ssh thor 'grep ^NODES_UI_BASE_URL= ~/.culture-nodes/prod.env'
ssh orin 'grep ^NODES_UI_BASE_URL= ~/.culture-nodes/prod.env'
# both: NODES_UI_BASE_URL=https://nodes.culture.dev
```

The machine-facing addresses do **not** change: `NODES_API_URL` for the sweep
and `NODES_CALLBACK_BASE_URL` for the bridges stay LAN IPs / `http://thor:18080`
(spec c26). Only the link a *person* follows moves to the public name.

## Verification

The acceptance check for this task, which **cannot run in-repo** because it
needs cloudflared installed on thor and the t8 listener bound:

```bash
# the same revision through the tunnel and on the LAN
curl -s https://nodes.culture.dev/v1alpha1/version
curl -s http://192.168.1.146:18080/v1alpha1/version
# the two bodies must be identical
```

`/v1alpha1/version` is unauthenticated on the LAN; through the tunnel an
unauthenticated `curl` is redirected by Access (`302` to
`agentculture.cloudflareaccess.com`) unless a Bypass policy covers the path,
so for the tunnel half either sign in once in a browser and pass the
`CF_Authorization` cookie, or use an Access service token
(`CF-Access-Client-Id` / `CF-Access-Client-Secret` headers) mapped to a
registered actor (spec c9). What matters is that both halves report the same
`revision`. Also worth reading while you are there:

```bash
curl -sI https://nodes.culture.dev/ | head -1                 # 302 to the Access login, never a Cloudflare 1033/502
systemctl --user is-active cloudflared-nodes                  # active
journalctl --user -u cloudflared-nodes -n 5                   # "Registered tunnel connection" lines
ss -ltn '( sport = :18081 )'                                  # 127.0.0.1:18081 only, never 0.0.0.0
```

The last line is the c43 property in one command: if `18081` is ever bound
on anything but loopback, the JWT-replay split is gone.

## Jira system webhook and path-scoped Access Bypass

Task t16 claims spec c42 and c51 and satisfies honesty conditions h14 and
h37: the push is explicitly only a wake-up, and the five-minute reconciler is
not removed before sustained production evidence exists.

Create a second Cloudflare Access policy on the existing application with
**Action: Bypass**, **Include: Everyone**, and restrict it to the path
`/v1alpha1/webhooks/jira`. Do not widen the path: the API mounts this receiver
only on `NODES_ACCESS_LISTEN`; the LAN `NODES_LISTEN` listener answers 404.
The receiver independently authenticates every delivery with either Jira's
`X-Hub-Signature` HMAC and `NODES_JIRA_WEBHOOK_SECRET`, or the URL token and
`NODES_JIRA_WEBHOOK_TOKEN`. With neither configured it stays closed (401).

In Jira open **Settings → System → WebHooks**, register:

- URL: `https://nodes.culture.dev/v1alpha1/webhooks/jira?token=...`
- events: issue created, updated, and deleted; comment created and updated
- JQL: `project = SCRUM`
- secret: `NODES_JIRA_WEBHOOK_SECRET`, if this Jira tenant offers webhook
  secrets (the signature takes precedence over the URL token).

This is a wake-up, not an event authority: the control plane re-reads the
issue changelog and comments with `JIRA_ACCOUNT_EMAIL` and `JIRA_API_TOKEN`
(`JIRA_API_BASE` overrides `https://<JIRA_SITE>`), then replays the same facts
and cumulative watermarks as the PR-upkeep sweep. Decision c51 remains: use no
Jira Automation rules. Keep the sweep at five minutes as reconciliation until
one full week of push runs is green; only then make a separate scheduling
decision. Applying the Bypass policy and registering the Jira webhook are
operator hand-turns and must be recorded on the cycle issue.

## Hand-turn ledger for this document

Each of these is a manual operator step and gets an issue, or a comment on
the cycle's issue, when it is applied:

1. Install `cloudflared` at `/usr/local/bin/cloudflared` on thor (step 1) and
   record the version.
2. Run `cultureflare remote-login setup … --apply` from thor with the API
   credentials exported (step 2) — creates tunnel, CNAME, Access app,
   policy.
3. Read the Access app's AUD tag and write it into thor's `prod.env` under
   t8's variable name (step 2).
4. Set the Access session duration to the value t13 records (step 2).
5. Write the tunnel token to `~/.config/cloudflared/nodes-culture-dev.token`,
   mode `0600` (step 3).
6. Install `cloudflared-nodes.service` into `~/.config/systemd/user/`,
   `daemon-reload`, `enable --now`, and `loginctl enable-linger` (step 4).
7. `remove-secret.sh NODES_UI_BASE_URL` on thor and orin, re-run
   `install-secrets.sh`, redeploy (step 5).
8. Run the verification pair of `curl`s and paste both bodies on the issue.
9. Add the path-scoped Access Bypass policy for the Jira receiver.
10. Register and test the Jira system webhook and record which authentication
    mode the tenant supplies.
11. `remove-secret.sh NODES_HUMAN_DECISION_TOKEN_SECRET` on thor (h25) —
    **sequenced after** the c48 break-glass credential exists. That
    credential is minted by task t18's operator run of break-glass step 1 in
    `docs/operations/people.md`; until it is on thor and `whoami` answers
    with `provider: nodes-inbound-credential`, the shared decision secret is
    the only LAN break-glass there is, and removing it first would lock every
    human out of a misconfigured Access policy — exactly what c48 forbids.
    Record both runs, in that order, on the cycle issue.

Adding a person later is a further hand-turn on the Access policy (one of
c46's three places) and is counted the same way.
