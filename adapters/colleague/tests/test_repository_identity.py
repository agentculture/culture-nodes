"""Repository identity -> a checkout on THIS host (task t2, issue #125).

The control plane sends a NAME (`internal/actors.RepositoryIdentityKey`,
read from the actor's registration and from nowhere else); this bridge is
the only party that knows which directory on its own filesystem that name
means. These tests pin the four properties the task's acceptance criteria
name, plus the two that keep the change from breaking what already worked:
an explicit `input.repo` still wins, and a dispatch carrying no identity
still falls back to `only_allowed_repo()`.

The resolver itself is `repositories.py`, byte-identical in every bridge
that executes in a checkout, so the first half of this file is the same
test body in all three adapters -- only the import line differs.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request

import pytest
from colleague_bridge import colleague_cli, repositories, server
from colleague_bridge.config import Config, ConfigError

# --------------------------------------------------------------------------
# The resolver, on its own.
# --------------------------------------------------------------------------


def _checkouts(tmp_path, *names):
    """Make each named directory under *tmp_path* and return their paths."""
    made = []
    for name in names:
        path = tmp_path / name
        path.mkdir(parents=True, exist_ok=True)
        made.append(str(path))
    return made


def test_a_multi_entry_allowlist_resolves_the_identity_it_was_given(tmp_path):
    """Acceptance 1. Two permitted repositories, one identity, no ambiguity:
    cardinality stops being the signal, which is the whole point of #125 --
    an allowlist is a permission surface and is allowed to hold many
    entries."""
    wanted, other = _checkouts(tmp_path, "culture-nodes", "daria")
    cfg = Config(repo_allowlist=(wanted, other))

    resolution = repositories.resolve_for_input(cfg, {"repository_identity": "culture-nodes"})

    assert resolution.refusal is None
    assert resolution.repo == wanted
    # And the cardinality fallback would have refused this configuration.
    assert cfg.only_allowed_repo() is None


def test_an_owner_slug_identity_resolves_by_its_repository_segment(tmp_path):
    """api/actor-protocol/README.md's own example is a slug
    (`agentculture/culture-nodes`), so the repository segment is what a
    checkout directory is compared against."""
    wanted, _ = _checkouts(tmp_path, "culture-nodes", "daria")
    cfg = Config(repo_allowlist=(wanted,))

    resolution = repositories.resolve_for_input(
        cfg, {"repository_identity": "agentculture/culture-nodes"}
    )

    assert resolution.repo == wanted


def test_an_identity_naming_two_permitted_checkouts_is_refused_not_first_matched(tmp_path):
    """Acceptance 2 (claim c51). `repo_allowed` accepts an exact entry OR a
    strict child of a scoped prefix, so one name really can reach two
    permitted paths -- and picking the first would silently check out a lane
    nobody named."""
    lane_a, lane_b = _checkouts(tmp_path, "a/culture-nodes", "b/culture-nodes")
    cfg = Config(repo_allowlist=(lane_a, lane_b))

    resolution = repositories.resolve_for_input(cfg, {"repository_identity": "culture-nodes"})

    assert resolution.repo is None
    refusal = resolution.refusal
    assert refusal is not None
    assert refusal.status == 400
    assert refusal.name == repositories.ERROR_AMBIGUOUS
    assert refusal.hint
    # Both candidates are named, so the operator can see the collision.
    assert lane_a in refusal.body["error"]
    assert lane_b in refusal.body["error"]
    assert "hint:" in refusal.body["error"]


def test_an_identity_naming_nothing_permitted_is_refused_and_names_the_identity(tmp_path):
    """Acceptance 3. A miss is a named refusal carrying the identity -- the
    diagnostic that `input.repo is required` never gave anyone."""
    (permitted,) = _checkouts(tmp_path, "daria")
    cfg = Config(repo_allowlist=(permitted,))

    resolution = repositories.resolve_for_input(cfg, {"repository_identity": "culture-nodes"})

    assert resolution.repo is None
    refusal = resolution.refusal
    assert refusal is not None
    assert refusal.status == 400
    assert refusal.name == repositories.ERROR_UNKNOWN
    assert "culture-nodes" in refusal.body["error"]
    assert refusal.hint


def test_a_declared_identity_outside_the_allowlist_is_refused(tmp_path):
    """Acceptance 4. A declaration is the operator's own statement, and it is
    still not permission: the resolved path goes through `repo_allowed` like
    any other."""
    permitted, elsewhere = _checkouts(tmp_path, "permitted", "elsewhere")
    cfg = Config(repo_allowlist=(permitted,), repo_identities={"culture-nodes": elsewhere})

    resolution = repositories.resolve_for_input(cfg, {"repository_identity": "culture-nodes"})

    assert resolution.repo is None
    refusal = resolution.refusal
    assert refusal is not None
    assert refusal.status == 403
    assert refusal.name == repositories.ERROR_NOT_PERMITTED
    assert elsewhere in refusal.body["error"]


def test_a_declaration_wins_over_inference(tmp_path):
    """The escape hatch for a host whose checkout directories are not named
    after the repository -- spark's `.worktrees.culture-nodes/<lane>` lanes
    are exactly that shape."""
    inferred, declared = _checkouts(tmp_path, "culture-nodes", "upkeep-lane")
    cfg = Config(
        repo_allowlist=(inferred, declared),
        repo_identities={"culture-nodes": declared},
    )

    resolution = repositories.resolve_for_input(cfg, {"repository_identity": "culture-nodes"})

    assert resolution.repo == declared


def test_an_identity_resolves_under_a_scoped_prefix_root(tmp_path):
    """A prefix root is a directory this host mints worktrees into, and
    `repo_allowed` already permits a strict child of one. `<root>/<name>` is
    therefore a real candidate -- but only when it exists, or every identity
    would "resolve" to a directory that is not there and the miss would
    surface as a checkout failure deeper in."""
    root = tmp_path / "lanes"
    (root / "culture-nodes").mkdir(parents=True)
    cfg = Config(repo_allowlist_prefixes=(str(root),))

    resolution = repositories.resolve_for_input(cfg, {"repository_identity": "culture-nodes"})
    assert resolution.repo == str(root / "culture-nodes")

    missing = repositories.resolve_for_input(cfg, {"repository_identity": "not-checked-out"})
    assert missing.refusal is not None
    assert missing.refusal.name == repositories.ERROR_UNKNOWN


def test_an_allowlist_entry_and_a_prefix_child_of_one_name_collide(tmp_path):
    """The collision shape claim c51 actually describes: an exact allowlist
    entry and a strict child of a scoped prefix, both named `culture-nodes`."""
    (exact,) = _checkouts(tmp_path, "exact/culture-nodes")
    root = tmp_path / "lanes"
    (root / "culture-nodes").mkdir(parents=True)
    cfg = Config(repo_allowlist=(exact,), repo_allowlist_prefixes=(str(root),))

    resolution = repositories.resolve_for_input(cfg, {"repository_identity": "culture-nodes"})

    assert resolution.refusal is not None
    assert resolution.refusal.name == repositories.ERROR_AMBIGUOUS


def test_no_identity_leaves_the_only_allowed_repo_fallback_alone(tmp_path):
    """An actor whose registration declares no identity dispatches exactly as
    it did before this key existed -- the single-repo deployments that work
    today must keep working."""
    (only,) = _checkouts(tmp_path, "culture-nodes")
    cfg = Config(repo_allowlist=(only,))

    for absent in ({}, {"repository_identity": ""}, {"repository_identity": "   "}):
        resolution = repositories.resolve_for_input(cfg, absent)
        assert resolution.repo is None
        assert resolution.refusal is None
    assert cfg.only_allowed_repo() == only


def test_a_non_string_identity_is_refused(tmp_path):
    """The key comes from `actor.metadata`, which is operator-written JSON: a
    number or an object there is a registration mistake, and saying so beats
    resolving nothing and blaming the graph."""
    cfg = Config(repo_allowlist=tuple(_checkouts(tmp_path, "culture-nodes")))

    resolution = repositories.resolve_for_input(cfg, {"repository_identity": 7})

    assert resolution.refusal is not None
    assert resolution.refusal.status == 400
    assert resolution.refusal.name == repositories.ERROR_INVALID


def test_a_traversing_identity_cannot_reach_outside_the_permitted_surface(tmp_path):
    """`..` and an absolute path are refused as identities rather than joined
    onto a prefix root; even if one slipped through, `repo_allowed` is still
    the last word."""
    root = tmp_path / "lanes"
    (root / "culture-nodes").mkdir(parents=True)
    cfg = Config(repo_allowlist_prefixes=(str(root),))

    for hostile in ("..", ".", "/etc", "../culture-nodes/.."):
        resolution = repositories.resolve_for_input(cfg, {"repository_identity": hostile})
        assert resolution.repo is None, hostile
        assert resolution.refusal is not None, hostile
        assert resolution.refusal.name == repositories.ERROR_UNKNOWN, hostile


def test_the_input_key_is_the_one_the_control_plane_sends():
    """`repository_identity`, not `repo`. t1 kept them distinct on purpose:
    `repo` is a checkout PATH validated against the allowlist, an identity is
    a name. Merging them would make an identity indistinguishable from a path
    the moment a bridge read either."""
    assert repositories.INPUT_KEY == "repository_identity"


# --------------------------------------------------------------------------
# Config: where the declarations live.
# --------------------------------------------------------------------------


def test_repo_identities_loads_from_a_config_file_and_from_the_env(tmp_path):
    config_file = tmp_path / "bridge.json"
    config_file.write_text(
        json.dumps({"repo_identities": {"culture-nodes": "/srv/git/culture-nodes"}}),
        encoding="utf-8",
    )

    cfg = Config.load(str(config_file), env={})
    assert cfg.repo_identities == {"culture-nodes": "/srv/git/culture-nodes"}

    cfg = Config.load(
        str(config_file),
        env={"COLLEAGUE_BRIDGE_REPO_IDENTITIES": "daria=/srv/git/daria"},
    )
    assert cfg.repo_identities == {"daria": "/srv/git/daria"}

    with pytest.raises(ConfigError):
        Config.load(env={"COLLEAGUE_BRIDGE_REPO_IDENTITIES": "no-equals-sign"})


# --------------------------------------------------------------------------
# The wiring: a real dispatch over loopback.
# --------------------------------------------------------------------------


def _request(base_url, path, *, method="POST", body=None, headers=None):
    url = base_url + path
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:  # nosec B310 - fixed http scheme
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


@pytest.fixture()
def multi_repo_bridge(tmp_path):
    """A bridge whose allowlist holds TWO repositories -- the deployed shape
    that made every triggered pr-upkeep run fail closed (#125)."""
    wanted, other = _checkouts(tmp_path, "culture-nodes", "daria")
    cfg = Config(
        repo_allowlist=(wanted, other),
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
        sync_max_steps=6,
        default_max_steps=6,
    )
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    yield f"http://{host}:{port}", cfg, wanted, other
    srv.shutdown()
    srv.server_close()


def _invocation_body(**input_overrides):
    input_payload = {"instruction": "say hello"}
    input_payload.update(input_overrides)
    return {
        "protocol_version": "1.0",
        "run_id": "run_1",
        "token_id": "tok_1",
        "node_run_id": "nr_1",
        "attempt_id": "att_1",
        "attempt": 1,
        "workflow": {"name": "wf", "version_digest": "sha256:0"},
        "node": {"id": "n1", "contract_digest": "sha256:1"},
        "input": input_payload,
        "artifact_refs": [],
        "context_refs": [],
        "callback": {"url": "http://127.0.0.1:1/callback", "token": "cbtok"},
    }


def _capture_run_sync(monkeypatch, captured):
    def fake_run_sync(cfg_, instruction, repo_, **kwargs):
        captured["repo"] = repo_
        captured["instruction"] = instruction
        return colleague_cli.SyncRunResult(
            exit_code=0,
            stdout="",
            stderr="",
            task_result={
                "task_id": "abc",
                "status": "ok",
                "summary": "did it",
                "changed_files": [],
                "artifacts_path": "/x/.colleague/abc.json",
                "usage": {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
                "error": None,
            },
            timed_out=False,
        )

    monkeypatch.setattr(colleague_cli, "run_sync", fake_run_sync)


def test_a_dispatch_with_a_multi_entry_allowlist_reaches_the_identified_checkout(
    multi_repo_bridge, monkeypatch
):
    """Acceptance 1, end to end: no `input.repo`, a two-entry allowlist, and
    the session still starts in the right directory."""
    base, cfg, wanted, _other = multi_repo_bridge
    captured = {}
    _capture_run_sync(monkeypatch, captured)

    status, body = _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation_body(repository_identity="agentculture/culture-nodes"),
        headers={"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_identity"},
    )

    assert status == 200, body
    assert captured["repo"] == wanted


def test_the_repository_identity_is_transport_not_a_bound_input(multi_repo_bridge, monkeypatch):
    """It is an addressing field, so it must not be appended to the prompt as
    an engine-resolved "Bound input" the way a node's real bindings are."""
    base, cfg, _wanted, _other = multi_repo_bridge
    captured = {}
    _capture_run_sync(monkeypatch, captured)

    _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation_body(
            repository_identity="culture-nodes", fixReport={"summary": "did the thing"}
        ),
        headers={"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_transport"},
    )

    assert "repository_identity" not in captured["instruction"]
    # ...while a genuine binding still is forwarded.
    assert "fixReport" in captured["instruction"]


