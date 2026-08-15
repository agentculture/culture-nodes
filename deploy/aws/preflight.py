#!/usr/bin/env python3
"""preflight.py — is this AWS account ready to host the culture-nodes database?

Run this FIRST, before any provisioning. It answers one question per check,
in plain language, and for every failure it names three things: **why** the
check exists, **who** is allowed to fix it, and the **exact command** that
does. Nothing here mutates anything — every probe is a read.

    ./deploy/aws/preflight.py                      # human-readable report
    ./deploy/aws/preflight.py --json               # same result, machine-readable
    ./deploy/aws/preflight.py --region eu-west-1   # a different region
    ./deploy/aws/preflight.py --db-target rds      # check the opt-in RDS path too

Exit codes follow the repo's CLI policy: ``0`` everything ready, ``1`` at
least one prerequisite is blocked (the report says which), ``2`` this
machine cannot run the checks at all (no AWS CLI, no credentials).

## Why this file exists

AWS refuses in ways that read as something other than what they are, and
two of those cost real time to diagnose:

* an **opt-in region** answers *every* API call with
  ``InvalidClientTokenId`` until the account enables it — a signature that
  reads like a broken credential, not like a disabled region. A second
  propagation hides behind the first: after the opt-status flips to
  ENABLED, credentials stay invalid there for a few more minutes;
* a **missing grant** surfaces as ``AccessDenied`` on whichever call
  happens to run first, which says nothing about which policy to change.

This script turns each into a labelled row with a fix attached.

## Database target — the checks follow the choice

Where the database lives is a deployment input, so ``--db-target`` decides
which checks even apply:

* ``local`` (default) — the bundled Postgres container on its own host.
  Durability comes from shipping its dump to S3, so S3 reachability is what
  gets checked and the RDS rows are marked not-applicable rather than
  pending.
* ``rds`` — the opt-in AWS path. Its grant is a separate attachable policy
  (``rds-optional-policy.json``, applied with
  ``bootstrap-operator.sh enable-rds``), deliberately not part of the base
  operator policy, so a deployment that never uses RDS never carries
  permissions nothing exercises.
* ``managed`` — a hosted Postgres that authenticates by credential rather
  than by address. Nothing AWS-side to check beyond the backup bucket.

## The two roles, and why the split is deliberate

* **operator** — ``culture-nodes-dev``, the scoped profile agents use.
  Runs everything in this script, and the provisioning itself once the
  policy allows it.
* **account admin** — root or an admin identity, human-run. Enables an
  opt-in region; changes the operator policy.

Enabling a region and editing the operator's own permissions are
account-level changes. They stay with the admin identity on purpose: an
automation credential that can widen its own policy is not a scoped
credential. Both admin actions are wrapped by ``bootstrap-operator.sh`` so
the admin runs one documented command rather than clicking through a
console.
"""

from __future__ import annotations

import argparse
import dataclasses
import json
import shutil
import socket
import statistics
import subprocess  # nosec B404 — every call is a fixed-argv `aws` read, never a shell
import sys
import time
from typing import Any

#: The region this script assumes when nobody says otherwise.
#:
#: This is the REFERENCE default — the one a stranger cloning this repo
#: gets — and it is deliberately boring: us-east-2 is enabled on every
#: account without an opt-in step, sits in the cheapest pricing tier
#: (measured: $0.016/hr for db.t4g.micro against il-central-1's $0.018),
#: carries the broadest service coverage, and has the operational track
#: record people reach for when they want us-east-1's ecosystem without
#: us-east-1's outage history.
#:
#: It is NOT a statement about where any particular deployment's database
#: belongs — that is a deployment input, and ``--region`` is how you say
#: so. A deployment whose control plane sits far from us-east-2 pays for it
#: in round trips, and the latency check below reports exactly that. For
#: reference, measured TCP connect from thor: il-central-1 13ms,
#: eu-south-1 54ms, eu-central-1 68ms, us-east-1 155ms, us-east-2 161ms,
#: us-west-2 224ms.
DEFAULT_REGION = "us-east-2"

