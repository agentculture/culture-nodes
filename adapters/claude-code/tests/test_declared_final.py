"""Final-answer shapes seen from real agent sessions."""

import pytest

from claude_code_bridge import mapping

PROSE = (
    "The only GitHub credential available is rejected with `401 Bad credentials`.\n"
    "Evidence:\n- GitHub rejected the PR edit."
)
OBJECT = '{"outcome":"blocked","output":{"reason":"No usable GitHub write credential."}}'


@pytest.mark.parametrize(
    "answer, expected",
    [
        (OBJECT, ("blocked", {"reason": "No usable GitHub write credential."})),
        (
            "```json\n" + OBJECT + "\n```",
            ("blocked", {"reason": "No usable GitHub write credential."}),
        ),
        (
            PROSE + "\n\n" + OBJECT + "  ",
            ("blocked", {"reason": "No usable GitHub write credential.", "summary": PROSE}),
        ),
        (OBJECT + "\nThe credential is still rejected.", None),
        (PROSE + '\n{"output":{"reason":"No credential"}}', None),
        (OBJECT + "\n\n" + OBJECT, None),
        ('{"outcome":"done"}', ("done", {})),
        ('{"outcome":"blocked","output":null}', None),
        (
            PROSE + '\n{"outcome":"blocked","output":{"summary":"Model summary"}}',
            ("blocked", {"summary": PROSE + "\n\nModel summary"}),
        ),
    ],
)
def test_declared_final_answer_shapes(answer, expected):
    assert mapping.declared_result_override({"result": answer}) == expected
