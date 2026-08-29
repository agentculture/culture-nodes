"""The BACKEND-SPECIFIC half of the preflight capability surface (issue #67,
task t15): the facts this bridge measures about the host it dispatches on.

The protocol shape is asserted in `test_preflight.py` against the shared,
byte-identical module; what is tested here is only what notify itself
contributes — and notify is the bridge that contributes the least, which is
the point. It runs no session, so the sandbox keys are absent rather than
empty, and the same protocol still carries what is left.
"""

from __future__ import annotations

import json
import os
import pwd
import urllib.error
import urllib.request

import pytest

from notify_bridge import capabilities, preflight, server
from notify_bridge.__main__ import main
from notify_bridge.config import Config


def _permissive(tmp_path):
    return ((str(tmp_path / "absent-knob"), "1"),)


def _restricted(tmp_path):
    knob = tmp_path / "apparmor_restrict_unprivileged_userns"
    knob.write_text("1\n")
    return ((str(knob), "1"),)


def test_the_surface_is_a_document_the_control_plane_accepts(tmp_path):
    block = preflight.capability_block(
        capabilities.host_facts(Config(), probes=_permissive(tmp_path))
    )
    preflight.validate_block(block)
    assert block["preflight"]["protocol_version"] == preflight.PROTOCOL_VERSION


def test_a_bridge_with_no_session_omits_the_sandbox_keys(tmp_path):
    """Not an empty list — an absent key. There are no confinement modes to
    report where nothing runs, and an empty list would read as "every mode
    was measured and none works", which is a claim about a session that does
    not exist."""
    host = capabilities.host_facts(Config(), probes=_permissive(tmp_path))
    assert set(host) == {
        "hostname",
        "confinement",
        "commit_policy",
        "writable_paths",
        "artifact_publish",
        # `deployment` is present even here, and deliberately so (task t32):
        # which revision a bridge is running is a fact about the BRIDGE, not
        # about the session it dispatches. A bridge that runs no session can
        # still be the stale one — this is the bridge that sends the
        # notifications, and a stale one sending yesterday's shape is exactly
        # as invisible as a stale codex bridge was in issue #120.
        "deployment",
    }
    assert host["artifact_publish"] == "not-applicable-no-workspace"


def test_what_remains_still_states_the_facts_a_dispatch_depends_on(tmp_path):
    host = capabilities.host_facts(Config(), probes=_permissive(tmp_path))
    account = pwd.getpwuid(os.getuid()).pw_name
    assert host["hostname"] == preflight.hostname()
    assert host["confinement"].startswith(f"unix-user:{account}: no session:")
    assert host["commit_policy"].startswith("no workspace:")
    assert host["writable_paths"] == []


def test_the_kernel_probe_changes_nothing_on_this_backend(tmp_path):
    assert capabilities.host_facts(Config(), probes=_permissive(tmp_path)) == (
        capabilities.host_facts(Config(), probes=_restricted(tmp_path))
    )


def test_no_host_fact_can_carry_the_webhook_url(tmp_path, monkeypatch):
    """The isolation rule this bridge is built around (issue #68): the URL is
    read exactly once, at POST time, by `webhook.resolve_webhook`. A
    capability surface is a document that leaves the host and lands in a
    ledger record — it must never become the place the secret escapes."""
    monkeypatch.setenv("CULTURE_NODES_WEBHOOK_URL", "https://discord.com/api/webhooks/1/s3cr3t")
    rendered = json.dumps(
        preflight.capability_block(capabilities.host_facts(Config(), probes=_permissive(tmp_path)))
    )
    assert "s3cr3t" not in rendered
    assert "discord" not in rendered.lower()


# --- how the surface leaves this host -----------------------------------


def _get(base_url, path, headers=None):
    req = urllib.request.Request(base_url + path, method="GET", headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


@pytest.fixture()
def bridge_url(tmp_path):
    cfg = Config(
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
    )
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    yield f"http://{host}:{port}", cfg
    srv.shutdown()
    srv.server_close()


def test_the_running_bridge_serves_its_measured_surface(bridge_url):
    base, _cfg = bridge_url
    status, body = _get(base, preflight.CAPABILITIES_PATH, {"Authorization": "Bearer s3cr3t"})
    assert status == 200
    preflight.validate_block(body)
    assert body["preflight"]["host"]["hostname"] == preflight.hostname()


def test_the_surface_is_authenticated_like_every_other_protocol_route(bridge_url):
    """It names a hostname. `/healthz` stays the only unauthenticated route
    on every bridge."""
    base, _cfg = bridge_url
    status, _body = _get(base, preflight.CAPABILITIES_PATH)
    assert status == 401


def test_print_capabilities_emits_the_registration_document(capsys):
    """The pre-start path: an operator registers the actor before the bridge
    has ever run, and must not have to hand-write facts about a host they
    are guessing at."""
    rc = main(["serve", "--print-capabilities"])
    assert rc == 0
    printed = json.loads(capsys.readouterr().out)
    preflight.validate_block(printed)


# --- the two keys this bridge deliberately does not carry (issue #96) ----


def test_no_toolchain_or_grant_facts_because_there_is_no_dispatch_to_have_them(tmp_path):
    """`toolchains: []` here would read as "this host has no uv" — a claim
    about a host nobody measured. This bridge starts no session, so there is
    no posture to grant anything and no tool a dispatch could invoke, and
    both keys are absent rather than empty.

    The sandbox keys are absent for the same reason, and this test states
    both together so the pair cannot drift apart."""
    host = capabilities.host_facts(Config())
    for key in ("dispatch_grants", "toolchains", "sandbox_modes", "sandbox_modes_unavailable"):
        assert key not in host
    # ...and what remains still says something: this is not an empty surface.
    assert host["hostname"]
    assert host["writable_paths"] == []
