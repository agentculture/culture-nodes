from pathlib import Path

import jsonschema
import yaml

WORKFLOW = Path(__file__).parents[1] / "examples/jira-intake/workflow.yaml"
MARKER = "culture-nodes:ticket-page-link"


def test_intake_leaves_page_link_comment_to_the_engine():
    source = WORKFLOW.read_text()
    document = yaml.safe_load(source)
    post_comment_nodes = {
        node_id: node
        for node_id, node in document["spec"]["nodes"].items()
        if ((node.get("input") or {}).get("bindings") or {}).get("verb", {}).get("literal")
        == "post_comment"
    }
    instruction = document["spec"]["nodes"]["intake"]["input"]["bindings"]["instruction"]["literal"]
    # The graph posts exactly two comments, and neither is the ticket page
    # link: the drafted intake comment, and the machine-readable stage record
    # the sweep reads back as this ticket's watermark (task t17). The page
    # link is posted once per TICKET by the engine, not once per run by a
    # graph, which is why no node here may carry its marker.
    assert set(post_comment_nodes) == {"post-comment", "stage-intake"}
    assert MARKER not in source
    stage = post_comment_nodes["stage-intake"]["input"]["bindings"]["comment"]["literal"]
    assert stage.startswith("culture-nodes:stage=intake\n")
    assert "acknowledge pickup" in instruction
    assert "clarifying question" in instruction


def test_intake_contract_and_actor_receive_the_ticket_description():
    document = yaml.safe_load(WORKFLOW.read_text())
    schema = document["spec"]["contract"]["input"]["schema"]
    fact = {
        "source": "jira",
        "id": "SCRUM-6",
        "title": "Ticket description reaches agents",
        "status": "To Do",
        "description": "Carry the actual ask into the intake session.",
        "description_truncated": False,
    }

    jsonschema.Draft202012Validator(schema).validate(fact)
    bindings = document["spec"]["nodes"]["intake"]["input"]["bindings"]
    assert bindings["description"] == "/run/input/description"
    assert "description" in bindings["instruction"]["literal"]
