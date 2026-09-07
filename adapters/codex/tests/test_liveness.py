"""Bridge-side lane liveness (issue #308 detector, plan
loop-closure-claude-codex task t9) — codex's half.

Three things are asserted here, and the acceptance criteria name all three:

* CHECK mode: a dry read-only `codex exec` probe against a fake whose
  refresh token is spent yields `session_ok=false reason=refresh_token_spent`
  within ONE probe; a healthy fake yields `session_ok=true`.
* LOCK mode: the first run whose output carries the spent-token text flips
  the lane to `session_ok=false`, and it STAYS flipped across a re-probe of
  the capability surface until something clears it.
* The execution-failure path exposes a distinct outcome class
  (`credential_spent`) so the control plane can read it, and every existing
  class keeps its meaning.

The spent-token texts are the two observed live (harness-hardening wave 0,
deviation d1; and the later "revoked" wording), pinned verbatim.
"""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.request

import pytest
from codex_bridge import capabilities, codex_cli, liveness, mapping, preflight, server
from codex_bridge.config import Config, ConfigError

REVOKED = (
    "Your access token could not be refreshed because your refresh token was revoked. "
    "Please log out and sign in again."
)
ALREADY_USED = (
    "Your access token could not be refreshed because your refresh token was already used. "
    "Please log out and sign in again."
)


# --- the shared module's own contract -------------------------------------


def test_the_liveness_fact_carries_exactly_the_four_agreed_keys():
    fact = liveness.liveness_fact(
        session_ok=True, reason=liveness.REASON_OK, mode=liveness.MODE_CHECK
    )
    assert tuple(fact) == liveness.LIVENESS_KEYS == ("session_ok", "reason", "checked_at", "mode")
    assert fact["session_ok"] is True
    assert fact["mode"] == "CHECK"
    assert fact["checked_at"].endswith("+00:00")


def test_a_mode_outside_lock_and_check_is_refused():
    with pytest.raises(liveness.LivenessError):
        liveness.liveness_fact(session_ok=True, reason="ok", mode="MAYBE")
    assert liveness.parse_mode("lock") == "LOCK"
    assert liveness.parse_mode(" check ") == "CHECK"
    with pytest.raises(liveness.LivenessError):
        liveness.parse_mode("")


def test_an_unagreed_reason_is_refused_rather_than_carried():
    with pytest.raises(liveness.LivenessError):
        liveness.liveness_fact(session_ok=False, reason="broke", mode="LOCK")


@pytest.mark.parametrize("text", [REVOKED, ALREADY_USED])
def test_both_observed_spent_token_texts_are_recognised(text):
    assert liveness.credential_spent(text)
    assert liveness.credential_spent("", None, "noise\n" + text + "\n")


def test_ordinary_failure_text_is_not_a_spent_credential():
    assert not liveness.credential_spent("rate limit exceeded")
    assert not liveness.credential_spent("session not found")
    assert not liveness.credential_spent()


def test_an_unmeasured_fact_is_neither_true_nor_false():
    fact = liveness.unmeasured(liveness.MODE_LOCK)
    assert fact["session_ok"] is None
    assert fact["reason"] == liveness.REASON_UNMEASURED


def test_validate_liveness_refuses_the_shapes_the_engine_should_never_see():
    good = liveness.liveness_fact(session_ok=False, reason="refresh_token_spent", mode="LOCK")
    liveness.validate_liveness(good)
    for bad in (
        None,
        [],
        {**good, "session_ok": "yes"},
        {**good, "mode": "lock"},
        {**good, "extra": 1},
        {k: v for k, v in good.items() if k != "checked_at"},
    ):
        with pytest.raises(liveness.LivenessError):
            liveness.validate_liveness(bad)


def test_the_lock_state_starts_unmeasured_flips_on_spent_text_and_holds_until_cleared():
    state = liveness.LivenessState(liveness.MODE_LOCK)
    assert state.fact()["session_ok"] is None
    assert state.observe_text("some other stderr") is False
    assert state.fact()["session_ok"] is None

    assert state.observe_text("", REVOKED) is True
    assert state.locked
    fact = state.fact()
    assert fact["session_ok"] is False
    assert fact["reason"] == liveness.REASON_REFRESH_TOKEN_SPENT
    assert fact["mode"] == "LOCK"

    # A healthy-looking observation does not unlock: only a clear does.
    state.observe_text("OK")
    assert state.locked

    state.clear()
    assert not state.locked
    assert state.fact()["reason"] == liveness.REASON_UNMEASURED


