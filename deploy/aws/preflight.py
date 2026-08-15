#!/usr/bin/env python3
"""preflight.py — is this AWS account ready to host the culture-nodes database?

Run this FIRST, before any provisioning. It answers one question per check,
in plain language, and for every failure it names three things: **why** the
check exists, **who** is allowed to fix it, and the **exact command** that
does. Nothing here mutates anything — every probe is a read.

    ./deploy/aws/preflight.py                     # human-readable report
    ./deploy/aws/preflight.py --json              # same result, machine-readable
    ./deploy/aws/preflight.py --region eu-west-1  # check a different target

Exit codes follow the repo's CLI policy: ``0`` everything ready, ``1`` at
least one prerequisite is blocked (the report says which), ``2`` this
machine cannot run the checks at all (no AWS CLI, no credentials).

## Why this file exists

Two prerequisites blocked the #59 migration, and neither was discoverable
without an AWS call that fails in a confusing way:

* an **opt-in region** answers *every* API call with
  ``InvalidClientTokenId`` until the account enables it — a signature that
  reads like a broken credential, not like a disabled region;
* the scoped ``culture-nodes-dev`` operator policy grants SQS, S3, ECR,
  Lambda, IAM and STS, but granted **no RDS at all**, so provisioning fails
  with ``AccessDenied`` on the first describe.

Both cost time to diagnose from the error alone. This script turns each one
into a labelled row with a fix attached.

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

#: The region the database goes in. il-central-1 (Tel Aviv) was chosen by
#: measurement, not preference: TCP connect from thor is 19ms to
#: il-central-1, 70ms to eu-central-1 and 160ms to us-east-1, and with the
#: control plane staying on thor the worker-to-Postgres link is the
#: chattiest path in the system. The SQS and Lambda lanes stay in
#: us-east-1; a split-region deployment is correct here.
DEFAULT_REGION = "il-central-1"

#: A region known to be enabled on every account, used to ask questions
#: whose answer does not depend on the region — IAM permissions are global,
#: so a policy gap can be reported even while the target region is still
#: switched off. Without this, an admin clears one blocker, waits ten
#: minutes for it to propagate, and only then learns about the second.
FALLBACK_REGION = "us-east-1"

#: The latency ceiling the migration spec commits to. Above this the region
#: choice is wrong and the cutover stops rather than proceeding.
LATENCY_BUDGET_MS = 30.0

#: Everything this project creates is named with this prefix, and the
#: operator policy fences on it.
PROJECT_PREFIX = "culture-nodes-"

#: Marker AWS returns for an opt-in region the account has not enabled. It
#: arrives as a credential error, which is why it is worth naming.
OPT_IN_MARKER = "InvalidClientTokenId"

OK = "ok"
BLOCKED = "blocked"
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
    if denied(err):
        return Result(
            key="rds_iam",
            title="Operator may use RDS",
            status=BLOCKED,
            detail="rds:DescribeDBInstances denied — the operator policy grants no RDS",
            why="provisioning, describing and snapshotting the database all need RDS actions;"
            " the policy predates this decision and was scoped to SQS, S3, ECR, Lambda and IAM",
            who="account admin — the policy is a checked-in artifact, so this is a reviewed diff",
            fix="./deploy/aws/bootstrap-operator.sh update-policy"
            "   # applies deploy/aws/dev-operator-policy.json as a new default version",
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
        f" {LATENCY_BUDGET_MS:.0f}ms budget",
        why="the control plane stays on thor, so this link carries every engine transaction,"
        " every lease claim and every tick; over budget means the timeouts change before the"
        " database does",
        who="operator — re-run from the machine that will actually host the control plane",
        fix=f"for r in il-central-1 eu-central-1 {region}; do printf '%s ' $r;"
        " curl -s -o /dev/null -w 'connect=%{time_connect}\\n'"
        " https://sts.$r.amazonaws.com/ --max-time 10; done",
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

BADGE = {OK: "  ok   ", BLOCKED: "BLOCKED", WARN: " warn  ", SKIPPED: " skip  "}


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
        if r.status in (BLOCKED, WARN) and r.why:
            lines.append(f"              why:  {r.why}")
            lines.append(f"              who:  {r.who}")
            lines.append(f"              fix:  {r.fix}")
        lines.append("")
    counts = {s: sum(1 for r in results if r.status == s) for s in (OK, BLOCKED, WARN, SKIPPED)}
    lines.append("-" * 64)
    lines.append(
        f"  {counts[OK]} ready, {counts[BLOCKED]} blocked,"
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

    rds = check_rds_permissions(args.region, region_ok)
    results.append(rds)
    rds_ok = rds.status == OK

    results.append(check_vpc_reads(args.region, region_ok))
    results.append(check_instance_class(args.region, region_ok, rds_ok, args.instance_class))
    results.append(check_latency(args.region))
    results.append(check_existing_instance(args.region, region_ok, rds_ok))

    _emit(results, args)
    return 1 if any(r.status == BLOCKED for r in results) else 0


def _emit(results: list[Result], args: argparse.Namespace, exit_hint: str = "") -> None:
    if args.json:
        payload = {
            "region": args.region,
            "ready": not any(r.status == BLOCKED for r in results),
            "checks": [r.as_dict() for r in results],
        }
        print(json.dumps(payload, indent=2))
    else:
        print(render(results, args.region))
    if exit_hint:
        print(f"hint: {exit_hint}", file=sys.stderr)


if __name__ == "__main__":
    sys.exit(main())
