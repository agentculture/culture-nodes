"""``culture-nodes learn`` — the learnability affordance.

Prints a structured self-teaching prompt. Must satisfy the agent-first rubric:
>=200 chars and mention purpose, command map, exit codes, --json, and explain.
"""

from __future__ import annotations

import argparse

from culture_nodes import __version__
from culture_nodes.api_client import DEFAULT_API_URL, ENV_API_URL
from culture_nodes.cli._output import emit_result

_TEXT = """\
culture-nodes — Python front + mesh agent for the Culture Nodes workflow
orchestrator.

Purpose
-------
The Python side of Culture Nodes: a mesh-agent identity (culture.yaml +
AGENTS.colleague.md), the agent-first CLI contract (cited from the teken
`python-cli` reference), and a set of product verbs — workflow, run, ledger,
review — that are pure REST clients over the Go control-plane API
(api/openapi/openapi.yaml). No workflow-engine logic lives here: every
product verb sends one HTTP request to the API and renders the response.

Identity commands
------------------
  culture-nodes whoami             Identity from culture.yaml.
  culture-nodes learn              This self-teaching prompt.
  culture-nodes explain <path>...  Markdown docs for any noun/verb path.
  culture-nodes overview           Descriptive snapshot of the agent.
  culture-nodes doctor             Check agent-identity + API-reachability invariants.
  culture-nodes cli overview       Describe the CLI surface itself.

Product commands (thin API clients)
------------------------------------
  culture-nodes workflow validate|publish|list|get
  culture-nodes run create|list|get|cancel|events
  culture-nodes ledger records|projection
  culture-nodes review create|commit

API configuration
------------------
Every product verb resolves the API base URL as: --api-url flag, then the
NODES_API_URL environment variable, then http://127.0.0.1:8080. Start the
control-plane binary with 'nodes serve' (the Go binary, a separate build)
before running a product verb.

Machine-readable output
-----------------------
Every command supports --json. Errors in JSON mode emit
{"code", "message", "remediation"} to stderr. Stdout and stderr never mix.
Product-verb --json output passes the API's own JSON payload straight
through (byte-exact).

Exit-code policy
----------------
  0 success
  1 user-input error (bad flag, bad path, missing arg, an API domain error)
  2 environment / setup error (unreachable API, unreadable file)
  3+ reserved

More detail
-----------
  culture-nodes explain culture-nodes
"""


def _as_json_payload() -> dict[str, object]:
    return {
        "tool": "culture-nodes",
        "version": __version__,
        "purpose": "Python front + mesh agent for the Culture Nodes workflow orchestrator.",
        "commands": [
            {"path": ["whoami"], "summary": "Identity probe from culture.yaml."},
            {"path": ["learn"], "summary": "Self-teaching prompt."},
            {"path": ["explain"], "summary": "Markdown docs by path."},
            {"path": ["overview"], "summary": "Descriptive snapshot of the agent."},
            {
                "path": ["doctor"],
                "summary": "Check agent-identity + API-reachability invariants.",
            },
            {"path": ["cli", "overview"], "summary": "Describe the CLI surface."},
            {
                "path": ["workflow", "validate"],
                "summary": "Compile a workflow and report diagnostics.",
            },
            {
                "path": ["workflow", "publish"],
                "summary": "Publish a workflow as an immutable version.",
            },
            {"path": ["workflow", "list"], "summary": "List published workflow versions."},
            {"path": ["workflow", "get"], "summary": "Fetch one workflow version by digest."},
            {"path": ["run", "create"], "summary": "Create a run of a published workflow."},
            {"path": ["run", "list"], "summary": "List runs, optionally filtered by state."},
            {"path": ["run", "get"], "summary": "Fetch the Run-view payload."},
            {"path": ["run", "cancel"], "summary": "Cancel a run."},
            {"path": ["run", "events"], "summary": "Stream a run's committed events (SSE)."},
            {"path": ["ledger", "records"], "summary": "List a run's ledger records."},
            {"path": ["ledger", "projection"], "summary": "Compute a standard ledger projection."},
            {
                "path": ["review", "create"],
                "summary": "Create a review request over ledger records.",
            },
            {
                "path": ["review", "commit"],
                "summary": "Commit a review's confirm/reject decisions.",
            },
        ],
        "exit_codes": {
            "0": "success",
            "1": "user-input error",
            "2": "environment/setup error",
        },
        "json_support": True,
        "api_base_url": {"precedence": ["--api-url", ENV_API_URL, DEFAULT_API_URL]},
        "explain_pointer": "culture-nodes explain <path>",
    }


def cmd_learn(args: argparse.Namespace) -> int:
    if getattr(args, "json", False):
        emit_result(_as_json_payload(), json_mode=True)
    else:
        emit_result(_TEXT, json_mode=False)
    return 0


def register(sub: argparse._SubParsersAction) -> None:
    p = sub.add_parser(
        "learn",
        help="Print a structured self-teaching prompt for agent consumers.",
    )
    p.add_argument("--json", action="store_true", help="Emit structured JSON.")
    p.set_defaults(func=cmd_learn)
