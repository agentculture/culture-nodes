"""``nodes chain`` (task t21, #328): the thin client over the declaration
alias (chain) API, exercised against ``tests/fake_api.py``."""

from __future__ import annotations

import json

import pytest

from culture_nodes.cli import main


def test_chain_bare_noun(capsys) -> None:
    rc = main(["chain"])
    assert rc == 0
    assert "usage: nodes chain" in capsys.readouterr().out


# --- alias (write: human session vs agent token) --------------------------------


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
def test_chain_alias_sends_whichever_credential_the_environment_gives_it(
    fake_api, monkeypatch, credential
) -> None:
    monkeypatch.delenv("NODES_ACTOR_TOKEN", raising=False)
    monkeypatch.delenv("NODES_OP_COOKIE", raising=False)
    for key, value in credential["env"].items():
        monkeypatch.setenv(key, value)

    seen: dict = {}

    def handler(h, m, q, b):
        seen["auth"] = h.headers.get("Authorization")
        seen["cookie"] = h.headers.get("Cookie")
        seen["body"] = json.loads(b)
        h.send_json(201, {"id": "al-1", "name": "chain-a", "declarations": ["one"]})

    fake_api.route("POST", r"/v1alpha1/declarations/aliases$", handler)
    fake_api.start()
    rc = main(
        ["chain", "alias", "chain-a", "--declarations", "one", "--api-url", fake_api.base_url]
    )
    assert rc == 0
    assert seen["auth"] == credential["expect_auth"]
    assert seen["cookie"] == credential["expect_cookie"]
    assert seen["body"] == {"name": "chain-a", "declarations": ["one"]}


def test_chain_alias_without_declarations_omits_the_field(fake_api, monkeypatch) -> None:
    monkeypatch.setenv("NODES_ACTOR_TOKEN", "tok-1")
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(201, {"id": "al-2", "name": "chain-parent", "declarations": []})

    fake_api.route("POST", r"/v1alpha1/declarations/aliases$", handler)
    fake_api.start()
    main(["chain", "alias", "chain-parent", "--api-url", fake_api.base_url])
    assert seen["body"] == {"name": "chain-parent"}


