"""codex-bridge: a reference actor-protocol adapter over headless `codex exec`.

Implements the PRD §13 actor protocol (`internal/actors/protocol.go` in the
culture-nodes repo) as an HTTP server that dispatches invocations to a
`codex exec --json` subprocess. It is a **reference** adapter (mirrors
adapters/colleague's own c15/h13, c24, c31 stance): it runs on the machine
that hosts a `codex` CLI install, OUTSIDE the culture-nodes control-plane
deployment, and exists to prove the actor protocol against a second, real
agent backend — not to be the production actor host for every deployment.

Stdlib-only runtime dependencies, deliberately: this package's own
`pyproject.toml` declares `dependencies = []`. It talks HTTP via
`http.server`/`urllib` and shells out to the `codex` binary via
`subprocess` — it never imports any codex Python package, so a bridge
deployment's Python environment never has to reconcile its own dependency
graph against codex's own (it is a Rust binary distributed via npm/pip
wrappers or a standalone executable).

See `README.md` for the deployment model, the exact `codex exec` argv this
bridge generates, the JSONL session-classification rules (in particular: an
incomplete or crashed codex session is a failure, never success — no
adapter-specific exemption), and the trust model (this bridge emits
`proposed`-only ledger records — no actor promotes its own proposal).
"""

__all__ = ["__version__"]

__version__ = "0.1.0"
