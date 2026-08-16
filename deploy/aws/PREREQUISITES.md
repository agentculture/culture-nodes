# AWS prerequisites — what we need, and how to clear it

**Audience:** whoever holds admin on the AWS account. You do not need to know
anything about culture-nodes to work through this page.

**Time:** about 15 minutes, most of it waiting for AWS.

**What you are enabling:** the optional RDS database target for culture-nodes.
The base deployment does not select RDS and the base operator policy does not
grant it. If issue #112 selects RDS, the application can stay where it is while
its authoritative PostgreSQL moves to managed RDS.

---

## TL;DR

```bash
# 1. See exactly what is missing (read-only, safe, repeatable)
./deploy/aws/preflight.py --db-target rds                        # defaults to us-east-2
./deploy/aws/preflight.py --db-target rds --region eu-west-1     # chosen region

# 2. As an ACCOUNT ADMIN, clear whatever it reported. On a default region
#    this is usually just the policy; an opt-in region needs the first line too:
./deploy/aws/bootstrap-operator.sh enable-region <region>   # opt-in regions only
./deploy/aws/bootstrap-operator.sh enable-rds

# 3. Confirm
./deploy/aws/preflight.py --db-target rds     # exits 0 when everything is ready
```

`preflight.py` prints, for every failing check, **why** it exists, **who** may
fix it, and the **exact command**. If you read nothing else on this page, run
it — it is the authoritative version of this document.

---

## Choosing a region, then clearing the two blockers

### 0. Which region?

**The default is `us-east-2`, and that is a deliberate choice about *other
people*, not about us.** A stranger cloning this repository and running
`preflight.py` should land somewhere that is enabled on every account, sits in
the cheapest pricing tier, and has the longest operational track record.
`us-east-2` is all three:

| | `us-east-2` | `il-central-1` |
|---|---|---|
| Opt-in required | no | **yes** — every API call fails until enabled |
| `db.t4g.micro` PostgreSQL, Single-AZ | **$0.016/hr** ≈ $11.68/mo | $0.018/hr ≈ $13.14/mo |
| Service coverage | broadest | thinner, newer region |
| Track record | the region people pick for `us-east-1`'s ecosystem without its outage history | launched 2023 |
| TCP connect from `thor` | 161 ms | **13 ms** |

Prices are from AWS's public price list, not estimates.

**Where your database goes is a deployment input.** Pass `--region` and the
whole script follows. A control plane far from its database pays in round
trips — every claim, every one-second poll, every transition, every UI read —
and `preflight.py` reports that as a **warning, never a blocker**, because at
a 60-second lease with a 20-second heartbeat, correctness is not at stake even
at 161 ms. What you lose is transitions landing in about a second instead of
about a tenth of one.

Choose on what you actually weigh. If cost, familiarity and maturity matter
more than response time, `us-east-2` is the better answer, and that is why it
is the default.

### 1. Is your chosen region enabled on the account?

Regions launched after 2019 are opt-in — `il-central-1` is one, `us-east-2` is
not. If you picked a default region, skip to blocker 2.

AWS regions launched after 2019 are **opt-in**: they exist, but your account
cannot use them until you say so. Here is what that looks like, measured
against a region this account had not enabled:

```console
$ aws sts get-caller-identity --region il-central-1
An error occurred (InvalidClientTokenId) when calling the GetCallerIdentity
operation: The security token included in the request is invalid.
```

**Read that error carefully, because it is misleading.** It says the
credential is invalid. The credential is fine. An opt-in region answers
*every* API call that way until the account enables it. Anyone debugging this
from the error alone will spend an hour rotating a key that was never broken.

There is a **second** propagation behind the first: after the opt-status flips
to `ENABLED`, credentials stay invalid in the new region for a few more
minutes. `preflight.py` reports both as `waiting` — not as something you broke.

**Fix it** (only if you chose an opt-in region):

```bash
./deploy/aws/bootstrap-operator.sh enable-region <region>
```

The script checks the current status first and does nothing if the region is
already on or already enabling. AWS takes a few minutes to propagate; re-run
`./deploy/aws/preflight.py` until it reports `ok`.

> **Why can't the automation do this itself?** It is an account-wide setting.
> A credential that can turn regions on for the whole account is not a scoped
> credential. The automation gets the *read* (`account:GetRegionOptStatus`) so
> it can report the status, and nothing more.

### 2. The base operator policy grants no RDS at all

The scoped IAM user's base policy covers SQS, S3, ECR, Lambda, scoped IAM and
STS. RDS is deliberately separated into an opt-in overlay:

```console
$ aws rds describe-orderable-db-instance-options --engine postgres
An error occurred (AccessDenied) ... User: arn:aws:iam::435593604218:user/
culture-nodes-dev is not authorized to perform:
rds:DescribeOrderableDBInstanceOptions because no identity-based policy allows
the rds:DescribeOrderableDBInstanceOptions action
```

**Fix it:**

```bash
./deploy/aws/bootstrap-operator.sh enable-rds
```

That creates (if needed) and attaches the policy recorded in
`deploy/aws/rds-optional-policy.json`. Remove it with
`./deploy/aws/bootstrap-operator.sh disable-rds`. Both policy files are
committed, so the base grant and optional widening are separately reviewable.

---

## What the new permissions allow, and what fences them

Everything the project creates is named `culture-nodes-*`, and the policy
fences on that prefix wherever AWS supports resource-level permissions.

