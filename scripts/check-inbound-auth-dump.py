#!/usr/bin/env python3
"""Reject plaintext-capable inbound auth schema and presentable dump values."""

import argparse
import hashlib
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

TABLE = "inbound_authentication"
CANARY = "ATTACKER_CAN_PRESENT_ME"
UNSAFE_COLUMN = re.compile(
    r"(?im)^\s*[a-z0-9_]*(?:credential|secret|password|token|presented|material|value)[a-z0-9_]*\s+"
)


def check_files(schema: str, dump: str) -> list[str]:
    problems = []
    match = re.search(rf"CREATE TABLE (?:public\.)?{TABLE}\s*\((.*?)\n\);", schema, re.DOTALL)
    if not match:
        problems.append(f"schema does not define {TABLE}")
    elif found := UNSAFE_COLUMN.search(match.group(1)):
        problems.append(f"plaintext-capable credential column found: {found.group(0).strip()}")
    if CANARY in dump:
        problems.append("dump contains the presentable credential canary")
    return problems


def run(command: list[str], **kwargs) -> subprocess.CompletedProcess:
    return subprocess.run(command, text=True, capture_output=True, **kwargs)


def live_dump(database_url: str) -> tuple[str, str]:
    for program in ("psql", "pg_dump"):
        if shutil.which(program) is None:
            raise RuntimeError(f"{program} is not installed")

    psql = ["psql", database_url, "-X", "-v", "ON_ERROR_STOP=1"]
    cleanup = psql + [
        "-c",
        f"DELETE FROM {TABLE} WHERE party_kind = 'host' AND party_key = 'credential-dump-guard.invalid'",
    ]
    canary_hash = hashlib.sha256(CANARY.encode()).hexdigest()
    insert = run(
        psql
        + [
            "-c",
            f"""
        INSERT INTO {TABLE} (party_kind, party_key, verifier_sha256)
        VALUES ('host', 'credential-dump-guard.invalid', decode('{canary_hash}', 'hex'))
        ON CONFLICT (party_kind, party_key) DO UPDATE
        SET verifier_sha256 = EXCLUDED.verifier_sha256, verifier_env_name = NULL
    """,
        ]
    )
    if insert.returncode != 0:
        raise RuntimeError(insert.stderr.strip() or "could not seed dump guard row")
    try:
        schema = run(["pg_dump", database_url, "--schema-only", "--no-owner", "--no-privileges"])
        data = run(
            [
                "pg_dump",
                database_url,
                "--data-only",
                "--no-owner",
                "--no-privileges",
                f"--table={TABLE}",
            ]
        )
        if schema.returncode != 0 or data.returncode != 0:
            raise RuntimeError((schema.stderr + data.stderr).strip())
        return schema.stdout, data.stdout
    finally:
        run(cleanup)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--schema-file", type=Path)
    parser.add_argument("--dump-file", type=Path)
    args = parser.parse_args()
    if bool(args.schema_file) != bool(args.dump_file):
        parser.error("--schema-file and --dump-file must be supplied together")

    if args.schema_file:
        schema, dump = args.schema_file.read_text(), args.dump_file.read_text()
    else:
        database_url = os.environ.get("NODES_TEST_DATABASE_URL")
        if not database_url:
            print(
                "SKIP: inbound authentication schema/dump guard needs NODES_TEST_DATABASE_URL",
                file=sys.stderr,
            )
            return 2
        try:
            schema, dump = live_dump(database_url)
        except RuntimeError as exc:
            print(
                f"SKIP: inbound authentication schema/dump guard could not reach PostgreSQL: {exc}",
                file=sys.stderr,
            )
            return 2

    problems = check_files(schema, dump)
    if problems:
        print("FAIL: " + "; ".join(problems), file=sys.stderr)
        return 1
    print(
        "PASS: authentication and lockout state have no plaintext-capable column and dump has no presentable canary"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
