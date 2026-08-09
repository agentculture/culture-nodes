"""claude-code-bridge: a reference actor-protocol adapter over headless `claude`.

Implements the PRD §13 actor protocol (`internal/actors/protocol.go` in the
culture-nodes repo) as an HTTP server that dispatches invocations to a
headless `claude -p` (Claude Code CLI, "print mode") subprocess. It is a
**reference** adapter (c15/h13, c24, c31), the claude-code sibling of
`adapters/colleague`: it runs on the machine that has `claude` installed and
authenticated, OUTSIDE the culture-nodes control-plane deployment, and exists
to prove the actor protocol against a real agent backend — not to be the
production actor host for every deployment.

Stdlib-only runtime dependencies, deliberately: this package's own
`pyproject.toml` declares `dependencies = []`. It talks HTTP via
`http.server`/`urllib` and shells out to the `claude` binary via
`subprocess` — it never imports `claude_agent_sdk` or any other PyPI
package, so a bridge deployment's Python environment never has to reconcile
its own dependency graph against anything claude-code ships.

See `README.md` for the deployment model, configuration reference, the
`claude` CLI version gate, and trust model (this bridge emits `proposed`-only
ledger records — no actor promotes its own proposal).
"""

__all__ = ["__version__"]

__version__ = "0.1.0"
