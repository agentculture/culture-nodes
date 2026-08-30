from pathlib import Path

import jsonschema
import yaml

WORKFLOW = Path(__file__).parents[1] / "examples/jira-intake/workflow.yaml"
MARKER = "culture-nodes:ticket-page-link"


def test_intake_leaves_page_link_comment_to_the_engine():
    source = WORKFLOW.read_text()
    document = yaml.safe_load(source)
    post_comment_nodes = [
        node
        for node in document["spec"]["nodes"].values()
        if ((node.get("input") or {}).get("bindings") or {}).get("verb", {}).get("literal")
        == "post_comment"
    ]
    instruction = document["spec"]["nodes"]["intake"]["input"]["bindings"]["instruction"]["literal"]
    assert len(post_comment_nodes) == 1
    assert MARKER not in source
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
