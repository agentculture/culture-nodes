"""colleague-bridge: a reference actor-protocol adapter over `colleague`.

Implements the PRD §13 actor protocol (`internal/actors/protocol.go` in the
culture-nodes repo) as an HTTP server that dispatches invocations to a
`colleague work` subprocess. It is a **reference** adapter (c15/h13, c24,
c31): it runs on the machine that hosts a `colleague` checkout, OUTSIDE the
culture-nodes control-plane deployment, and exists to prove the actor
protocol against a real agent backend — not to be the production actor host
for every deployment.

Stdlib-only runtime dependencies, deliberately: this package's own
`pyproject.toml` declares `dependencies = []`. It talks HTTP via
`http.server`/`urllib` and shells out to the `colleague` binary via
`subprocess` — it never imports the `colleague` Python package itself, so a
bridge deployment's Python environment never has to reconcile its own
dependency graph against colleague's.

See `README.md` for the deployment model, configuration reference, and
trust model (this bridge emits `proposed`-only ledger records — no actor
promotes its own proposal).
"""

__all__ = ["__version__"]

__version__ = "0.1.0"