def test_freshness_is_measured_from_the_last_record():
    state = liveness.LivenessState(liveness.MODE_CHECK)
    assert not state.fresh(60.0), "an unmeasured lane is never fresh"
    state.record(liveness.liveness_fact(session_ok=True, reason="ok", mode="CHECK"))
    assert state.fresh(60.0)
    assert not state.fresh(0.0)


def test_read_json_int_never_raises(tmp_path):
    assert liveness.read_json_int(tmp_path / "absent.json", "a", "b") is None
    (tmp_path / "bad.json").write_text("{not json", encoding="utf-8")
    assert liveness.read_json_int(tmp_path / "bad.json", "a") is None
    (tmp_path / "ok.json").write_text(json.dumps({"a": {"b": 1700000000000}}), encoding="utf-8")
    assert liveness.read_json_int(tmp_path / "ok.json", "a", "b") == 1700000000000
    assert liveness.read_json_int(tmp_path / "ok.json", "a", "missing") is None
    (tmp_path / "str.json").write_text(json.dumps({"a": "17"}), encoding="utf-8")
    assert liveness.read_json_int(tmp_path / "str.json", "a") is None


def test_from_expiry_reads_a_millisecond_epoch():
    now = 1_800_000_000.0
    past = liveness.from_expiry(int((now - 60) * 1000), mode="LOCK", now=now)
    assert past["session_ok"] is False
    assert past["reason"] == liveness.REASON_CREDENTIAL_EXPIRED
    future = liveness.from_expiry(int((now + 3600) * 1000), mode="LOCK", now=now)
    assert future["session_ok"] is True
    assert future["reason"] == liveness.REASON_OK
    assert liveness.from_expiry(None, mode="LOCK", now=now)["reason"] == "unmeasured"


# --- the preflight surface carries it --------------------------------------


def test_liveness_is_an_agreed_host_key_and_rides_the_surface():
    assert "liveness" in preflight.HOST_KEYS
    fact = liveness.liveness_fact(session_ok=False, reason="refresh_token_spent", mode="LOCK")
    host = preflight.host_block(hostname="h", commit_policy="none", liveness=fact)
    assert host["liveness"] == fact
    preflight.validate_block(preflight.capability_block(host))


def test_a_bridge_that_measures_no_liveness_omits_the_key():
    host = preflight.host_block(hostname="h", commit_policy="none")
    assert "liveness" not in host


# --- CHECK: the dry probe against the fake ---------------------------------


def _cfg(fake_codex, tmp_path, **overrides):
    base = dict(
        codex_bin=str(fake_codex),
        repo_allowlist=(str(tmp_path),),
        state_dir=str(tmp_path / "state"),
        liveness_probe_timeout_seconds=20.0,
    )
    base.update(overrides)
    return Config(**base)


def test_the_probe_argv_is_a_dry_read_only_exec_with_a_one_line_instruction(tmp_path):
    argv = codex_cli.liveness_probe_argv(str(tmp_path))
    assert argv[:2] == ["exec", "--json"]
    assert argv[argv.index("--sandbox") + 1] == "read-only"
    assert "--skip-git-repo-check" in argv
    assert argv[argv.index("-C") + 1] == str(tmp_path)
    assert argv[-1] == codex_cli.LIVENESS_PROBE_INSTRUCTION
    assert "\n" not in codex_cli.LIVENESS_PROBE_INSTRUCTION


def test_check_probe_reports_a_spent_refresh_token_within_one_probe(
    fake_codex, tmp_path, monkeypatch
):
    monkeypatch.setenv("FAKE_CODEX_SPENT_TOKEN", "1")
    started = time.monotonic()
    fact = codex_cli.liveness_probe(_cfg(fake_codex, tmp_path, liveness_mode="CHECK"))
    assert time.monotonic() - started < 20.0
    assert fact["session_ok"] is False
    assert fact["reason"] == liveness.REASON_REFRESH_TOKEN_SPENT
    assert fact["mode"] == "CHECK"


