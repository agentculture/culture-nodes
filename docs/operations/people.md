# People: onboarding, offboarding, session policy, break-glass

This is the operator recipe for letting a **person** decide human tasks on
`https://nodes.culture.dev` from anywhere, and for taking that ability away
again. It is task t13 of the login-from-anywhere cycle
(`docs/specs/2026-09-01-login-from-anywhere-sso-identity-permissions-jira.md`)
and covers spec claims c26, c37, c38, c46 and c48 and honesty conditions
h25, h32 and h34. Its break-glass section was amended by task t22, which
built the control-plane half c48 needs (deviation d2) and turned this
document's recorded gap into a recipe. It assumes the tunnel, Access
application and first allow policy from
`docs/operations/nodes-culture-dev.md` (task t19) already exist and that the
control plane runs with task t8's Access listener enabled
(`NODES_ACCESS_LISTEN`, `NODES_ACCESS_TEAM_DOMAIN`, `NODES_ACCESS_AUD` set
in thor's `prod.env`).

Every step marked **hand-turn** is a manual operator action and gets an
issue, or a comment on the cycle's issue, when it is applied (CLAUDE.md,
"every piece of operator work opens or updates an issue"). The ledger at
the end of this document lists them so they can be counted.

## The three places, and what each one decides

A person is admitted by three independent records, in three different
systems. All three must exist for a decision to land; each one missing
produces a different, deliberately visible refusal (spec c46):

| Place | System | What it controls | What the person sees if it is missing |
|---|---|---|---|
| Access allow policy | Cloudflare Zero Trust (the Access app on `nodes.culture.dev`) | whether the person can reach the site at all | the Cloudflare Access login refuses them; the control plane never sees a request |
| Registered human actor revision | the control plane's `actors` table (`kind=human`) | the identity that appears as decider on ledger records | `bind-identity.sh` refuses to bind: "no actor registered under key" |
| Identity binding | the control plane's `actor_identities` table (migration 0053) | which actor the SSO principal is, and its roles | the **unbound page**: "no actor is bound to this login", naming the principal; every write is refused `403` with reason class `unbound` (h32) |

The binding is keyed by `(provider, subject)`, **never by email** (spec
c37): bot and service identities may carry fake emails, and Cloudflare's
`sub` claim is the stable user id that survives an email change. The
`email` in the JWT is shown on the unbound page and on `/v1alpha1/whoami`
for a human to recognise the person, and stored nowhere.

Roles are a closed vocabulary (spec c38, `internal/auth/roles.go`):

| Role | May |
|---|---|
| `viewer` | read everything (every `GET` is open to a verified principal) |
| `approver` | decide human tasks, review, reply on tickets, grade |
| `namespace_administrator` | everything `approver` may, plus register actors, publish, cancel, schedules, credentials |

A principal that passes Access with a binding but the wrong role is refused
`403` with reason class `forbidden_role`; a principal with no binding at all
is `unbound`. Both are logged with the subject and counted on the
`api.auth_refusals.count` telemetry counter (spec c48, task t8) — the JWT itself appears in no
log.

## Onboarding a person

The recipe is: **operator adds the email to the policy → operator registers
the human actor → person visits once and appears unbound → operator binds the
subject the site showed → person reloads and decides.**

### 1. Add the email to the Access allow policy (hand-turn)

cultureflare creates the policy at provisioning time; `--allow` is
repeatable, so the people known at setup go in there:

```bash
# from thor, CLOUDFLARE_API_TOKEN / CLOUDFLARE_ACCOUNT_ID in the shell only
cultureflare remote-login setup --hostname nodes.culture.dev \
  --service http://127.0.0.1:18081 \
  --allow <operator-email> --allow <person-email> \
  --session-duration 8h --apply
```

Adding a person to an **existing** app is different: cultureflare's
`remote-login setup` is find-or-create — it locates the app and the policy by
name and leaves both untouched (`_remote_login/_access_policy.py`,
`ensure_allow_policy`), so re-running it with a new `--allow` changes
nothing. Either path below works; both are hand-turns:

