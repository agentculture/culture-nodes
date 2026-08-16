import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
GUARD = ROOT / "scripts" / "check-inbound-auth-dump.py"


def run_guard(tmp_path, schema, dump):
    schema_path = tmp_path / "schema.sql"
    dump_path = tmp_path / "dump.sql"
    schema_path.write_text(schema)
    dump_path.write_text(dump)
    return subprocess.run(
        [sys.executable, GUARD, "--schema-file", schema_path, "--dump-file", dump_path],
        text=True,
        capture_output=True,
    )


def test_guard_accepts_hashes_and_environment_names(tmp_path):
    result = run_guard(
        tmp_path,
        """CREATE TABLE public.inbound_authentication (
 party_kind text NOT NULL,
 party_key text NOT NULL,
 verifier_sha256 bytea,
 verifier_env_name text
);""",
        "COPY public.inbound_authentication FROM stdin;\nhost\tbridge-a\t\\x00\t\\N\n\\.\n",
    )
    assert result.returncode == 0, result.stderr
    assert "PASS:" in result.stdout


def test_guard_rejects_plaintext_capable_schema_column(tmp_path):
    result = run_guard(
        tmp_path,
        """CREATE TABLE public.inbound_authentication (
 party_key text NOT NULL,
 credential text
);""",
        "",
    )
    assert result.returncode == 1
    assert "credential" in result.stderr


def test_guard_rejects_presentable_canary_in_dump(tmp_path):
    result = run_guard(
        tmp_path,
        "CREATE TABLE public.inbound_authentication (verifier_env_name text);",
        "COPY public.inbound_authentication FROM stdin;\nATTACKER_CAN_PRESENT_ME\n\\.\n",
    )
    assert result.returncode == 1
    assert "presentable" in result.stderr