def test_check_probe_recognises_the_earlier_already_used_wording(fake_codex, tmp_path, monkeypatch):
    monkeypatch.setenv("FAKE_CODEX_SPENT_TOKEN", "already_used")
    fact = codex_cli.liveness_probe(_cfg(fake_codex, tmp_path, liveness_mode="CHECK"))
    assert fact["session_ok"] is False
    assert fact["reason"] == liveness.REASON_REFRESH_TOKEN_SPENT


def test_check_probe_reports_a_healthy_lane_as_session_ok(fake_codex, tmp_path, monkeypatch):
    monkeypatch.delenv("FAKE_CODEX_SPENT_TOKEN", raising=False)
    monkeypatch.setenv("FAKE_CODEX_BEHAVIOR", "ok")
    fact = codex_cli.liveness_probe(_cfg(fake_codex, tmp_path, liveness_mode="CHECK"))
    assert fact["session_ok"] is True
    assert fact["reason"] == liveness.REASON_OK


def test_check_probe_that_fails_for_another_reason_is_not_a_spent_credential(
    fake_codex, tmp_path, monkeypatch
):
    monkeypatch.delenv("FAKE_CODEX_SPENT_TOKEN", raising=False)
    monkeypatch.setenv("FAKE_CODEX_BEHAVIOR", "error")
    fact = codex_cli.liveness_probe(_cfg(fake_codex, tmp_path, liveness_mode="CHECK"))
    assert fact["session_ok"] is None
    assert fact["reason"] == liveness.REASON_PROBE_FAILED


def test_check_probe_is_bounded_and_a_timeout_is_not_a_verdict(fake_codex, tmp_path, monkeypatch):
    monkeypatch.delenv("FAKE_CODEX_SPENT_TOKEN", raising=False)
    monkeypatch.setenv("FAKE_CODEX_BEHAVIOR", "hang_then_clean_exit_zero_on_sigterm")
    cfg = _cfg(fake_codex, tmp_path, liveness_mode="CHECK", liveness_probe_timeout_seconds=1.0)
    started = time.monotonic()
    fact = codex_cli.liveness_probe(cfg)
    assert time.monotonic() - started < 10.0
    assert fact["session_ok"] is None
    assert fact["reason"] == liveness.REASON_PROBE_TIMEOUT


def test_a_missing_codex_binary_is_probe_failed_never_a_crash(tmp_path):
    cfg = Config(codex_bin=str(tmp_path / "no-such-codex"), liveness_mode="CHECK")
    fact = codex_cli.liveness_probe(cfg)
    assert fact["session_ok"] is None
    assert fact["reason"] == liveness.REASON_PROBE_FAILED


def test_check_mode_surface_runs_the_probe_and_caches_it_for_the_ttl(tmp_path):
    calls = []

    def probe():
        calls.append(1)
        return liveness.liveness_fact(session_ok=True, reason="ok", mode="CHECK")

    cfg = Config(liveness_mode="CHECK", liveness_check_ttl_seconds=3600.0)
    state = liveness.LivenessState("CHECK")
    first = capabilities.host_facts(cfg, liveness_state=state, liveness_probe=probe)["liveness"]
    second = capabilities.host_facts(cfg, liveness_state=state, liveness_probe=probe)["liveness"]
    assert first["session_ok"] is True and second == first
    assert len(calls) == 1, "a fresh fact is served from the cache, not re-billed"

    cfg.liveness_check_ttl_seconds = 0.0
    capabilities.host_facts(cfg, liveness_state=state, liveness_probe=probe)
    assert len(calls) == 2


def test_lock_mode_surface_never_runs_the_probe(tmp_path):
    def probe():
        raise AssertionError("LOCK mode must not spend a session on a probe")

    cfg = Config(liveness_mode="LOCK")
    fact = capabilities.host_facts(cfg, liveness_probe=probe)["liveness"]
    assert fact["mode"] == "LOCK"
    assert fact["session_ok"] is None


def test_the_surface_with_liveness_is_still_a_document_the_engine_accepts(tmp_path):
    cfg = Config(repo_allowlist=(str(tmp_path),))
    state = liveness.LivenessState("LOCK")
    state.observe_text(REVOKED)
    block = preflight.capability_block(capabilities.host_facts(cfg, liveness_state=state))
    preflight.validate_block(block)
    assert block["preflight"]["host"]["liveness"]["session_ok"] is False


