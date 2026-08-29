from pathlib import Path

import yaml


WORKFLOW = Path(__file__).parents[1] / "examples/jira-intake/workflow.yaml"
MARKER = "culture-nodes:ticket-page-link"


def test_intake_has_one_page_link_marker_comment_across_its_milestones():
    source = WORKFLOW.read_text()
    document = yaml.safe_load(source)
    post_comment_nodes = [
        node
        for node in document["spec"]["nodes"].values()
        if ((node.get("input") or {}).get("bindings") or {}).get("verb", {}).get("literal")
        == "post_comment"
    ]
    assert len(post_comment_nodes) == 1
    assert source.count(MARKER) == 1
