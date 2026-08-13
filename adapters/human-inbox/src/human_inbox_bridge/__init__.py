"""human-inbox-bridge: a reference actor-protocol adapter for humans as actors.

Implements the PRD §13 actor protocol (`internal/actors/protocol.go` in the
culture-nodes repo) for `kind=human` actors (issue #38a, spec claim c11): an
invocation does not dispatch a subprocess — it parks as a durable inbox task
(HTTP 202, no lease, no heartbeat promise) until a human lists it and submits
a result `{outcome, output, note}`, at which point the bridge delivers the
terminal `completed` event through the same authenticated §13.4 callback path
the agent bridges use. Zero actor-kind branching is needed engine-side:
dispatch never reads `actors.kind`, so a human actor is just an actor whose
"execution" is a person answering later.

Stdlib-only runtime dependencies, deliberately (mirrors `adapters/colleague`
and `adapters/claude-code`): HTTP via `http.server`/`urllib`, durability via
JSON files under the state dir. No PyPI dependency graph.

Honesty rules this bridge encodes:

* the human's submission becomes a `proposed` claim record with
  `origin.kind: "human"` — via the ordinary append path a human proposes;
  confirmation stays a review transaction (PRD §10.4/§10.8);
* NO `usage` block is ever attached — humans report no token usage, and the
  protocol treats an absent usage as absent, never as zero (omit, never
  fabricate).

See `README.md` for deployment, configuration, the human surface, and
registering a `kind=human` actor that points here.
"""

__all__ = ["__version__"]

__version__ = "0.1.0"