#: A region known to be enabled on every account, used to ask questions
#: whose answer does not depend on the region — IAM permissions are global,
#: so a policy gap can be reported even while the target region is still
#: switched off. Without this, an admin clears one blocker, waits ten
#: minutes for it to propagate, and only then learns about the second.
FALLBACK_REGION = "us-east-1"

#: The round-trip time above which this script says the link is slow.
#:
#: Deliberately a warning and never a blocker, because at the engine's
#: actual timeouts — a 60-second lease with a 20-second heartbeat
#: (internal/worker/worker.go:26-37) — even a 161ms round trip does not
#: threaten correctness: a hundred round trips still fit inside one lease.
#: What a distant region costs is responsiveness. Every claim, every
#: one-second poll, every transition and every UI read pays the RTT, so a
#: node transition doing five to ten round trips takes roughly a second
#: instead of roughly a tenth of one. Worth knowing, worth choosing
#: deliberately, not worth refusing to deploy over.
LATENCY_BUDGET_MS = 30.0

#: Everything this project creates is named with this prefix, and the
#: operator policy fences on it.
PROJECT_PREFIX = "culture-nodes-"

#: Marker AWS returns for an opt-in region the account has not enabled. It
#: arrives as a credential error, which is why it is worth naming.
OPT_IN_MARKER = "InvalidClientTokenId"

OK = "ok"
BLOCKED = "blocked"
#: Requested and in flight at AWS. Not ready, so the exit code still says
#: "do not provision yet" — but nobody has anything to do, so it must never
#: appear in the next-actions list. A report that tells an admin to re-run
#: the command they just ran sends them hunting for a failure that has not
#: happened.
PENDING = "pending"
WARN = "warn"
SKIPPED = "skipped"


@dataclasses.dataclass
class Result:
    """One prerequisite, its verdict, and how to clear it."""

    key: str
    title: str
    status: str
    detail: str
    why: str = ""
    who: str = ""
    fix: str = ""

    def as_dict(self) -> dict[str, Any]:
        return dataclasses.asdict(self)


def aws(*args: str, region: str | None = None, timeout: int = 45) -> tuple[int, str, str]:
    """Run one `aws` read and hand back (returncode, stdout, stderr).

    Fixed argv, never a shell string: nothing here interpolates user input
    into a command line.
    """
    argv = ["aws", *args, "--output", "json"]
    if region:
        argv += ["--region", region]
    try:
        proc = subprocess.run(  # nosec B603 — fixed argv, no shell
            argv, capture_output=True, text=True, timeout=timeout, check=False
        )
    except subprocess.TimeoutExpired:
        return 124, "", f"timed out after {timeout}s: {' '.join(argv)}"
    except FileNotFoundError:
        return 127, "", "aws CLI not found on PATH"
    return proc.returncode, proc.stdout.strip(), proc.stderr.strip()


def denied(stderr: str) -> bool:
    return "AccessDenied" in stderr or "not authorized to perform" in stderr


def region_disabled(stderr: str) -> bool:
    return OPT_IN_MARKER in stderr


# --------------------------------------------------------------------------
# checks
# --------------------------------------------------------------------------


def check_cli() -> Result:
    path = shutil.which("aws")
    if not path:
        return Result(
            key="cli",
            title="AWS CLI installed",
            status=BLOCKED,
            detail="`aws` is not on PATH",
            why="every probe and every provisioning step is an AWS CLI call",
            who="whoever operates this machine",
            fix="curl -sSL https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip -o /tmp/awscli.zip"
            " && unzip -q /tmp/awscli.zip -d /tmp && sudo /tmp/aws/install",
        )
    code, out, err = aws("--version")
    version = (out or err).split()[0] if (out or err) else "unknown"
    return Result(key="cli", title="AWS CLI installed", status=OK, detail=f"{version} at {path}")


