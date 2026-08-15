# AWS prerequisites — what we need, and how to clear it

**Audience:** whoever holds admin on the AWS account. You do not need to know
anything about culture-nodes to work through this page.

**Time:** about 15 minutes, most of it waiting for AWS.

**What you are enabling:** culture-nodes is moving its authoritative
PostgreSQL database from a Docker container on one machine into managed RDS.
The application itself stays where it is. Two account-level things block that
move, and both need an admin identity — not the scoped automation credential.

---

## TL;DR

```bash
# 1. See exactly what is missing (read-only, safe, repeatable)
./deploy/aws/preflight.py

# 2. As an ACCOUNT ADMIN, clear whatever it reported:
./deploy/aws/bootstrap-operator.sh enable-region il-central-1
./deploy/aws/bootstrap-operator.sh update-policy

# 3. Confirm
./deploy/aws/preflight.py     # exits 0 when everything is ready
```

`preflight.py` prints, for every failing check, **why** it exists, **who** may
fix it, and the **exact command**. If you read nothing else on this page, run
it — it is the authoritative version of this document.

---

## The two blockers, in plain language

### 1. The region `il-central-1` is not enabled on the account

AWS regions launched after 2019 are **opt-in**: they exist, but your account
cannot use them until you say so. Ours is not enabled today, measured:

```console
$ aws sts get-caller-identity --region il-central-1
An error occurred (InvalidClientTokenId) when calling the GetCallerIdentity
operation: The security token included in the request is invalid.
```

**Read that error carefully, because it is misleading.** It says the
credential is invalid. The credential is fine. An opt-in region answers
*every* API call that way until the account enables it. Anyone debugging this
from the error alone will spend an hour rotating a key that was never broken.

**Why this region and not the one we already use.** Measured TCP connect from
`thor`, the machine that will keep running the application:

| Region | Connect | |
|---|---:|---|
| `il-central-1` (Tel Aviv) | **19 ms** | the target |
| `eu-central-1` (Frankfurt) | 70 ms | |
| `us-east-1` (N. Virginia) | 160 ms | where our existing SQS/Lambda lane lives |

The application stays on `thor` while the database moves, so this link carries
every database round trip the engine makes — transactions, lease claims, the
scheduler tick. An eight-fold latency difference is not a preference, it is
whether the lease timeouts still hold. **The SQS and Lambda resources stay in
`us-east-1`.** A split-region deployment is correct here, not something to
tidy up later.

**Fix it:**

```bash
./deploy/aws/bootstrap-operator.sh enable-region il-central-1
```

The script checks the current status first and does nothing if the region is
already on or already enabling. AWS takes a few minutes to propagate; re-run
`./deploy/aws/preflight.py` until it reports `ok`.

> **Why can't the automation do this itself?** It is an account-wide setting.
> A credential that can turn regions on for the whole account is not a scoped
> credential. The automation gets the *read* (`account:GetRegionOptStatus`) so
> it can report the status, and nothing more.

### 2. The operator policy grants no RDS at all

The scoped IAM user the automation runs as, `culture-nodes-dev`, was created
before this decision. Its policy covers SQS, S3, ECR, Lambda, scoped IAM and
STS. It has never had a single RDS permission:

```console
$ aws rds describe-orderable-db-instance-options --engine postgres
An error occurred (AccessDenied) ... User: arn:aws:iam::435593604218:user/
culture-nodes-dev is not authorized to perform:
rds:DescribeOrderableDBInstanceOptions because no identity-based policy allows
the rds:DescribeOrderableDBInstanceOptions action
```

**Fix it:**

```bash
./deploy/aws/bootstrap-operator.sh update-policy
```

That applies `deploy/aws/dev-operator-policy.json` as a new default policy
version. The file is committed to the repository, so what you are granting is
a reviewable diff in git — not something typed into a console. Read it before
you run this; that is the point of it being a file.

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
| Enable the region | **account admin** | `./deploy/aws/bootstrap-operator.sh enable-region il-central-1` | account setting |
| Grant RDS | **account admin** | `./deploy/aws/bootstrap-operator.sh update-policy` | IAM policy version |
| Confirm | operator | `./deploy/aws/preflight.py` | no |

Per standing policy, agent sessions never run `bootstrap-operator.sh` and
never handle key material. They run `preflight.py`, read its report, and tell
a human which of the two commands above to run.

---

## Reading the preflight report

```text
culture-nodes AWS preflight — target region il-central-1
================================================================

  [  ok   ]  AWS CLI installed
              aws-cli/2.x.y at /usr/local/bin/aws

  [BLOCKED]  Region il-central-1 enabled
              sts in il-central-1 returned InvalidClientTokenId — the region is not enabled
              why:  il-central-1 is an opt-in region: until the account enables it, EVERY
                    API call there fails with InvalidClientTokenId, which looks like a
                    broken credential and is not one
              who:  account admin — an account-level setting, ~10 minutes to take effect
              fix:  ./deploy/aws/bootstrap-operator.sh enable-region il-central-1
```

| Badge | Meaning |
|---|---|
| `ok` | ready |
| `BLOCKED` | must be cleared before provisioning; the report names who and how |
| `warn` | proceed with attention — e.g. latency over the 30 ms budget |
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
