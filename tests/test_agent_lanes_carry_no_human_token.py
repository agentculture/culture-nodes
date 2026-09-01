"""No agent lane or operator gate script carries the human decision token.

Task t11 of login-from-anywhere (spec c45 / h31). The developer lane, the
merge-gate program and the handover gate used to write suite verdicts,
gate reports and ticket frames with ``NODES_HUMAN_DECISION_TOKEN`` — an
agent writing with the human credential. They now authenticate as their
own registered agent actor, so the bare name must not appear anywhere a
lane or a script would read it from.

What the sweep covers: ``docs/operations``, ``scripts``, ``deploy`` and
``examples``. What it deliberately admits, each for a stated reason:

- ``NODES_HUMAN_DECISION_TOKEN_SECRET`` is the SERVER's configuration of the
  human decision bearer (compose passes it to the api service; people.md
  and remove-secret.sh manage its custody). The human bearer itself keeps
  working for people until the Access principal replaces it (t18 removes
  the secret from every human's hands); this task is about agents.
- ``deploy/prod/install-secrets.sh`` generates that secret (the one place
  the acceptance criterion names) and is covered by the same ``_SECRET``
  admission; it never names the bare token.
- ``deploy/prod/lanes/unix-user.sh`` and ``account-bridges.sh`` name the
  variable in order to REFUSE a config that carries it (q5). They are the
  guard, not a consumer.
- ``scripts/decide-claims.py`` is a PERSON's decision tool, not an agent
  lane: it records human decisions and stays on the human bearer until
  the principal work re-points it.
"""

from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).parents[1]
SWEPT = ("docs/operations", "scripts", "deploy", "examples")

# The bare human credential name, not the server-side secret that
# configures it.
HUMAN_TOKEN = re.compile(r"NODES_HUMAN_DECISION_TOKEN(?!_SECRET)")

ADMITTED = {
    "deploy/prod/lanes/unix-user.sh": "refuses operator-only material naming it",
    "deploy/prod/lanes/account-bridges.sh": "refuses a rendered bridge config carrying it (q5)",
    "scripts/decide-claims.py": "a person's decision tool, not an agent lane",
}


def _swept_files() -> list[Path]:
    out: list[Path] = []
    for rel in SWEPT:
        for path in sorted((ROOT / rel).rglob("*")):
            if path.is_file() and ".git" not in path.parts:
                out.append(path)
    return out


def test_no_agent_lane_or_gate_script_names_the_human_decision_token():
    hits: dict[str, list[int]] = {}
    for path in _swept_files():
        try:
            text = path.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            continue
        rel = path.relative_to(ROOT).as_posix()
        if rel in ADMITTED:
            continue
        lines = [i + 1 for i, line in enumerate(text.splitlines()) if HUMAN_TOKEN.search(line)]
        if lines:
            hits[rel] = lines
    assert not hits, (
        "NODES_HUMAN_DECISION_TOKEN is still read or granted by an agent lane or operator "
        f"script (c45): {hits}. Authenticate with the actor's own credential instead "
        "(NODES_ACTOR_MERGE_GATE_TOKEN for the gate scripts, the lane's own actor token "
        "for the developer lane)."
    )


def test_the_admitted_files_still_exist_and_still_name_it():
    """An allowlist entry that no longer matches anything is a stale
    exemption, and one that matches nothing is where drift hides."""
    for rel in ADMITTED:
        path = ROOT / rel
        assert path.is_file(), rel
        assert HUMAN_TOKEN.search(
            path.read_text(encoding="utf-8")
        ), f"{rel} no longer names NODES_HUMAN_DECISION_TOKEN; drop it from ADMITTED"