def test_chain_alias_text_output(fake_api, capsys, monkeypatch) -> None:
    monkeypatch.setenv("NODES_ACTOR_TOKEN", "tok-1")
    fake_api.route(
        "POST",
        r"/v1alpha1/declarations/aliases$",
        lambda h, m, q, b: h.send_json(
            201, {"id": "al-1", "name": "chain-a", "declarations": ["one"]}
        ),
    )
    fake_api.start()
    rc = main(["chain", "alias", "chain-a", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "name: chain-a" in out
    assert "declarations: one" in out


def test_chain_alias_401_relayed(fake_api, capsys, monkeypatch) -> None:
    monkeypatch.delenv("NODES_ACTOR_TOKEN", raising=False)
    monkeypatch.delenv("NODES_OP_COOKIE", raising=False)
    fake_api.route(
        "POST",
        r"/v1alpha1/declarations/aliases$",
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
    rc = main(["chain", "alias", "chain-a", "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert "error: declarations require an authenticated principal" in captured.err


# --- show -------------------------------------------------------------------------


def test_chain_show_text(fake_api, capsys) -> None:
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
                "author": "a",
                "created_at": "2026-01-01T00:00:00Z",
                "warnings": [],
                "active": False,
                "links": [{"to": "one", "kind": "must"}],
            },
        ),
    )
    fake_api.start()
    rc = main(["chain", "show", "two", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert "name: two" in out
    assert "link: must -> one" in out


def test_chain_show_accepts_a_declaration_name_without_touching_the_alias_route(
    fake_api,
) -> None:
    """A name that resolves as a declaration is rendered as a declaration --
    the alias route is never reached (declaration wins, spec c31/h23's
    documented precedence; same order ``GET .../focus`` uses server-side)."""
    seen = {"alias_route_hit": False}

    def decl_handler(h, m, q, b):
        seen["path"] = h.path
        h.send_json(
            200,
            {
                "id": "dv-1",
                "declaration_id": "decl-1",
                "name": "two",
                "version": 1,
                "digest": "sha256:abc",
                "author": "a",
                "created_at": "2026-01-01T00:00:00Z",
                "warnings": [],
                "active": False,
                "links": [],
            },
        )

    def alias_handler(h, m, q, b):
        seen["alias_route_hit"] = True
        h.send_json(200, {"id": "al-1", "name": "two", "declarations": [], "aliases": []})

    fake_api.route("GET", r"/v1alpha1/declarations/(?P<name>[^/]+)$", decl_handler)
    fake_api.route("GET", r"/v1alpha1/declaration-aliases/(?P<name>[^/]+)$", alias_handler)
    fake_api.start()
    rc = main(["chain", "show", "two", "--api-url", fake_api.base_url])
    assert rc == 0
    assert seen["path"] == "/v1alpha1/declarations/two"
    assert seen["alias_route_hit"] is False


def test_chain_show_falls_back_to_the_alias_route_for_a_pure_alias_name(fake_api, capsys) -> None:
    """The chain-defining acceptance criterion (task t21b, #328; spec h23):
    ``chain show`` accepts a NAME THAT IS ONLY AN ALIAS, not a declaration --
    the declaration route 404s and ``show`` falls back to
    ``GET /v1alpha1/declaration-aliases/{name}`` before giving up, and
    renders the alias's parent, member declarations, and child aliases."""
    seen: dict = {}

    def decl_handler(h, m, q, b):
        h.send_json(
            404,
            {
                "code": 1,
                "message": 'no declaration named "chain-a"',
                "remediation": "check the declaration name",
            },
        )

    def alias_handler(h, m, q, b):
        seen["path"] = h.path
        h.send_json(
            200,
            {
                "id": "al-1",
                "name": "chain-a",
                "parent": "chain-parent",
                "declarations": ["one"],
                "aliases": ["chain-a-child"],
            },
        )

    fake_api.route("GET", r"/v1alpha1/declarations/(?P<name>[^/]+)$", decl_handler)
    fake_api.route("GET", r"/v1alpha1/declaration-aliases/(?P<name>[^/]+)$", alias_handler)
    fake_api.start()
    rc = main(["chain", "show", "chain-a", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    assert seen["path"] == "/v1alpha1/declaration-aliases/chain-a"
    assert "name: chain-a" in out
    assert "parent: chain-parent" in out
    assert "declarations: one" in out
    assert "aliases: chain-a-child" in out


def test_chain_show_json_passthrough_on_alias_fallback(fake_api, capsys) -> None:
    """``--json`` passthrough still works on the alias-fallback path -- the
    raw alias-route response body, unmodified, not the text rendering."""
    fake_api.route(
        "GET",
        r"/v1alpha1/declarations/(?P<name>[^/]+)$",
        lambda h, m, q, b: h.send_json(
            404,
            {
                "code": 1,
                "message": 'no declaration named "chain-a"',
                "remediation": "check the declaration name",
            },
        ),
    )
    fake_api.route(
        "GET",
        r"/v1alpha1/declaration-aliases/(?P<name>[^/]+)$",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "id": "al-1",
                "name": "chain-a",
                "parent": "chain-parent",
                "declarations": ["one"],
                "aliases": ["chain-a-child"],
            },
        ),
    )
    fake_api.start()
    rc = main(["chain", "show", "chain-a", "--json", "--api-url", fake_api.base_url])
    out = capsys.readouterr().out
    assert rc == 0
    parsed = json.loads(out)
    assert parsed == {
        "id": "al-1",
        "name": "chain-a",
        "parent": "chain-parent",
        "declarations": ["one"],
        "aliases": ["chain-a-child"],
    }


def test_chain_show_404_when_neither_a_declaration_nor_an_alias(fake_api, capsys) -> None:
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
    fake_api.route(
        "GET",
        r"/v1alpha1/declaration-aliases/(?P<name>[^/]+)$",
        lambda h, m, q, b: h.send_json(
            404,
            {
                "code": 1,
                "message": 'no alias named "bogus"',
                "remediation": "check the alias name",
            },
        ),
    )
    fake_api.start()
    rc = main(["chain", "show", "bogus", "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert "no alias named" in captured.err


def test_chain_show_does_not_mask_a_non_404_failure_with_the_alias_route(fake_api, capsys) -> None:
    # A refused credential on the declaration route is the answer; the alias
    # route must not be tried, or its 404 would hide the real 401.
    alias_hits = []
    fake_api.route(
        "GET",
        r"/v1alpha1/declarations/(?P<name>[^/]+)$",
        lambda h, m, q, b: h.send_json(
            401,
            {
                "code": 1,
                "message": "declarations require an authenticated principal",
                "remediation": "set NODES_ACTOR_TOKEN or NODES_OP_COOKIE",
            },
        ),
    )

    def alias(h, m, q, b):
        alias_hits.append(1)
        h.send_json(404, {"code": 1, "message": "no alias", "remediation": ""})

    fake_api.route("GET", r"/v1alpha1/declaration-aliases/(?P<name>[^/]+)$", alias)
    fake_api.start()
    rc = main(["chain", "show", "chain-a", "--api-url", fake_api.base_url])
    captured = capsys.readouterr()
    assert rc == 1
    assert "authenticated principal" in captured.err
    assert alias_hits == []
