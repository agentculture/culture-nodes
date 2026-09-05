"""pi and qwen run_conformance_kit.sh must send the SAME -input, -async-input
and -bad-input JSON to the conformance kit, modulo the scratch repo path
(task t3, issue #297 acceptance criterion) -- that identical-input property
is the actual harness-swap proof: the same JSON reaching two different
bridges is what shows the swap is real, not a coincidence of two scripts
that happen to both pass.

Both scripts interpolate the live scratch repo path into these strings at
run time (`${SCRATCH_REPO}`), so this test compares the LITERAL JSON
templates as they appear in each script's source -- after normalizing that
one placeholder away -- rather than executing either script.
"""

from __future__ import annotations

import re
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
PI_SCRIPT = REPO_ROOT / "adapters" / "pi" / "scripts" / "run_conformance_kit.sh"
QWEN_SCRIPT = REPO_ROOT / "adapters" / "qwen" / "scripts" / "run_conformance_kit.sh"

# Matches e.g. INPUT="{...}" -- captures the flag name (INPUT / ASYNC_INPUT /
# BAD_INPUT) and its JSON template string.
_ASSIGNMENT_RE = re.compile(r'^(INPUT|ASYNC_INPUT|BAD_INPUT)="(.*)"$', re.MULTILINE)

#: The one difference the templates are allowed to have: the scratch repo
#: variable interpolation. Every other byte must match.
_REPO_PLACEHOLDER_RE = re.compile(r"\$\{SCRATCH_REPO\}")


def _extract_input_assignments(script_path: Path) -> dict[str, str]:
    text = script_path.read_text(encoding="utf-8")
    assignments = dict(_ASSIGNMENT_RE.findall(text))
    assert assignments.keys() == {"INPUT", "ASYNC_INPUT", "BAD_INPUT"}, (
        f"{script_path} does not declare exactly INPUT/ASYNC_INPUT/BAD_INPUT "
        f"assignments (found: {sorted(assignments)})"
    )
    return assignments


def _normalized(template: str) -> str:
    """The template with the scratch-repo placeholder removed, so what is
    left is everything the two scripts must agree on byte-for-byte."""
    return _REPO_PLACEHOLDER_RE.sub("", template)


def test_pi_and_qwen_scripts_exist_and_are_executable():
    for script in (PI_SCRIPT, QWEN_SCRIPT):
        assert script.is_file(), f"{script} does not exist"
        assert script.stat().st_mode & 0o111, f"{script} is not executable"


def test_input_json_matches_modulo_repo_path():
    pi_inputs = _extract_input_assignments(PI_SCRIPT)
    qwen_inputs = _extract_input_assignments(QWEN_SCRIPT)

    for flag in ("INPUT", "ASYNC_INPUT", "BAD_INPUT"):
        pi_normalized = _normalized(pi_inputs[flag])
        qwen_normalized = _normalized(qwen_inputs[flag])
        assert pi_normalized == qwen_normalized, (
            f"-{flag.lower().replace('_', '-')} differs between the pi and qwen "
            f"run_conformance_kit.sh scripts beyond the scratch repo path:\n"
            f"  pi:   {pi_inputs[flag]!r}\n"
            f"  qwen: {qwen_inputs[flag]!r}"
        )


def test_input_json_actually_interpolates_the_repo_path():
    """Guards against the comparison above passing vacuously because
    neither script interpolates the repo path at all."""
    for script, inputs in (
        (PI_SCRIPT, _extract_input_assignments(PI_SCRIPT)),
        (QWEN_SCRIPT, _extract_input_assignments(QWEN_SCRIPT)),
    ):
        assert _REPO_PLACEHOLDER_RE.search(
            inputs["INPUT"]
        ), f"{script}: -input does not interpolate the scratch repo path"
        assert _REPO_PLACEHOLDER_RE.search(
            inputs["ASYNC_INPUT"]
        ), f"{script}: -async-input does not interpolate the scratch repo path"
        assert _REPO_PLACEHOLDER_RE.search(
            inputs["BAD_INPUT"]
        ), f"{script}: -bad-input does not interpolate the scratch repo path"