def check_credentials() -> tuple[Result, str | None]:
    """Resolve the caller. Returns the result and the account id when known."""
    code, out, err = aws("sts", "get-caller-identity")
    if code != 0:
        return (
            Result(
                key="credentials",
                title="Credentials resolve",
                status=BLOCKED,
                detail=err.splitlines()[-1] if err else "sts:GetCallerIdentity failed",
                why="nothing else can be checked without an identity",
                who="operator",
                fix="export AWS_PROFILE=culture-nodes   # or re-run"
                " deploy/aws/bootstrap-operator.sh as an admin to mint the profile",
            ),
            None,
        )
    identity = json.loads(out)
    return (
        Result(
            key="credentials",
            title="Credentials resolve",
            status=OK,
            detail=f"{identity['Arn']} (account {identity['Account']})",
        ),
        identity["Account"],
    )


def check_region_enabled(region: str, account: str | None) -> Result:
    """Is the target region enabled on this account?

    Two probes, because the precise one needs a permission the operator may
    not hold yet. ``account:GetRegionOptStatus`` gives a named status;
    without it, an ``sts`` call into the region returns InvalidClientTokenId
    when the region is off, which is diagnostic even though it reads like a
    credential problem.
    """
    fix = f"./deploy/aws/bootstrap-operator.sh enable-region {region}"
    why = (
        f"{region} is an opt-in region: until the account enables it, EVERY API call there"
        f" fails with {OPT_IN_MARKER}, which looks like a broken credential and is not one"
    )
    code, out, err = aws("account", "get-region-opt-status", "--region-name", region)
    if code == 0:
        status = json.loads(out).get("RegionOptStatus", "UNKNOWN")
        if status in ("ENABLED", "ENABLED_BY_DEFAULT"):
            return Result(
                key="region",
                title=f"Region {region} enabled",
                status=OK,
                detail=f"RegionOptStatus={status}",
            )
        if status in ("ENABLING", "DISABLING"):
            # In flight. Not ready, but there is nothing for anyone to do —
            # and telling an admin to re-run the enable they already ran is
            # worse than saying nothing, because it invites them to go
            # looking for a failure that has not happened.
            return Result(
                key="region",
                title=f"Region {region} enabled",
                status=PENDING,
                detail=f"RegionOptStatus={status} — AWS is working on it, typically a few minutes",
                why="an opt-in region takes time to propagate after the account requests it",
                who="nobody — no action to take",
                fix=f"wait, then re-run ./deploy/aws/preflight.py --region {region}",
            )
        return Result(
            key="region",
            title=f"Region {region} enabled",
            status=BLOCKED,
            detail=f"RegionOptStatus={status}",
            why=why,
            who="account admin — an account-level setting, ~10 minutes to take effect",
            fix=fix,
        )

    # Fall back to the indirect probe.
    code, _out, err = aws("sts", "get-caller-identity", region=region)
    if code == 0:
        return Result(
            key="region",
            title=f"Region {region} enabled",
            status=OK,
            detail=f"sts answered in {region} (opt-status unreadable: account:GetRegionOptStatus"
            " not granted, which is cosmetic once the region works)",
        )
    if region_disabled(err):
        return Result(
            key="region",
            title=f"Region {region} enabled",
            status=BLOCKED,
            detail=f"sts in {region} returned {OPT_IN_MARKER} — the region is not enabled",
            why=why,
            who="account admin — an account-level setting, ~10 minutes to take effect",
            fix=fix,
        )
    return Result(
        key="region",
        title=f"Region {region} enabled",
        status=BLOCKED,
        detail=err.splitlines()[-1] if err else "unknown failure",
        why=why,
        who="account admin",
        fix=fix,
    )


