"""CLI coverage for the single workflow-generation API."""

import json

from culture_nodes.cli import main
from culture_nodes.explain import known_paths


def test_workflow_generate_calls_generation_api(fake_api, capsys) -> None:
    fake_api.route(
        "POST",
        r"/v1alpha1/workflow-generations",
        lambda h, m, q, b: h.send_json(
            202,
            {
                "run_id": "run_gen",
                "status": "proposed",
                "valid": False,
                "diagnostics": [],
            },
        ),
    )
    fake_api.start()
    rc = main(
        [
            "workflow",
            "generate",
            "make a review flow",
            "--actor-ref",
            "actor://company/planner@sha256:abc",
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert rc == 0
    assert "status: proposed" in capsys.readouterr().out


def test_workflow_generation_exhausted_is_domain_exit_one(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/workflow-generations/run_gen",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "run_id": "run_gen",
                "status": "exhausted",
                "valid": False,
                "diagnostics": [],
            },
        ),
    )
    fake_api.start()
    rc = main(
        [
            "workflow",
            "generation-get",
            "run_gen",
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert rc == 1
    captured = capsys.readouterr()
    assert "status: exhausted" in captured.out
    assert captured.err == ""


# --- declaration output (task t35, #328; spec c86/h59) ----------------------
#
# Counterparts of the two tests above for the declaration form: the same
# verbs, with --output declarations selecting it, plus generation-publish
# (the only verb that writes, so the only one that must carry a credential).

DECL_SET = {
    "declarations": [
        {
            "name": "gen-intake",
            "format": "json",
            "source": "{}",
            "valid": True,
            "diagnostics": [],
            "warnings": [],
        },
    ],
    "links": [{"from": "gen-announce", "to": "gen-intake", "kind": "must"}],
    "valid": True,
    "published": False,
    "diagnostics": [],
    "warnings": ["gen-announce: sensitivity: widens into discord"],
}


def test_workflow_generate_default_output_is_unchanged(fake_api, capsys) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(
            202,
            {
                "run_id": "r",
                "status": "proposed",
                "output": "workflow",
                "valid": False,
                "diagnostics": [],
            },
        )

    fake_api.route("POST", r"/v1alpha1/workflow-generations", handler)
    fake_api.start()
    rc = main(
        [
            "workflow",
            "generate",
            "d",
            "--actor-ref",
            "actor://company/p",
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert rc == 0
    assert "output" not in seen["body"]


def test_workflow_generate_declarations_sends_output(fake_api, capsys) -> None:
    seen = {}

    def handler(h, m, q, b):
        seen["body"] = json.loads(b)
        h.send_json(
            202,
            {
                "run_id": "r",
                "status": "proposed",
                "output": "declarations",
                "valid": False,
                "diagnostics": [],
            },
        )

    fake_api.route("POST", r"/v1alpha1/workflow-generations", handler)
    fake_api.start()
    rc = main(
        [
            "workflow",
            "generate",
            "d",
            "--actor-ref",
            "actor://company/p",
            "--output",
            "declarations",
            "--api-url",
            fake_api.base_url,
        ]
    )
    assert rc == 0
    assert seen["body"]["output"] == "declarations"
    assert "output: declarations" in capsys.readouterr().out


def test_workflow_generation_get_renders_declarations_and_warnings(fake_api, capsys) -> None:
    fake_api.route(
        "GET",
        r"/v1alpha1/workflow-generations/run_gen",
        lambda h, m, q, b: h.send_json(
            200,
            {
                "run_id": "run_gen",
                "status": "confirmed",
                "output": "declarations",
                "valid": True,
                "diagnostics": [],
                "declarations": DECL_SET,
            },
        ),
    )
    fake_api.start()
    rc = main(["workflow", "generation-get", "run_gen", "--api-url", fake_api.base_url])
    assert rc == 0
    out = capsys.readouterr().out
    assert "declaration: gen-intake (valid)" in out
    assert "link: gen-announce -must-> gen-intake" in out
    assert "warning: gen-announce: sensitivity: widens into discord" in out


def test_workflow_generation_publish_sends_credential_and_surfaces_warnings(
    fake_api, capsys, monkeypatch
) -> None:
    monkeypatch.delenv("NODES_ACTOR_TOKEN", raising=False)
    monkeypatch.setenv("NODES_OP_COOKIE", "jwt-human")
    seen = {}

    def handler(h, m, q, b):
        seen["cookie"] = h.headers.get("Cookie")
        published = dict(DECL_SET, published=True)
        published["declarations"] = [
            dict(DECL_SET["declarations"][0], version=1, version_id="dv1", author="actor-human")
        ]
        h.send_json(
            201,
            {
                "run_id": "run_gen",
                "status": "confirmed",
                "output": "declarations",
                "valid": True,
                "diagnostics": [],
                "declarations": published,
            },
        )

    fake_api.route("POST", r"/v1alpha1/workflow-generations/run_gen/publish", handler)
    fake_api.start()
    rc = main(["workflow", "generation-publish", "run_gen", "--api-url", fake_api.base_url])
    assert rc == 0
    assert seen["cookie"] == "CF_Authorization=jwt-human"
    out = capsys.readouterr().out
    assert "published: 1 declaration(s), inactive" in out
    assert "gen-intake v1 author=actor-human" in out
    assert "warning: gen-announce: sensitivity: widens into discord" in out


def test_workflow_generation_publish_catalog_entry() -> None:
    assert ("workflow", "generation-publish") in set(known_paths())