def test_an_ambiguous_identity_is_a_400_with_a_named_error_and_a_hint(tmp_path, monkeypatch):
    lane_a, lane_b = _checkouts(tmp_path, "a/culture-nodes", "b/culture-nodes")
    cfg = Config(
        repo_allowlist=(lane_a, lane_b),
        state_dir=str(tmp_path / "state"),
        host="127.0.0.1",
        port=0,
        auth_token="s3cr3t",
    )
    srv, _thread = server.start_background(cfg)
    host, port = srv.server_address
    try:
        status, body = _request(
            f"http://{host}:{port}",
            server.INVOCATIONS_PATH,
            body=_invocation_body(repository_identity="culture-nodes"),
            headers={"Authorization": "Bearer s3cr3t", "Idempotency-Key": "att_ambiguous"},
        )
    finally:
        srv.shutdown()
        srv.server_close()

    assert status == 400
    assert body["class"] == "actor_rejected_input"
    assert body["error"].startswith(repositories.ERROR_AMBIGUOUS)
    assert body["hint"]


def test_an_unknown_identity_is_a_400_naming_the_identity(multi_repo_bridge):
    base, cfg, _wanted, _other = multi_repo_bridge

    status, body = _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation_body(repository_identity="some-other-repo"),
        headers={"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_unknown"},
    )

    assert status == 400
    assert body["error"].startswith(repositories.ERROR_UNKNOWN)
    assert "some-other-repo" in body["error"]


def test_an_explicit_input_repo_still_wins_over_the_identity(multi_repo_bridge, monkeypatch):
    """`input.repo` is a checkout a workflow author bound deliberately. The
    identity answers "which repository is this actor's lane", which is only a
    question when nobody answered it."""
    base, cfg, _wanted, other = multi_repo_bridge
    captured = {}
    _capture_run_sync(monkeypatch, captured)

    status, body = _request(
        base,
        server.INVOCATIONS_PATH,
        body=_invocation_body(repo=other, repository_identity="culture-nodes"),
        headers={"Authorization": f"Bearer {cfg.auth_token}", "Idempotency-Key": "att_explicit"},
    )

    assert status == 200, body
    assert captured["repo"] == other
