"""CLI coverage for the single workflow-generation API."""

from culture_nodes.cli import main


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
