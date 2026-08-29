"""One contract for page replies and Jira sweep comment facts (t10)."""

import ast
import hashlib
import json
from pathlib import Path

import jsonschema


ROOT = Path(__file__).parents[1]


def test_page_reply_and_sweep_comment_validate_against_the_same_schema():
    schema = json.loads((ROOT / "schemas/events/jira_comment.schema.json").read_text())
    fixtures = [
        {
            "id": "SCRUM-230",
            "originating_question_id": "question-1",
            "replier": "operator",
            "origin": {"kind": "human", "replier": "operator"},
            "answer": {"comment_id": "ticket-page", "body": "Use option A."},
        },
        {
            "id": "SCRUM-230",
            "originating_question_id": "question-1",
            "answer": {"comment_id": "10123", "body": "Use option A."},
            "source": "jira",
        },
    ]
    for fact in fixtures:
        jsonschema.Draft202012Validator(schema).validate(fact)


def test_jira_comment_self_echo_function_bytes_are_pinned():
    source = (ROOT / "examples/pr-upkeep/pr_upkeep_jira.py").read_text()
    tree = ast.parse(source)
    node = next(
        item
        for item in tree.body
        if isinstance(item, ast.FunctionDef) and item.name == "jira_comment_is_self_echo"
    )
    function_bytes = "".join(source.splitlines(keepends=True)[node.lineno - 1 : node.end_lineno]).encode()
    assert hashlib.sha256(function_bytes).hexdigest() == (
        "d8640d8b9d07123e98c62e02ff466077b58ca4f764c03adbce7f46d82ff0eebe"
    )