# --- the outcome class the control plane reads -----------------------------


@pytest.mark.parametrize("text", [REVOKED, ALREADY_USED])
def test_a_spent_credential_turn_failure_has_its_own_class(text):
    c = mapping.classify(
        {"status": "error", "error": text},
        mapping.InvocationContext(),
        default_success_outcome="completed",
    )
    assert c.domain is False
    assert c.error_class == mapping.CLASS_CREDENTIAL_SPENT == "credential_spent"
    assert text in c.message


def test_the_existing_classes_keep_their_meaning():
    ctx = mapping.InvocationContext()
    assert (
        mapping.classify(
            {"status": "error", "error": "fake failure"}, ctx, default_success_outcome="completed"
        ).error_class
        == mapping.CLASS_EXECUTION
    )
    assert (
        mapping.classify(
            {"status": "error", "error": "rate limit"}, ctx, default_success_outcome="completed"
        ).error_class
        == mapping.CLASS_CAPACITY_EXHAUSTED
    )
    assert (
        mapping.classify(
            {"status": "incomplete"}, ctx, default_success_outcome="completed"
        ).error_class
        == mapping.CLASS_EXECUTION
    )
    assert (
        mapping.classify(None, ctx, default_success_outcome="completed").error_class
        == mapping.CLASS_EXECUTION
    )
    assert {
        mapping.CLASS_EXECUTION,
        mapping.CLASS_TIMEOUT,
        mapping.CLASS_ACTOR_REJECTED_INPUT,
        mapping.CLASS_CAPACITY_EXHAUSTED,
        mapping.CLASS_CREDENTIAL_SPENT,
    } == {"execution", "timeout", "actor_rejected_input", "capacity_exhausted", "credential_spent"}


def test_a_refusal_printed_only_to_stderr_becomes_a_credential_spent_task_result():
    """The live failure wrote the text to stderr and no terminal event: on
    its own `parse_session` says `None`, which mapping would report as a
    generic execution failure. The refusal is attached before mapping sees
    it, so the class survives."""
    result = codex_cli.SyncRunResult(
        exit_code=1, stdout="", stderr=REVOKED + "\n", task_result=None, timed_out=False
    )
    refused = codex_cli.with_credential_refusal(result)
    assert refused.task_result["status"] == "error"
    assert REVOKED in refused.task_result["error"]
    c = mapping.classify(
        refused.task_result, mapping.InvocationContext(), default_success_outcome="completed"
    )
    assert c.error_class == mapping.CLASS_CREDENTIAL_SPENT


def test_an_ok_session_is_never_rewritten_into_a_refusal():
    result = codex_cli.SyncRunResult(
        exit_code=0,
        stdout="",
        stderr=REVOKED,  # a stale warning next to a completed turn
        task_result={"status": "ok", "summary": "OK", "error": None},
        timed_out=False,
    )
    assert codex_cli.with_credential_refusal(result) is result


def test_with_credential_refusal_locks_the_lane_it_is_handed():
    state = liveness.LivenessState("LOCK")
    result = codex_cli.SyncRunResult(
        exit_code=1, stdout="", stderr=ALREADY_USED, task_result=None, timed_out=False
    )
    codex_cli.with_credential_refusal(result, liveness_state=state)
    assert state.locked
    assert state.fact()["reason"] == liveness.REASON_REFRESH_TOKEN_SPENT


# --- LOCK through a real dispatch on the running bridge ---------------------


def _request(base, path, *, method="POST", body=None, headers=None):
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(base + path, data=data, method=method, headers=headers or {})
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


def _invocation(repo):
    return {
        "protocol_version": "1.0",
        "run_id": "run_liveness",
        "token_id": "tok_1",
        "node_run_id": "nr_1",
        "attempt_id": "att_1",
        "attempt": 1,
        "workflow": {"name": "wf", "version_digest": "sha256:0"},
        "node": {"id": "n1", "contract_digest": "sha256:1"},
        "input": {"instruction": "say hello", "repo": repo},
        "artifact_refs": [],
        "context_refs": [],
        "callback": {"url": "http://127.0.0.1:1/callback", "token": "cbtok"},
    }


