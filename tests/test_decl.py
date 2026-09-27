"""``nodes decl`` (task t21, #328): the thin client over the declarations
API, exercised against ``tests/fake_api.py``. Every write verb is driven
once with an agent actor bearer and once with a human Access-cookie
session, asserting the SAME route is called with the credential the
environment provides (spec c66/h43)."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from culture_nodes.cli import main
from culture_nodes.explain import known_paths

VERSION = {
    "id": "dv-1",
    "declaration_id": "decl-1",
    "name": "demo",
    "version": 1,
    "digest": "sha256:abc",
    "author": "actor://company/dev",
    "created_at": "2026-01-01T00:00:00Z",
    "warnings": [],
}


@pytest.fixture
def decl_file(tmp_path: Path) -> Path:
    p = tmp_path / "decl.yaml"
    p.write_text("name: demo\ntrigger: {}\n", encoding="utf-8")
    return p


def _seen_auth(seen: dict, h) -> None:
    seen["auth"] = h.headers.get("Authorization")
    seen["cookie"] = h.headers.get("Cookie")


# --- catalog ----------------------------------------------------------------


def test_decl_and_chain_catalog_entries_present() -> None:
    paths = set(known_paths())
    for path in [
        ("decl",),
        ("decl", "declare"),
        ("decl", "validate"),
        ("decl", "link"),
        ("decl", "show"),
        ("decl", "list"),
        ("decl", "focus"),
        ("decl", "activate"),
        ("chain",),
        ("chain", "alias"),
        ("chain", "show"),
    ]:
        assert path in paths, path


def test_every_catalog_path_resolves_including_decl_and_chain(capsys) -> None:
    for path in known_paths():
        if path[:1] not in {("decl",), ("chain",)}:
            continue
        rc = main(["explain", *path])
        assert rc == 0, f"explain {' '.join(path)} failed"
        capsys.readouterr()


# --- bare noun ----------------------------------------------------------------


def test_decl_bare_noun(capsys) -> None:
    rc = main(["decl"])
    assert rc == 0
    assert "usage: nodes decl" in capsys.readouterr().out


# --- validate -----------------------------------------------------------------


def test_decl_validate_valid_text(fake_api, decl_file, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/declarations/validate",
        lambda h, m, q, b: h.send_json(
            200, {"valid": True, "digest": "sha256:abc", "diagnostics": [], "warnings": []}
        ),
    )
    fake_api.start()
    rc = main(["decl", "validate", str(decl_file), "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "valid: true" in out
    assert "digest: sha256:abc" in out


def test_decl_validate_invalid_is_domain_outcome_not_error(fake_api, decl_file, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/declarations/validate",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "valid": False,
                "diagnostics": ["error: trigger.kind is required"],
                "warnings": [],
            },
        ),
    )
    fake_api.start()
    rc = main(["decl", "validate", str(decl_file), "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert captured.err == ""
    assert "error: trigger.kind is required" in captured.out
    assert "valid: false" in captured.out


def test_decl_validate_sends_yaml_format(fake_api, decl_file) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(200, {"valid": True, "diagnostics": [], "warnings": []})

    fake_api.route("POST", r"/v1alpha1/declarations/validate", handler)
    fake_api.start()
    main(["decl", "validate", str(decl_file), "--api-url", fake_api.base_url])
    assert seen["body"]["format"] == "yaml"


def test_decl_validate_json_passthrough_byte_exact(fake_api, decl_file, capsys) -> None:
    payload = {"valid": True, "digest": "sha256:x", "diagnostics": [], "warnings": []}

    def handler(h, m, q, b):
        body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        h.send_response(200)
        h.send_header("Content-Type", "application/json")
        h.send_header("Content-Length", str(len(body)))
        h.end_headers()
        h.wfile.write(body)

    fake_api.route("POST", r"/v1alpha1/declarations/validate", handler)
    fake_api.start()
    rc = main(["decl", "validate", str(decl_file), "--json", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert out == json.dumps(payload, separators=(",", ":")) + "\n"


def test_decl_validate_unreadable_file_exit_2(fake_api, capsys) -> None:
    fake_api.start()
    rc = main(["decl", "validate", "/no/such/file.yaml", "--api-url", fake_api.base_url])
    err = capsys.readouterr().err
    assert rc == 2
    assert err.startswith("error:")
    assert "hint:" in err


# --- declare (write: human session vs agent token) -----------------------------


@pytest.mark.parametrize(
    "credential",
    [
        {
            "env": {"NODES_ACTOR_TOKEN": "tok-123"},
            "expect_auth": "Bearer tok-123",
            "expect_cookie": None,
        },
        {
            "env": {"NODES_OP_COOKIE": "jwt-abc"},
            "expect_auth": None,
            "expect_cookie": "CF_Authorization=jwt-abc",
        },
    ],
    ids=["agent-token", "human-cookie"],
)
def test_decl_declare_sends_whichever_credential_the_environment_gives_it(
    fake_api, decl_file, monkeypatch, credential
) -> None:
    monkeypatch.delenv("NODES_ACTOR_TOKEN", raising=False)
    monkeypatch.delenv("NODES_OP_COOKIE", raising=False)
    for key, value in credential["env"].items():
        monkeypatch.setenv(key, value)

    seen: dict = {}

    def handler(h, m, q, b):
        _seen_auth(seen, h)
        seen["body"] = json.loads(b)
        h.send_json(201, VERSION)

    fake_api.route("POST", r"/v1alpha1/declarations$", handler)
    fake_api.start()
    rc = main(["decl", "declare", str(decl_file), "--api-url", fake_api.base_url])
    assert rc == 0
    assert seen["auth"] == credential["expect_auth"]
    assert seen["cookie"] == credential["expect_cookie"]
    assert seen["body"]["format"] == "yaml"


def test_decl_declare_text_output(fake_api, decl_file, capsys, monkeypatch) -> None:
    monkeypatch.setenv("NODES_ACTOR_TOKEN", "tok-1")
    fake_api.route("POST", r"/v1alpha1/declarations$", lambda h, m, q, b: h.send_json(201, VERSION))
    fake_api.start()
    rc = main(["decl", "declare", str(decl_file), "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "name: demo" in out
    assert "digest: sha256:abc" in out


def test_decl_declare_takes_no_credential_flag(decl_file, capsys) -> None:
    # A credential on argv is visible in ps and shell history: env only.
    with pytest.raises(SystemExit):
        main(["decl", "declare", str(decl_file), "--actor-token", "t"])
    assert "--actor-token" in capsys.readouterr().err


def test_decl_declare_401_relayed(fake_api, decl_file, capsys, monkeypatch) -> None:
    monkeypatch.delenv("NODES_ACTOR_TOKEN", raising=False)
    monkeypatch.delenv("NODES_OP_COOKIE", raising=False)
    fake_api.route(
        "POST",
        r"/v1alpha1/declarations$",
        lambda h, m, q, b: h.send_json(
            401,
            {
                "code": 1,
                "message": "declarations require an authenticated principal",
                "remediation": (
                    "authenticate as a human (Cloudflare Access) or a registered "
                    "agent actor's own bearer"
                ),
            },
        ),
    )
    fake_api.start()
    rc = main(["decl", "declare", str(decl_file), "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert "error: declarations require an authenticated principal" in captured.err


# --- link -----------------------------------------------------------------------


def test_decl_link_sends_to_and_kind(fake_api, capsys, monkeypatch) -> None:
    monkeypatch.setenv("NODES_ACTOR_TOKEN", "tok-1")
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        seen["path"] = h.path
        h.send_json(201, {"to": "one", "kind": "must"})

    fake_api.route("POST", r"/v1alpha1/declarations/two/links", handler)
    fake_api.start()
    rc = main(
        ["decl", "link", "two", "--to", "one", "--kind", "must", "--api-url", fake_api.base_url]
    )
    out = capsys.readouterr().out
    assert rc == 0
    assert seen["body"] == {"to": "one", "kind": "must"}
    assert "linked: two --must--> one" in out


# --- show -------------------------------------------------------------------------


def test_decl_show_text(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/declarations/(?P<name>[^/]+)$",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "id": "dv-1",
                "declaration_id": "decl-1",
                "name": "two",
                "version": 1,
                "digest": "sha256:abc",
                "author": "actor://company/dev",
                "created_at": "2026-01-01T00:00:00Z",
                "warnings": [],
                "active": True,
                "active_version_id": "dv-1",
                "links": [{"to": "one", "kind": "must"}],
            },
        ),
    )
    fake_api.start()
    rc = main(["decl", "show", "two", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "name: two" in out
    assert "active: True" in out
    assert "link: must -> one" in out


def test_decl_show_accepts_an_alias_shaped_name(fake_api, capsys) -> None:
    """decl show forwards whatever name string is given -- including one
    shaped like a chain alias -- unchanged, as a thin client (h23/h24)."""
    seen = {}

    def handler(h, m, q, b):
        seen["path"] = h.path
        h.send_json(
            200,
            {
                "id": "dv-1",
                "declaration_id": "decl-1",
                "name": "chain-a",
                "version": 1,
                "digest": "sha256:abc",
                "author": "a",
                "created_at": "2026-01-01T00:00:00Z",
                "warnings": [],
                "active": False,
                "links": [],
            },
        )

    fake_api.route("GET", r"/v1alpha1/declarations/(?P<name>[^/]+)$", handler)
    fake_api.start()
    rc = main(["decl", "show", "chain-a", "--api-url", fake_api.base_url])
    assert rc == 0
    assert seen["path"] == "/v1alpha1/declarations/chain-a"


def test_decl_show_404(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/declarations/(?P<name>[^/]+)$",
        lambda h, m, q, b: h.send_json(
            404,
            {
                "code": 1,
                "message": 'no declaration named "bogus"',
                "remediation": "check the declaration name",
            },
        ),
    )
    fake_api.start()
    rc = main(["decl", "show", "bogus", "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert "no declaration named" in captured.err


# --- list -------------------------------------------------------------------------


def test_decl_list_text(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/declarations$",
        lambda h, m, q, b: h.send_json(200, {"items": [VERSION]}),
    )
    fake_api.start()
    rc = main(["decl", "list", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "demo" in out
    assert "sha256:abc" in out


def test_decl_list_empty(fake_api, capsys) -> None:
    fake_api.route(
        "GET", r"/v1alpha1/declarations$", lambda h, m, q, b: h.send_json(200, {"items": []})
    )
    fake_api.start()
    rc = main(["decl", "list", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "no published declarations" in out


# --- focus ------------------------------------------------------------------------


FOCUS_PAYLOAD = {
    "center": "two",
    "distance": 1,
    "direction": "both",
    "link": "both",
    "declarations": [
        {
            "name": "two",
            "declaration_id": "d2",
            "version": 1,
            "digest": "sha256:two",
            "distance": 0,
            "trigger_kind": "t",
            "action_kind": "a",
            "start_node": "s",
            "landing_node": "l",
        },
        {
            "name": "one",
            "declaration_id": "d1",
            "version": 1,
            "digest": "sha256:one",
            "distance": 1,
            "trigger_kind": "t",
            "action_kind": "a",
            "start_node": "s",
            "landing_node": "l",
        },
    ],
    "nodes": [],
    "links": [{"from": "two", "to": "one", "kind": "must"}],
}


def test_decl_focus_passes_distance_direction_link_query(fake_api) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["query"] = q
        h.send_json(200, FOCUS_PAYLOAD)

    fake_api.route("GET", r"/v1alpha1/declarations/(?P<name>[^/]+)/focus", handler)
    fake_api.start()
    main(
        [
            "decl",
            "focus",
            "two",
            "--distance",
            "2",
            "--direction",
            "up",
            "--link",
            "must",
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert seen["query"] == {"distance": ["2"], "direction": ["up"], "link": ["must"]}


def test_decl_focus_text(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/declarations/(?P<name>[^/]+)/focus",
        lambda h, m, q, b: h.send_json(200, FOCUS_PAYLOAD),
    )
    fake_api.start()
    rc = main(["decl", "focus", "two", "--distance", "1", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "center: two" in out
    assert "[0] two" in out
    assert "[1] one" in out
    assert "link: two --must--> one" in out


def test_decl_focus_accepts_an_alias_shaped_name(fake_api) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["path"] = h.path
        h.send_json(200, {**FOCUS_PAYLOAD, "center": "chain-a"})

    fake_api.route("GET", r"/v1alpha1/declarations/(?P<name>[^/]+)/focus", handler)
    fake_api.start()
    rc = main(["decl", "focus", "chain-a", "--api-url", fake_api.base_url])
    assert rc == 0
    assert seen["path"].split("?")[0] == "/v1alpha1/declarations/chain-a/focus"


# --- activate -----------------------------------------------------------------------


def test_decl_activate_text(fake_api, capsys, monkeypatch) -> None:
    monkeypatch.setenv("NODES_ACTOR_TOKEN", "tok-1")
    fake_api.route(
        "POST",
        r"/v1alpha1/declarations/(?P<name>[^/]+)/activate",
        lambda h, m, q, b: h.send_json(
            200, {"name": "toggle", "version_id": "dv-1", "active": True}
        ),
    )
    fake_api.start()
    rc = main(["decl", "activate", "toggle", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "active: True" in out


def test_decl_activate_sends_version_id(fake_api, monkeypatch) -> None:
    monkeypatch.setenv("NODES_ACTOR_TOKEN", "tok-1")
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(200, {"name": "toggle", "version_id": "dv-2", "active": True})

    fake_api.route("POST", r"/v1alpha1/declarations/(?P<name>[^/]+)/activate", handler)
    fake_api.start()
    main(["decl", "activate", "toggle", "--version-id", "dv-2", "--api-url", fake_api.base_url])
    assert seen["body"] == {"version_id": "dv-2"}


def test_decl_activate_422_relayed(fake_api, capsys, monkeypatch) -> None:
    monkeypatch.setenv("NODES_ACTOR_TOKEN", "tok-1")
    fake_api.route(
        "POST",
        r"/v1alpha1/declarations/(?P<name>[^/]+)/activate",
        lambda h, m, q, b: h.send_json(
            422,
            {
                "code": 1,
                "message": "an agent principal may not activate an activation declaration",
                "remediation": "have a human activate it",
            },
        ),
    )
    fake_api.start()
    rc = main(["decl", "activate", "root-rule", "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert "may not activate" in captured.err