def check_rds_permissions(region: str, region_ok: bool) -> Result:
    """Can the operator identity use RDS at all?

    Deliberately probed in a region that already answers, when the target
    one does not. IAM is global — the permission verdict is identical in
    every region — and asking it here means an admin sees BOTH blockers in
    one report instead of clearing the region, waiting ten minutes, and
    only then discovering the policy gap.
    """
    probe_region, note = (
        (region, "")
        if region_ok
        else (FALLBACK_REGION, f" (probed in {FALLBACK_REGION}; IAM is global)")
    )
    code, _out, err = aws("rds", "describe-db-instances", region=probe_region)
    if code == 0:
        return Result(
            key="rds_iam",
            title="Operator may use RDS",
            status=OK,
            detail=f"rds:DescribeDBInstances succeeded{note}",
        )
    if region_disabled(err):
        # The opt-status flipped to ENABLED but the credential is not valid
        # in the new region yet — AWS propagates that separately, and for a
        # few minutes a freshly enabled region rejects a key that works
        # everywhere else. Reporting this as a policy problem would send an
        # admin to re-run update-policy, which fixes nothing.
        return Result(
            key="rds_iam",
            title="Operator may use RDS",
            status=PENDING,
            detail=f"{probe_region} answered {OPT_IN_MARKER} — the region was just enabled and"
            " credentials are still propagating into it",
            why="enabling a region and making credentials valid there are two separate"
            " propagations; the second trails the first by a few minutes",
            who="nobody — no action to take",
            fix=f"wait, then re-run ./deploy/aws/preflight.py --region {region}",
        )
    if denied(err):
        return Result(
            key="rds_iam",
            title="Operator may use RDS",
            status=BLOCKED,
            detail="rds:DescribeDBInstances denied — the operator policy grants no RDS",
            why="provisioning, describing and snapshotting the database all need RDS actions;"
            " the policy predates this decision and was scoped to SQS, S3, ECR, Lambda and IAM",
            who="account admin — RDS is an opt-in target, so this is a deliberate widening",
            fix="./deploy/aws/bootstrap-operator.sh enable-rds"
            "   # attaches deploy/aws/rds-optional-policy.json; disable-rds detaches it",
        )
    return Result(
        key="rds_iam",
        title="Operator may use RDS",
        status=BLOCKED,
        detail=err.splitlines()[-1] if err else "unknown failure",
        why="RDS must be reachable before anything can be provisioned",
        who="account admin",
        fix="./deploy/aws/bootstrap-operator.sh update-policy",
    )


def check_vpc_reads(region: str, region_ok: bool) -> Result:
    """Same reasoning as check_rds_permissions: a global answer, asked where it can be."""
    probe_region, note = (
        (region, "")
        if region_ok
        else (FALLBACK_REGION, f" (probed in {FALLBACK_REGION}; IAM is global)")
    )
    code, _out, err = aws("ec2", "describe-vpcs", "--max-items", "1", region=probe_region)
    if code == 0:
        return Result(
            key="vpc_iam",
            title="Operator may read VPC placement",
            status=OK,
            detail=f"ec2:DescribeVpcs succeeded{note}",
        )
    if region_disabled(err):
        return Result(
            key="vpc_iam",
            title="Operator may read VPC placement",
            status=PENDING,
            detail=f"{probe_region} answered {OPT_IN_MARKER} — the region was just enabled and"
            " credentials are still propagating into it",
            why="enabling a region and making credentials valid there are two separate"
            " propagations; the second trails the first by a few minutes",
            who="nobody — no action to take",
            fix=f"wait, then re-run ./deploy/aws/preflight.py --region {region}",
        )
    if denied(err):
        return Result(
            key="vpc_iam",
            title="Operator may read VPC placement",
            status=BLOCKED,
            detail="ec2:DescribeVpcs denied",
            why="an RDS instance needs a subnet group and a security group, and choosing them"
            " means reading the VPC, its subnets and its availability zones first",
            who="account admin",
            fix="./deploy/aws/bootstrap-operator.sh update-policy",
        )
    return Result(
        key="vpc_iam",
        title="Operator may read VPC placement",
        status=BLOCKED,
        detail=err.splitlines()[-1] if err else "unknown failure",
        why="VPC reads precede any database placement decision",
        who="account admin",
        fix="./deploy/aws/bootstrap-operator.sh update-policy",
    )


