"""qwen-bridge: a reference actor-protocol adapter driving Qwen Code over ACP.

Implements the PRD §13 actor protocol (`internal/actors/protocol.go` in the
culture-nodes repo) as an HTTP server that dispatches invocations to a
`qwen --acp` subprocess (stdio JSON-RPC; the bridge acts as the ACP client,
Qwen Code as the agent). It is a **reference** adapter — the fourth, after
`adapters/colleague`, `adapters/claude-code` and `adapters/codex`: it runs
on the machine that hosts a `qwen` install, OUTSIDE the culture-nodes
control-plane deployment, and exists to prove the actor protocol against a
fourth, real agent backend — not to be the production actor host for every
deployment.

Stdlib-only runtime dependencies, deliberately: this package's own
`pyproject.toml` declares `dependencies = []`. It talks HTTP via
`http.server`/`urllib` and spawns the `qwen` binary via `subprocess` — it
never imports any qwen package (there is none to import: qwen-code is a
Node.js bundle), so a bridge deployment's Python environment never has to
reconcile its own dependency graph against qwen's own install.

See `README.md` for the deployment model, the exact `qwen --acp` argv and
the JSON-RPC calls this bridge makes, the ACP terminal-event classification
rules (in particular: an incomplete or crashed qwen session is a failure,
never success — no adapter-specific exemption), and the trust model (this
bridge emits `proposed`-only ledger records — no actor promotes its own
proposal).
"""

__all__ = ["__version__"]

__version__ = "0.1.0"
