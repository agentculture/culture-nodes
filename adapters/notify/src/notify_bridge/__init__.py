"""notify_bridge: an actor-protocol bridge that posts a workflow-declared
message to a Discord webhook (issue #68).

# Why an actor, not a node kind

The control-plane process gains no Discord egress (the same PRD boundary
rule `cmd/nodes-notifier` (task t14) and `internal/events/doc.go` already
honor). A `kind: notify` node the worker executed directly would put a
webhook POST inside the control plane and undo that. This package is the
out-of-process thing that holds the credential instead: a workflow node
reaches it as an ordinary `kind: agent` node with `uses:` naming this
bridge's registered actor.

# Distinct from the other two Discord lanes

* `internal/notify` / `cmd/nodes-notifier` (task t13/t14) answer "what is
  the system doing?" -- unattended run-lifecycle monitoring, never a
  mechanism a workflow depends on for delivery, no ledger record.
* `scripts/notify-discord.sh` (where present) is an operator pinging the
  channel by hand -- ad hoc, no ledger record.
* This bridge answers "tell this person this thing" -- a declared step in
  a workflow, gated on a ledger record per send.

# Ported rules

Same env-resolved-URL / bounded-POST / no-retry / no-redirect / fail-open
discipline as `internal/notify` (itself a Go port of devex's proven
design) -- see `webhook.py`'s module docstring for the rule-by-rule
mapping. The payload shape differs on purpose: `internal/notify`'s Payload
is a fixed five-field run-lifecycle envelope so the notifier daemon can
never leak ledger/node content into a channel nobody scoped for it; this
bridge's message is exactly what the workflow node's `input` asked to be
sent -- the workflow author is the trust boundary here, the same way any
node's `input` is author-controlled.

# Trust model

The bridge emits exactly one `proposed` ledger record per invocation
(`mapping.claim_record`), carrying a domain outcome and the webhook's HTTP
status -- never `observed` (a 2xx from Discord means the request was
accepted, not that a human read it) and never the webhook URL or the
message body (issue #68's hard constraint: "only status codes").
"""