def check_instance_class(region: str, region_ok: bool, rds_ok: bool, klass: str) -> Result:
    if not (region_ok and rds_ok):
        return Result(
            key="instance_class",
            title=f"{klass} orderable for postgres",
            status=SKIPPED,
            detail="needs the region enabled and RDS permitted",
        )
    code, out, err = aws(
        "rds",
        "describe-orderable-db-instance-options",
        "--engine",
        "postgres",
        "--db-instance-class",
        klass,
        "--query",
        "OrderableDBInstanceOptions[0].{class:DBInstanceClass,az:AvailabilityZones[0].Name}",
        region=region,
    )
    if code == 0 and out and out != "null":
        return Result(
            key="instance_class",
            title=f"{klass} orderable for postgres",
            status=OK,
            detail=f"available in {region}",
        )
    if code == 0:
        return Result(
            key="instance_class",
            title=f"{klass} orderable for postgres",
            status=WARN,
            detail=f"{klass} is not offered in {region} — pick another class before provisioning",
            why="a class that does not exist in the region fails at CreateDBInstance, late",
            who="operator",
            fix=f"aws rds describe-orderable-db-instance-options --engine postgres"
            f" --region {region} --query 'OrderableDBInstanceOptions[].DBInstanceClass'"
            " --output text | tr '\\t' '\\n' | sort -u",
        )
    return Result(
        key="instance_class",
        title=f"{klass} orderable for postgres",
        status=WARN,
        detail=err.splitlines()[-1] if err else "probe failed",
    )


def check_latency(region: str) -> Result:
    """Measure TCP connect time from THIS machine to the region's RDS endpoint.

    The spec commits to a latency budget because the control plane stays on
    thor while the database moves: every engine transaction, lease claim and
    tick crosses this link. A region that is far away is not a slower
    deployment, it is a different set of timeout assumptions.
    """
    host = f"rds.{region}.amazonaws.com"
    samples: list[float] = []
    for _ in range(3):
        started = time.monotonic()
        try:
            with socket.create_connection((host, 443), timeout=5):
                samples.append((time.monotonic() - started) * 1000.0)
        except OSError as exc:
            return Result(
                key="latency",
                title=f"Latency to {region} within budget",
                status=WARN,
                detail=f"could not connect to {host}:443 ({exc})",
            )
    median = statistics.median(samples)
    hostname = socket.gethostname()
    if median <= LATENCY_BUDGET_MS:
        return Result(
            key="latency",
            title=f"Latency to {region} within budget",
            status=OK,
            detail=f"{median:.0f}ms median TCP connect from {hostname} (budget {LATENCY_BUDGET_MS:.0f}ms)",
        )
    return Result(
        key="latency",
        title=f"Latency to {region} within budget",
        status=WARN,
        detail=f"{median:.0f}ms median TCP connect from {hostname} — over the"
        f" {LATENCY_BUDGET_MS:.0f}ms guideline (a warning, never a blocker)",
        why="correctness is unaffected — a 60s lease with a 20s heartbeat absorbs round trips"
        " this size — but every claim, one-second poll, transition and UI read pays this cost,"
        " so transitions land in about a second rather than about a tenth of one",
        who="operator — a deliberate choice, not an error. Re-run from the machine that will"
        " actually host the control plane, and pick --region accordingly",
        fix=f"for r in il-central-1 eu-central-1 {region}; do printf '%s ' $r;"
        " curl -s -o /dev/null -w 'connect=%{time_connect}\\n'"
        " https://sts.$r.amazonaws.com/ --max-time 10; done",
    )