- **Dashboard**: Zero Trust → **Access → Applications → nodes.culture.dev →
  Policies** → the allow policy → **Include → Emails** → add the address →
  Save.
- **API**: read the policy, add the email, write the whole policy back
  (Cloudflare's `PUT` replaces the object, so send every field you read):

  ```bash
  cultureflare remote-login show --hostname nodes.culture.dev --json   # app id, policy id
  curl -s -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
    "https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/access/apps/$APP_ID/policies/$POLICY_ID" \
    | python3 -c 'import json,sys; p=json.load(sys.stdin)["result"]; p["include"].append({"email":{"email":"<person-email>"}}); json.dump({k:p[k] for k in ("name","decision","include","exclude","require") if k in p}, sys.stdout)' \
    | curl -s -X PUT -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H 'Content-Type: application/json' --data @- \
    "https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/access/apps/$APP_ID/policies/$POLICY_ID"
  ```

The account-wide API token stays in the operator's shell for the length of
the edit and enters no file in this repo, `prod.env`, or a unit (the same
rule `nodes-culture-dev.md` sets for provisioning).

### 2. Register a human actor revision

Actor keys are lowercase paths; people follow the same `company/<handle>`
convention as the bridges (`company/codex-thor`, `company/human-ops`):

```bash
# from a host that can reach thor's Postgres (deploy/prod's compose-exec psql
# is the default PSQL_CMD; override PSQL_CMD to point elsewhere)
deploy/prod/register-actor.sh --human company/alice
# register-actor: registered company/alice at revision 1 ()
```

`--human` inserts a `kind=human`, `protocol=none`, endpoint-less row and is
idempotent like every other mode of the script; a person is reached through
the page, never dispatched to, so the IPv4-endpoint rule for agents does not
apply. Actor rows are append-only, so a later `--metadata` change appends
revision 2 rather than editing revision 1. The display name a person is
known by on ticket pages is the actor key; the login email is not stored
here (see c37 above).

### 3. Bind the identity to the actor

The subject to bind is the **`sub` claim of the Access JWT** — Cloudflare's
user id for the person, a UUID-shaped string. Nobody has to decode a token
to read it: on first visit the person is a verified-but-unbound principal,
and the control plane shows them their own subject.

1. The person opens `https://nodes.culture.dev` (or the ticket link from
   Jira / Discord), signs in through Access, and lands on the **unbound
   page** — "no actor is bound to this login" with the provider, subject and
   email. They send the operator the subject (or a screenshot).
2. The operator can read the same value with `GET /v1alpha1/whoami`. Reads
   are open to any verified principal, bound or not; `whoami` is the route
   that reports the binding state. Use the person's browser session cookie
   or their own:

   ```bash
   curl -s -b "CF_Authorization=<cookie>" https://nodes.culture.dev/v1alpha1/whoami
   # {"principal":{"provider":"cloudflare-access","subject":"<sub>","email":"<person-email>"},"unbound":true}
   ```

   A third place to read it: Zero Trust → **My Team → Users** → the person
   → the user id. It is the same value.
3. Bind it:

   ```bash
   scripts/bind-identity.sh bind --provider cloudflare-access --subject <sub> \
     --actor-key company/alice --roles approver
   # bind-identity: bound cloudflare-access/<sub> to company/alice (actor <actor-id>, identity <identity-id>, roles approver)
   ```

   Keep the printed `identity-id`; offboarding revokes by it (`list` finds
   it again). The script validates the provider, the roles and the key
   before it touches Postgres, points the binding at the key's newest
   actor revision, and refuses a second live binding for the same subject
   (revoke the old one first). It warns when the subject looks like an
   email address, because pasting the login email instead of the `sub`
   claim is the likeliest mistake here.
4. The person reloads. `whoami` now carries `actor_id` and `roles`, the
   ticket page offers its decision controls, and a decision from off-LAN
   lands as a `decision` record whose origin is `company/alice`.

A **service token** (a CI job, a machine) follows the same three places with
`--provider cloudflare-service-token --subject <token common name>`; the
subject is the `common_name` claim Access puts in the JWT it mints for the
token (`internal/auth/verifier.go`, `BindingKey`). Its Access policy is a
separate `Service Auth` policy, which cultureflare's `--with-service-token`
creates.

## Offboarding a person

Do the three places in this order. The binding first, because it is the
one step that takes effect on the very next request and does not depend on
Cloudflare's state; the session second, because the policy edit alone
leaves an already-issued session valid until it expires.

1. **Retire the binding** (control plane, immediate):

   ```bash
   scripts/bind-identity.sh list --actor-key company/alice      # find the identity id
   scripts/bind-identity.sh revoke --identity <identity-id>
   ```

   The row is kept with `revoked_at` set — history, not deletion — and the
   person becomes an unbound principal: every write `403 unbound` from the
   next request on. The human actor row stays; ledger records that name it
   as decider must keep resolving.
2. **Revoke the Access session** (hand-turn): Zero Trust → **My Team →
   Users** → the person → **Revoke** (ends their sessions for every app),
   or, to end everyone's sessions on this app at once, **Access →
   Applications → nodes.culture.dev → … → Revoke existing tokens**.
3. **Remove the email from the allow policy** (hand-turn): the same
   dashboard page or the same GET-edit-PUT as onboarding step 1, dropping
   the `{"email":{"email":…}}` entry.

If step 2 is skipped, step 3 still holds — but only after the session
expires, which is where the session duration below comes in.

## Session policy: 8 hours, set explicitly

The Access application's session duration is **8h**. Cloudflare's default
is 24h and cultureflare inherits it (`--session-duration` defaults to
`24h`); spec c46 requires the value to be chosen, not inherited.

Why 8h: it is a working day. A person signs in once in the morning and is
not re-prompted mid-task, and any revocation done only at the policy layer
(offboarding step 3 without step 2, or a policy mistake) is stale for at
most one working day rather than until tomorrow. Shorter would re-prompt
inside a single sitting for no security gain the session-revoke step does
not already give; longer stretches "removed from the policy" past the day
the operator did it.

How to set it (hand-turn; `nodes-culture-dev.md` step 2 defers to this
value):

- **At provisioning**: `cultureflare remote-login setup … --session-duration
  8h --apply` (the command in onboarding step 1). Only a *created* app takes
  the flag; `ensure_app` leaves an existing app as it is.
- **On the existing app**, dashboard: Zero Trust → **Access → Applications →
  nodes.culture.dev → Configure → Session Duration → 8 hours** → Save.
- **On the existing app**, API — read, change one field, write back:

  ```bash
  curl -s -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
    "https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/access/apps/$APP_ID" \
    | python3 -c 'import json,sys; a=json.load(sys.stdin)["result"]; a["session_duration"]="8h"; json.dump({k:v for k,v in a.items() if k not in ("id","aud","created_at","updated_at")}, sys.stdout)' \
    | curl -s -X PUT -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" -H 'Content-Type: application/json' --data @- \
    "https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/access/apps/$APP_ID"
  ```

Verify by reading the app back (`GET …/access/apps/$APP_ID` →
`"session_duration": "8h"`) and note the change on the hand-turn issue.

## Break-glass: an operator on the LAN when Access is misconfigured

Spec c48 wants a path that a wrong allow policy, a wrong AUD, or a dead
tunnel cannot close: **an issued service credential bound to the operator's
own human actor**, usable from `curl` on the LAN to decide a human task.

Task t13 wrote this section as a gap: the credential could be minted but the
control plane did not honour it, so the only real LAN break-glass was the
shared decision secret. Task t22 closed it (`internal/api/breakglass.go`).
What follows is the recipe, then the older path it replaces.

### The credential, and what it grants

It is an ordinary **issued dial-in credential** (`cnd_…`, `crypto/rand`,
digest at rest, revealed once, `RequireControlPlaneIssued`) — the same class
every bridge dials in with. Nothing about issuance was widened for this: a
credential is issued for a *party*, a party is any `namespace/name` key
(`internal/actors.ValidateInboundParty`), and a person's registered human
actor is one.

What is new is where it is **verified**. On the LAN listener (`Handler`),
and only there, a `Bearer cnd_…` on a route the principal gate protects is
resolved through the dial-in path's own
`internal/actors.InboundAuthenticator.Authenticate` — same store, same
per-credential lock, same order: revoked → control-plane-issued → lockout →
rate window → constant-time verifier. It is the same door, not a second one.

An admitted credential binds to **that party's newest registered actor
revision**:

| The party's actor kind | The principal it becomes | May |
|---|---|---|
| `human` | non-synthetic, subject = the party key, role `approver` | decide human tasks, review, reply, grade — what c48's break-glass is for |
| anything else (`agent`, …) | the machine principal `actorbearer.go` already defines, role `viewer` | write only where agents may; a human decision under it is refused `403 forbidden_role` |
| no registered actor | bound to nothing | every write refused `403 unbound`, the same visible refusal an unbound person gets |

It is deliberately **not** `namespace_administrator`. An operator locked out
of Access can decide the human task that is blocking the lane; they cannot
register actors or publish workflows with it.

The **loopback Access listener never honours it** — that listener exists so
an Access JWT is accepted only where the tunnel can reach (c43), and a second
credential class there would widen exactly the surface that split keeps
narrow.

### 1. Mint it (hand-turn, once, and on every rotation)

```bash
DIALIN_PREFIX=BREAK_GLASS DIALIN_HOST=thor \
DIALIN_DESTINATION=env:.culture-nodes/dialin/break-glass.env \
  deploy/prod/issue-dialin-credential.sh company/<operator-handle> thor
```

`company/<operator-handle>` is the **human actor registered in step 2 of
onboarding**, not a bridge. The party is not added to `dialin_bridges()` —
a person is not a bridge this deployment runs, and the three one-off
overrides above are the whole mechanism.

The lane's guarantees are unchanged for a person's credential: the plaintext
lands in exactly one mode-0600 file on thor
(`~/.culture-nodes/dialin/break-glass.env`, under the operator's own
account), the control plane keeps only its SHA-256 digest, the command
prints the digest and never the value, and `audit-credentials.sh` fails if a
copy ever reaches `prod.env`.

### 2. Prove it works before you need it

`whoami` resolves the credential without deciding anything, so the check
costs no ledger record and can be repeated after any rotation:

```bash
# on thor, on the LAN; the credential never enters an argv
. ~/.culture-nodes/dialin/break-glass.env
cat <<CURLRC | curl -fsS -K -
url = "http://127.0.0.1:18080/v1alpha1/whoami"
header = "Authorization: Bearer $BREAK_GLASS_DIAL_TOKEN"
CURLRC
# {"principal":{"provider":"nodes-inbound-credential","subject":"company/<handle>"},
#  "actor_id":"01J…","roles":["approver"]}
```

`provider` is the tell: `nodes-inbound-credential` is this credential,
`cloudflare-access` is a signed-in person, `transition` is the old shared
secret below.

### 3. Decide with it

```bash
# on thor, on the LAN
. ~/.culture-nodes/dialin/break-glass.env
cat <<CURLRC | curl -fsS -K -
url = "http://127.0.0.1:18080/v1alpha1/human-tasks/<task-id>/decision"
request = "POST"
header = "Authorization: Bearer $BREAK_GLASS_DIAL_TOKEN"
header = "Content-Type: application/json"
data = "{\"outcome\":\"approved\",\"expected_ledger_version\":<n>}"
CURLRC
```

**No `decider_actor_id` is typed.** That is the whole difference from the
shared secret: the origin on the ledger record is the actor the *credential*
is bound to, stamped server-side, not an actor the body claimed.

### 4. Retire it

`deploy/prod/issue-dialin-credential.sh --revoke company/<operator-handle>`
(with the same three overrides) ends authority at the control plane and
removes the only plaintext. A revoked credential then refuses with reason
class **`credential_revoked`** — logged with the party key and counted on
`api.auth_refusals.count`, never as `no_principal`, because "revoked" and
"there is no such credential" are different facts to an operator reading a
log during an incident. Its siblings are `credential_locked`,
`credential_rate_limited`, `credential_not_issued` and `credential_invalid`.
A `cnd_` value no record matches is still an unknown bearer: `401
no_principal`, unchanged.

Re-minting is how a rotation works, and it clears revocation and lockout —
issuing a new secret *is* the act of granting authority again.

### The older path this replaces: the shared decision secret

The LAN listener ignores `Cf-Access-Jwt-Assertion` entirely, so nothing about
Access can affect it. Before the credential above, a decision there was
authenticated by the **decision transition bearer**,
`NODES_HUMAN_DECISION_TOKEN_SECRET`, read on the host out of thor's
`prod.env`:

```bash
# on thor, on the LAN; the secret never leaves the host or enters an argv
sec=$(sed -n 's/^NODES_HUMAN_DECISION_TOKEN_SECRET=//p' ~/.culture-nodes/prod.env)
cat <<CURLRC | curl -fsS -K -
url = "http://127.0.0.1:18080/v1alpha1/human-tasks/<task-id>/decision"
request = "POST"
header = "Authorization: Bearer $sec"
header = "Content-Type: application/json"
data = "{\"outcome\":\"approved\",\"decider_actor_id\":\"<operator actor id>\",\"expected_ledger_version\":<n>}"
CURLRC
```

What that path is, honestly: the principal is the synthetic
`transition-bearer` (provider `transition`, role `namespace_administrator`),
and the operator's actor appears on the ledger only because the body's
`decider_actor_id` **claims** it. The credential does not identify a person;
it identifies whoever could read `prod.env`. Any use of it is recorded on the
cycle's issue with the task id and the reason Access was unusable (h34's
"its use is recorded") — and so is any use of the c48 credential above.