| Statement | Grants | Fenced to |
|---|---|---|
| `RdsProjectInstances` | create / modify / delete / snapshot / restore the database, its subnet group and its parameter group | `arn:aws:rds:*:*:{db,snapshot,subgrp,pg}:culture-nodes-*` |
| `RdsDescribesAreUnscoped` | read-only describes | `*` — **see the note below** |
| `VpcReadsForDatabasePlacement` | read VPCs, subnets, security groups, AZs | `*` — **see the note below** |
| `DatabaseSecurityGroupCreate` | create a security group | `*` (AWS requires it at creation time) |
| `TagOnlyAtSecurityGroupCreation` | tag a security group | only during `CreateSecurityGroup` |
| `MutateOnlyProjectTaggedSecurityGroups` | open / close / delete a security group | only groups tagged `project=culture-nodes` |
| `ReadRegionOptStatusOnly` | read whether a region is enabled | `*` (read-only; **not** `account:EnableRegion`) |

> **The two unscoped statements are unscoped because AWS gives no choice.**
> RDS `Describe*` and EC2 `Describe*` actions do not support resource-level
> permissions — a policy that names an ARN for them simply denies everything.
> Both are read-only, and they are split into their own statements with
> self-describing names precisely so a reviewer sees the widening rather than
> finding it buried inside a statement that looks fenced. This follows the
> convention already in the file (`EcrLoginIsUnscoped`, `AccountLevelListings`).

Note what is **not** granted: no `rds:DeleteDBCluster`, no permission over
databases that are not named `culture-nodes-*`, no `account:EnableRegion`, and
no ability for the automation to widen its own policy.

---

## Who runs what

| Step | Identity | Command | Mutates? |
|---|---|---|---|
| Check status | operator (`AWS_PROFILE=culture-nodes`) or admin | `./deploy/aws/preflight.py` | no |
| Enable the region (opt-in regions only) | **account admin** | `./deploy/aws/bootstrap-operator.sh enable-region <region>` | account setting |
| Grant RDS | **account admin** | `./deploy/aws/bootstrap-operator.sh enable-rds` | IAM policy attachment |
| Confirm | operator | `./deploy/aws/preflight.py --db-target rds` | no |

Per standing policy, agent sessions never run `bootstrap-operator.sh` and
never handle key material. They run `preflight.py`, read its report, and tell
a human which of the two commands above to run.

---

## Reading the preflight report

```text
culture-nodes AWS preflight — target region us-east-2
================================================================

  [  ok   ]  AWS CLI installed
              aws-cli/2.x.y at /usr/local/bin/aws

  [BLOCKED]  Region eu-south-1 enabled
              sts in eu-south-1 returned InvalidClientTokenId — the region is not enabled
              why:  eu-south-1 is an opt-in region: until the account enables it, EVERY
                    API call there fails with InvalidClientTokenId, which looks like a
                    broken credential and is not one
              who:  account admin — an account-level setting, ~10 minutes to take effect
              fix:  ./deploy/aws/bootstrap-operator.sh enable-region eu-south-1
```

| Badge | Meaning |
|---|---|
| `ok` | ready |
| `BLOCKED` | must be cleared before provisioning; the report names who and how |
| `waiting` | requested and in flight at AWS — nobody has anything to do |
| `warn` | proceed with attention — e.g. latency over the 30 ms guideline |
| `skip` | not testable yet because an earlier check is blocked |

Exit codes: `0` ready · `1` at least one blocker · `2` this machine cannot run
the checks at all (no CLI, no credentials).

`--json` emits the same result as a structured payload, so CI or another
script can consume it.

---

## After the prerequisites are clear

The database location is a **deployment input**, not a product decision. The
control plane reads one variable, `NODES_DATABASE_URL`, and does not care what
answers it — RDS, Cloud SQL, Neon, Supabase, a shared cluster, or the bundled
container. The Helm chart already exposes this
(`postgresql.enabled: false` plus `postgresql.external.url`), and a remote
database is already exercised in production: `orin`'s worker reaches `thor`'s
Postgres through exactly that variable.

So the RDS cutover is executed as **the first non-bundled deployment**, using
the same switch a stranger would use to point at their own database. If the
migration ever needs a code path a third-party install would not have, the
portability requirement has been violated — and the diff will show it.

Two gates stand between "prerequisites clear" and "production points at RDS":

1. **A measured latency budget.** Count the round trips in a completion
   transaction and a lease claim, multiply by the measured RTT, and compare
   against the lease and fencing timeouts already in the code. If a lease can
   expire inside one transaction's round trips, the timeouts change *before*
   the database does.
2. **A rollback that has actually been run.** Restore the current `pg_dump`
   from `~/.culture-nodes/backups` into the RDS instance and run the stack
   against it. A rollback that has never been executed is not a rollback.

## Troubleshooting

**`InvalidClientTokenId` on every call to the region.** The region is not
enabled. It is not your credential. See blocker 1.

**`enable-region` itself returns `AccessDenied`.** You are not running as an
account admin. `account:EnableRegion` is deliberately absent from the scoped
operator policy.

**The region says `ENABLING` and stays that way.** That is normal for several
minutes. `preflight.py` is read-only; re-run it rather than re-running the
enable.

**`update-policy` says the policy has 5 versions.** IAM caps a policy at five;
the script prunes the oldest non-default version automatically. The rollback
store is git, not IAM's version list.

**Latency comes back over budget.** Re-run `preflight.py` *from the machine
that will host the control plane* — measuring from a laptop on a different
network answers a different question.
