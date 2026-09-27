"""API/CLI parity (task t21, #328, spec h22/c32/h43): enumerates every
declaration/chain route this task covers against the CLI verb that calls
it, and drives each verb once as a human Access session and once as a
registered agent actor's own bearer -- proving agents and humans reach the
exact same route through the exact same verb (h43), with only activation
authority differing server-side (c33, not exercised here -- that is the
API's own test suite's job)."""

from __future__ import annotations

from pathlib import Path

import pytest

from culture_nodes.cli import main

CREDENTIALS = [
    pytest.param(
        {"NODES_ACTOR_TOKEN": "tok-agent-1"}, "Bearer tok-agent-1", None, id="agent-token"
    ),
    pytest.param(
        {"NODES_OP_COOKIE": "jwt-human-1"}, None, "CF_Authorization=jwt-human-1", id="human-session"
    ),
]


@pytest.fixture
def decl_file(tmp_path: Path) -> Path:
    p = tmp_path / "decl.yaml"
    p.write_text("name: demo\n", encoding="utf-8")
    return p


def _fixture_body(name: str):
    return {
        "id": "dv-1",
        "declaration_id": "decl-1",
        "name": name,
        "version": 1,
        "digest": "sha256:abc",
        "author": "a",
        "created_at": "2026-01-01T00:00:00Z",
        "warnings": [],
        "active": False,
        "links": [],
    }


def _route_cases(decl_file: Path) -> list[dict]:
    """One entry per declaration/chain route this task covers, paired with
    the CLI argv that must call it, the expected method, and a path regex."""
    return [
        {
            "id": "decl-validate",
            "argv": ["decl", "validate", str(decl_file)],
            "method": "POST",
            "path": r"/v1alpha1/declarations/validate$",
            "status": 200,
            "payload": {"valid": True, "diagnostics": [], "warnings": []},
        },
        {
            "id": "decl-declare",
            "argv": ["decl", "declare", str(decl_file)],
            "method": "POST",
            "path": r"/v1alpha1/declarations$",
            "status": 201,
            "payload": _fixture_body("demo"),
        },
        {
            "id": "decl-list",
            "argv": ["decl", "list"],
            "method": "GET",
            "path": r"/v1alpha1/declarations$",
            "status": 200,
            "payload": {"items": []},
        },
        {
            "id": "decl-show",
            "argv": ["decl", "show", "two"],
            "method": "GET",
            "path": r"/v1alpha1/declarations/two$",
            "status": 200,
            "payload": _fixture_body("two"),
        },
        {
            "id": "decl-link",
            "argv": ["decl", "link", "two", "--to", "one", "--kind", "must"],
            "method": "POST",
            "path": r"/v1alpha1/declarations/two/links$",
            "status": 201,
            "payload": {"to": "one", "kind": "must"},
        },
        {
            "id": "decl-focus",
            "argv": ["decl", "focus", "two", "--distance", "1"],
            "method": "GET",
            "path": r"/v1alpha1/declarations/two/focus$",
            "status": 200,
            "payload": {
                "center": "two",
                "distance": 1,
                "direction": "both",
                "link": "both",
                "declarations": [],
                "nodes": [],
                "links": [],
            },
        },
        {
            "id": "decl-activate",
            "argv": ["decl", "activate", "two"],
            "method": "POST",
            "path": r"/v1alpha1/declarations/two/activate$",
            "status": 200,
            "payload": {"name": "two", "version_id": "dv-1", "active": True},
        },
        {
            "id": "chain-alias",
            "argv": ["chain", "alias", "chain-a"],
            "method": "POST",
            "path": r"/v1alpha1/declarations/aliases$",
            "status": 201,
            "payload": {"id": "al-1", "name": "chain-a", "declarations": []},
        },
        {
            "id": "chain-show",
            "argv": ["chain", "show", "chain-a"],
            "method": "GET",
            "path": r"/v1alpha1/declarations/chain-a$",
            "status": 200,
            "payload": _fixture_body("chain-a"),
        },
    ]


@pytest.mark.parametrize("env,expect_auth,expect_cookie", CREDENTIALS)
def test_every_decl_and_chain_verb_calls_its_route_under_both_credentials(
    fake_api, decl_file, monkeypatch, env, expect_auth, expect_cookie
) -> None:
    monkeypatch.delenv("NODES_ACTOR_TOKEN", raising=False)
    monkeypatch.delenv("NODES_OP_COOKIE", raising=False)
    for key, value in env.items():
        monkeypatch.setenv(key, value)

    seen: dict[str, dict] = {}
    cases = _route_cases(decl_file)
    for case in cases:

        def handler(h, m, q, b, case=case):
            # ``fake_api`` only invokes this callback when the incoming
            # request's method already matched ``case["method"]`` at route
            # registration (tests/fake_api.py's ``_dispatch``), so recording
            # the method here would only ever restate the route table --
            # what actually proves parity is that the route was hit AT ALL.
            seen[case["id"]] = {
                "path": h.path.split("?")[0],
                "auth": h.headers.get("Authorization"),
                "cookie": h.headers.get("Cookie"),
            }
            h.send_json(case["status"], case["payload"])

        fake_api.route(case["method"], case["path"], handler)

    fake_api.start()

    for case in cases:
        rc = main([*case["argv"], "--api-url", fake_api.base_url])
        assert rc == 0, f"{case['id']} exited {rc}"
        recorded = seen.get(case["id"])
        assert recorded is not None, f"{case['id']} never called its route"
        assert recorded["auth"] == expect_auth, case["id"]
        assert recorded["cookie"] == expect_cookie, case["id"]


def test_parity_table_covers_every_registered_decl_and_chain_verb() -> None:
    """Every decl/chain subcommand this task registers appears in the
    parity table above -- catches a verb added without a route-parity case
    covering it."""
    from culture_nodes.cli import _build_parser

    parser = _build_parser()
    # argparse doesn't expose subparser choices cleanly without reaching
    # into the private action -- this mirrors how test_cli.py-style smoke
    # tests introspect the built parser elsewhere in this suite.
    sub_actions = [
        a for a in parser._subparsers._group_actions if hasattr(a, "choices")  # noqa: SLF001
    ]
    top = sub_actions[0].choices
    decl_verbs = {
        f"decl-{name}" for name in top["decl"]._subparsers._group_actions[0].choices  # noqa: SLF001
    }
    chain_verbs = {
        f"chain-{name}"
        for name in top["chain"]._subparsers._group_actions[0].choices  # noqa: SLF001
    }
    covered = {case["id"] for case in _route_cases(Path("/dev/null"))}
    assert decl_verbs <= covered, decl_verbs - covered
    assert chain_verbs <= covered, chain_verbs - covered
