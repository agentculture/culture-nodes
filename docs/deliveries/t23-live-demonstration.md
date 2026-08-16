# t23 criterion 2, demonstrated

Task t23's second criterion — *one dispatch reaches a bridge the control plane
holds no address for* — was split out of the dispatched package as deviation
**d21**, because the codex sandbox denies `socket(2)` and could neither perform
nor observe it. This is that half, run in the operator lane against a scratch
deployment.

**Scratch, not production, and deliberately.** The decision document
(`docs/decisions/transport-inversion.md`) sets its own precondition: run the
fleet demonstration only *after issue #111's replacement is deployed*, because
accepting inbound dials changes who may attempt to authenticate as an actor.
Converting a real bridge on thor or orin would have taken on that debt without
a decision from the owner. A scratch control plane, scratch database and
scratch bridge prove the mechanism and change the fleet's security posture not
at all.

## What ran

PostgreSQL 17 in a container, `nodes all` on `127.0.0.1:18744` (API, scheduler
and worker), and a minimal bridge: a local `/v1/invocations` handler plus the
shared `dialin.py` loop that the five bridge packages carry byte-identically.
The three pre-existing `nodes serve` processes on this host were left alone.

## The three proofs the procedure demands

**1. The control plane holds no address.** Not "an address it did not use" —
none:

```text
      actor_key      | revision | endpoint_ref | address_is_null
---------------------+----------+--------------+-----------------
 company/dialin-demo |        1 |              | t
```

**2. The completed attempt is tied to that actor key.**

```text
01M04K4V5721WMV4BD894XBFNJ | succeeded | company/dialin-demo
```

Run `01M04K4TZVYFQX3W9M9SGTTWK3` reached state `completed`.

**3. It travelled the inbound mailbox, not an outbound HTTP call.**

```text
id 64005fe5-…  actor_key company/dialin-demo  attempt_id att_01M04K4V1P2BEQ082W1V0VRRZ1
response_status 200   claimed t   completed t
```

The bridge logged `BRIDGE RECEIVED INVOCATION` carrying the run id and a
callback url — work it was handed after dialling out, having never been dialled.

## What the demonstration found

**Every bridge in the fleet would have spun forever without claiming work.**

`dialin.py` handled the idle poll like this:

```python
try: response = opener(req, timeout=35)
except urllib.error.HTTPError as exc:
    if exc.code == 204: pause(.25); continue
    raise
item = json.loads(response.read())
```

An idle long poll answers **204 with an empty body**. 204 is a 2xx, so `urllib`
does not raise `HTTPError` — that branch is unreachable against the shipped
server. `json.loads("")` raised instead, the loop logged
`dial-in reconnecting: Expecting value: line 1 column 1 (char 0)`, slept a
second, and started over. The empty mailbox is the **normal** case, so this was
the steady state and not an edge.

Unit tests could not have caught it: they exercised `configured()`, and the
only thing that shows the shape of an idle poll is an idle poll.

Fixed in all five bridges — the module is byte-identical, verified by checksum
before and after — with a regression test in each that drives the loop through
three idle polls and asserts every pause is the 0.25s idle nap rather than the
1s reconnect backoff. Ablation restores the original log line exactly.

## Two setup errors worth recording, because neither was a product defect

- `nodes all` ensures its own `default` namespace and ignores
  `NODES_NAMESPACE_ID`; an actor registered in another namespace resolves to
  `no registered actor for this reference`.
- A worker with no `NODES_CALLBACK_BASE_URL` / `NODES_CALLBACK_TOKEN_SECRET`
  refuses to dispatch to any actor that does not answer synchronously, and says
  so precisely: *"this worker offers no callback endpoint … so it can only
  dispatch to actors that answer synchronously"*.

Both diagnostics named the cause exactly, which is the only reason this took
minutes rather than an afternoon.

## Still not demonstrated

Mixed mode's *simultaneity* — one converted bridge and one unconverted bridge
live at the same time — is step 6 of the procedure and needs two real bridges
on two hosts. That remains a fleet action, gated on the same #111 precondition.
