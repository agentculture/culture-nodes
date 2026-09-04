"""pi-bridge: a reference actor-protocol adapter driving pi coding agent over ACP.

Implements the PRD §13 actor protocol (`internal/actors/protocol.go` in the
culture-nodes repo) as an HTTP server that dispatches invocations to a
`pi --acp` subprocess (stdio JSON-RPC; the bridge acts as the ACP client,
pi coding agent as the agent). It is a **reference** adapter — the fourth, after
`adapters/colleague`, `adapters/claude-code` and `adapters/codex`: it runs
on the machine that hosts a `pi` install, OUTSIDE the culture-nodes
control-plane deployment, and exists to prove the actor protocol against a
fourth, real agent backend — not to be the production actor host for every
deployment.

Stdlib-only runtime dependencies, deliberately: this package's own
`pyproject.toml` declares `dependencies = []`. It talks HTTP via
`http.server`/`urllib` and spawns the `pi` binary via `subprocess` — it
never imports any pi package (there is none to import: pi coding agent is a
Node.js bundle), so a bridge deployment's Python environment never has to
reconcile its own dependency graph against pi's own install.

See `README.md` for the deployment model, the exact `pi --acp` argv and
the JSON-RPC calls this bridge makes, the ACP terminal-event classification
rules (in particular: an incomplete or crashed pi session is a failure,
never success — no adapter-specific exemption), and the trust model (this
bridge emits `proposed`-only ledger records — no actor promotes its own
proposal).
"""

__all__ = ["__version__"]

__version__ = "0.1.0"