def check_backup_bucket(region: str) -> Result:
    """Can this identity reach S3 for the database backups?

    This is the check that matters for the target this deployment actually
    chose. The database stays on its own host; what makes it durable is
    shipping the six-hourly pg_dump off that host into object storage, and
    the base operator policy already grants s3 on culture-nodes-* for it.
    """
    code, out, err = aws("s3api", "list-buckets", "--query", "Buckets[].Name", region=region)
    if code != 0:
        return Result(
            key="s3_backups",
            title="Operator may reach S3 for backups",
            status=BLOCKED,
            detail=err.splitlines()[-1] if err else "s3:ListAllMyBuckets failed",
            why="off-host durability for the database is a backup in object storage; without S3"
            " the dumps stay on the same disk as the data they protect",
            who="account admin",
            fix="./deploy/aws/bootstrap-operator.sh update-policy",
        )
    buckets = [b for b in json.loads(out or "[]") if b.startswith(PROJECT_PREFIX)]
    if buckets:
        return Result(
            key="s3_backups",
            title="Operator may reach S3 for backups",
            status=OK,
            detail=f"s3 reachable; project buckets: {', '.join(buckets)}",
        )
    return Result(
        key="s3_backups",
        title="Operator may reach S3 for backups",
        status=OK,
        detail=f"s3 reachable; no {PROJECT_PREFIX}* bucket yet — the backup work creates it",
    )


def check_existing_instance(region: str, region_ok: bool, rds_ok: bool) -> Result:
    """Idempotency: has an instance already been provisioned?"""
    if not (region_ok and rds_ok):
        return Result(
            key="existing",
            title="Existing culture-nodes database",
            status=SKIPPED,
            detail="needs the region enabled and RDS permitted",
        )
    code, out, _err = aws(
        "rds",
        "describe-db-instances",
        "--query",
        "DBInstances[].{id:DBInstanceIdentifier,status:DBInstanceStatus,endpoint:Endpoint.Address}",
        region=region,
    )
    if code != 0:
        return Result(
            key="existing",
            title="Existing culture-nodes database",
            status=WARN,
            detail="could not enumerate instances",
        )
    rows = [r for r in json.loads(out or "[]") if r["id"].startswith(PROJECT_PREFIX)]
    if not rows:
        return Result(
            key="existing",
            title="Existing culture-nodes database",
            status=OK,
            detail=f"none yet — nothing named {PROJECT_PREFIX}* in {region}",
        )
    described = ", ".join(f"{r['id']} ({r['status']}) {r['endpoint'] or ''}".strip() for r in rows)
    return Result(
        key="existing",
        title="Existing culture-nodes database",
        status=OK,
        detail=f"already provisioned: {described}",
    )


# --------------------------------------------------------------------------
# report
# --------------------------------------------------------------------------

BADGE = {
    OK: "  ok   ",
    BLOCKED: "BLOCKED",
    PENDING: "waiting",
    WARN: " warn  ",
    SKIPPED: " skip  ",
}