**Sequencing constraint for h25.** h25 has the operator stop holding
`NODES_HUMAN_DECISION_TOKEN_SECRET` (`remove-secret.sh` run recorded).
Removing it from `prod.env` before the c48 credential exists removes the only
LAN break-glass there is — a misconfigured policy would then lock every human
out, which is exactly what c48 forbids. Run that `remove-secret.sh` **after**
step 1 above has been applied on thor and step 2 has answered, not before,
and say so on the measurement sitting. The same ordering is item 11 of
`docs/operations/nodes-culture-dev.md`'s hand-turn ledger.

## Hand-turn ledger for this document

Each of these is a manual operator step and gets an issue, or a comment on
the cycle's issue, when it is applied:

1. Add a person's email to the Access allow policy (onboarding step 1;
   dashboard or API), per person.
2. Read the person's subject from the unbound page / `whoami` and bind it
   (onboarding step 3) — the script run is automated, the copy from the
   person to the operator is not.
3. Set the Access session duration to 8h on the application (session
   policy; also item 4 of `nodes-culture-dev.md`'s ledger).
4. Revoke a departing person's Access session (offboarding step 2).
5. Remove a departing person's email from the allow policy (offboarding
   step 3).
6. Any use of a LAN break-glass credential — the c48 one or the older
   shared decision bearer — with the task id and the reason Access was
   unusable.
7. Minting the c48 break-glass credential onto thor (break-glass step 1),
   and re-minting it on every rotation.
8. Running `remove-secret.sh NODES_HUMAN_DECISION_TOKEN_SECRET` — only
   after item 7 has been applied and break-glass step 2 has answered.

Registering the human actor and binding / revoking the identity are script
runs against the control plane and are not counted as hand-turns; they are
the automated two of the three places.

## Measured proof (filled in by task t18)

*To be recorded in the measurement sitting (h25): a second person — not the
operator — was added through the three places above, opened a ticket link
from off-LAN, and decided a human task; the resulting `decision` record's
origin actor, the `whoami` output before and after binding, the `403
unbound` observed before the bind, and the Access app's `session_duration`
as read back from the API.*

| Item | Value |
|---|---|
| Second person's actor key | *pending t18* |
| Decision record id, off-LAN | *pending t18* |
| `403 unbound` observed before bind | *pending t18* |
| `session_duration` read back | *pending t18* |
| Break-glass bearer used during the sitting | *pending t18* |