@pytest.fixture()
def locked_lane_bridge(fake_codex, tmp_path, monkeypatch):
    monkeypatch.setenv("FAKE_CODEX_SPENT_TOKEN", "1")
    cfg = _cfg(
        fake_codex,
        tmp_path,
        liveness_mode="LOCK",
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
        preserve_on_failure=False,
    )
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    yield f"http://{host}:{port}", cfg, srv.bridge, tmp_path
    srv.shutdown()
    srv.server_close()


def test_lock_flips_after_one_run_and_holds_across_a_reprobe_until_cleared(locked_lane_bridge):
    base, cfg, bridge, repo = locked_lane_bridge
    auth = {"Authorization": f"Bearer {cfg.auth_token}"}

    status, before = _request(base, preflight.CAPABILITIES_PATH, method="GET", headers=auth)
    assert status == 200
    assert before["preflight"]["host"]["liveness"]["session_ok"] is None

    status, body = _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation(str(repo)),
        headers={**auth, "Idempotency-Key": "att_spent"},
    )
    assert status == 500
    assert body["class"] == "credential_spent"
    assert body.get("outcome") != "completed"

    for _ in range(2):  # a re-probe of the surface does not unlock a LOCK lane
        status, after = _request(base, preflight.CAPABILITIES_PATH, method="GET", headers=auth)
        fact = after["preflight"]["host"]["liveness"]
        assert fact["session_ok"] is False
        assert fact["reason"] == "refresh_token_spent"
        assert fact["mode"] == "LOCK"

    bridge.liveness.clear()
    status, cleared = _request(base, preflight.CAPABILITIES_PATH, method="GET", headers=auth)
    assert cleared["preflight"]["host"]["liveness"]["session_ok"] is None


def test_a_bridge_rederives_its_half_at_start(fake_codex, tmp_path, monkeypatch):
    """Whatever a previous process knew is gone with it; the new one probes
    once, in either mode, and starts from what it measured."""
    monkeypatch.setenv("FAKE_CODEX_SPENT_TOKEN", "1")
    bridge = server.Bridge(_cfg(fake_codex, tmp_path, liveness_mode="LOCK"))
    assert bridge.liveness.fact()["session_ok"] is None
    bridge.rederive_liveness()
    assert bridge.liveness.fact()["session_ok"] is False
    assert bridge.liveness.fact()["reason"] == "refresh_token_spent"

    monkeypatch.delenv("FAKE_CODEX_SPENT_TOKEN")
    monkeypatch.setenv("FAKE_CODEX_BEHAVIOR", "ok")
    bridge.rederive_liveness()
    assert bridge.liveness.fact()["session_ok"] is True
    assert not bridge.liveness.locked


def test_the_async_runner_locks_the_lane_it_shares_with_the_bridge(fake_codex, tmp_path):
    bridge = server.Bridge(_cfg(fake_codex, tmp_path))
    assert bridge.async_runner.liveness_state is bridge.liveness


# --- configuration ---------------------------------------------------------


def test_liveness_config_defaults_to_lock_with_the_spec_bounds():
    cfg = Config.load(env={})
    assert cfg.liveness_mode == "LOCK"
    assert cfg.liveness_probe_timeout_seconds == 20.0
    assert cfg.liveness_check_ttl_seconds == 120.0


def test_liveness_mode_is_configurable_per_lane_from_env_and_file(tmp_path):
    cfg = Config.load(
        env={
            "CODEX_BRIDGE_LIVENESS_MODE": "check",
            "CODEX_BRIDGE_LIVENESS_PROBE_TIMEOUT_SECONDS": "5",
            "CODEX_BRIDGE_LIVENESS_CHECK_TTL_SECONDS": "30",
        }
    )
    assert cfg.liveness_mode == "CHECK"
    assert cfg.liveness_probe_timeout_seconds == 5.0
    assert cfg.liveness_check_ttl_seconds == 30.0

    path = tmp_path / "bridge.json"
    path.write_text(json.dumps({"liveness_mode": "LOCK"}), encoding="utf-8")
    assert Config.load(str(path), env={}).liveness_mode == "LOCK"


def test_an_unknown_liveness_mode_is_a_config_error_at_load():
    with pytest.raises(ConfigError):
        Config.load(env={"CODEX_BRIDGE_LIVENESS_MODE": "maybe"})