def render(results: list[Result], region: str) -> str:
    lines = [
        "",
        f"culture-nodes AWS preflight — target region {region}",
        "=" * 64,
        "",
    ]
    for r in results:
        lines.append(f"  [{BADGE[r.status]}]  {r.title}")
        lines.append(f"              {r.detail}")
        if r.status in (BLOCKED, PENDING, WARN) and r.why:
            lines.append(f"              why:  {r.why}")
            lines.append(f"              who:  {r.who}")
            lines.append(f"              fix:  {r.fix}")
        lines.append("")
    counts = {
        s: sum(1 for r in results if r.status == s) for s in (OK, BLOCKED, PENDING, WARN, SKIPPED)
    }
    lines.append("-" * 64)
    lines.append(
        f"  {counts[OK]} ready, {counts[BLOCKED]} blocked, {counts[PENDING]} in progress,"
        f" {counts[WARN]} warning, {counts[SKIPPED]} not yet testable"
    )
    if counts[BLOCKED]:
        lines.append("")
        lines.append("  Next actions, in order:")
        # One line per distinct command: three blocked checks can share a
        # single fix (the policy update clears both the RDS and the VPC
        # rows), and printing it twice reads like two pieces of work.
        seen: list[str] = []
        for r in (x for x in results if x.status == BLOCKED):
            if r.fix and r.fix not in seen:
                seen.append(r.fix)
                lines.append(f"    {len(seen)}. [{r.who.split(' — ')[0]}]  {r.fix}")
        lines.append("")
        lines.append("  Then re-run this script. It is a read-only check and is safe to repeat.")
    elif counts[PENDING]:
        lines.append("")
        lines.append("  Nothing to do — AWS is still working. Re-run this script in a few minutes:")
        for r in (x for x in results if x.status == PENDING):
            lines.append(f"    - {r.title}: {r.detail}")
    else:
        lines.append("")
        lines.append("  All prerequisites are clear. Provisioning may proceed.")
    lines.append("")
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="preflight.py",
        description="Check whether this AWS account can host the culture-nodes database.",
    )
    parser.add_argument(
        "--region", default=DEFAULT_REGION, help=f"target region (default: {DEFAULT_REGION})"
    )
    parser.add_argument("--instance-class", default="db.t4g.micro", help="RDS class to check for")
    parser.add_argument(
        "--db-target",
        choices=("local", "rds", "managed"),
        default="local",
        help="where the database lives: local (the bundled container, the default), rds"
        " (opt-in, needs bootstrap-operator.sh enable-rds), or managed (a hosted Postgres"
        " that authenticates by credential). Only 'rds' makes the RDS checks apply.",
    )
    parser.add_argument("--json", action="store_true", help="emit the report as JSON on stdout")
    args = parser.parse_args(argv)

    results: list[Result] = []

    cli = check_cli()
    results.append(cli)
    if cli.status == BLOCKED:
        _emit(results, args, exit_hint="install the AWS CLI, then re-run")
        return 2

    creds, account = check_credentials()
    results.append(creds)
    if creds.status == BLOCKED:
        _emit(results, args, exit_hint="resolve credentials, then re-run")
        return 2

    region = check_region_enabled(args.region, account)
    results.append(region)
    region_ok = region.status == OK

    results.append(check_backup_bucket(args.region))

    if args.db_target == "rds":
        rds = check_rds_permissions(args.region, region_ok)
        results.append(rds)
        rds_ok = rds.status == OK
        results.append(check_vpc_reads(args.region, region_ok))
        results.append(check_instance_class(args.region, region_ok, rds_ok, args.instance_class))
        results.append(check_latency(args.region))
        results.append(check_existing_instance(args.region, region_ok, rds_ok))
    else:
        # RDS is opt-in and not the chosen target, so its checks are not
        # merely skipped-for-now — they do not apply. Saying so beats a row
        # of `skip` that reads like something is still pending.
        note = (
            "the bundled container on its own host"
            if args.db_target == "local"
            else "a hosted Postgres that authenticates by credential"
        )
        results.append(
            Result(
                key="rds_iam",
                title="RDS checks",
                status=SKIPPED,
                detail=f"not applicable — database target is '{args.db_target}': {note}."
                " Re-run with --db-target rds to check the RDS path.",
            )
        )

    _emit(results, args)
    # In-progress counts as not-ready: the exit code gates provisioning, and
    # a region that is still ENABLING cannot host anything yet. It differs
    # from BLOCKED only in that no human owes anyone an action.
    return 1 if any(r.status in (BLOCKED, PENDING) for r in results) else 0


def _emit(results: list[Result], args: argparse.Namespace, exit_hint: str = "") -> None:
    if args.json:
        payload = {
            "region": args.region,
            "db_target": args.db_target,
            "ready": not any(r.status in (BLOCKED, PENDING) for r in results),
            "blocked": [r.key for r in results if r.status == BLOCKED],
            "in_progress": [r.key for r in results if r.status == PENDING],
            "checks": [r.as_dict() for r in results],
        }
        print(json.dumps(payload, indent=2))
    else:
        print(render(results, args.region))
    if exit_hint:
        print(f"hint: {exit_hint}", file=sys.stderr)


if __name__ == "__main__":
    sys.exit(main())
