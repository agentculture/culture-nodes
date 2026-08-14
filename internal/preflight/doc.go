// Package preflight is the clarify-then-commit gate's contract: the
// capability surface a bridge advertises, the per-actor configuration that
// turns the gate on, and the deterministic composition of the preflight
// document an actor must acknowledge before its first billable turn
// (issue #67, task t14).
//
// # The shape this generalizes
//
// deploy/prod/install-secrets.sh already runs a single-use, windowed
// confirmation for DESTRUCTIVE actions: the first attempt is refused, a file
// is written stating exactly what breaks, and the rotation proceeds only
// after a human or agent edits `verdict: hold` to `verdict: rotate` — within
// a window, once per confirmation (pinned by
// tests/deploy/destructiveconfirm_test.go). Issue #67 generalizes that shape
// from danger to UNDERSTANDING: the operating facts a dispatched actor's
// task actually depends on are handed over as a refusal-shaped document
// before the first billable turn, rather than learned by violating them and
// being corrected after a session's budget is already spent.
//
// Everything the destructive protocol makes load-bearing is kept here:
// the document defaults to VerdictHold, it states what does not proceed and
// why, the acknowledgement is single-use, and it is windowed so a stale
// acknowledgement cannot authorize today's dispatch.
//
// # Protocol in the engine, facts from the bridges
//
// This package holds the PROTOCOL. It never learns which backend is on the
// other end: a bridge advertises its own host facts as an opaque `host`
// object inside its registered capabilities (Surface), and the composer
// copies that object into the document VERBATIM. The engine states who said
// it and when; it never re-renders, re-interprets, or supplements a fact it
// did not measure.
//
// A per-bridge protocol was rejected deliberately: four implementations of
// one contract is the exact duplication that let one bug ship three times in
// three lanes. A prompt-composer-only version was rejected because it
// produces no ledger record, which would leave "the actor was told" an
// assumption rather than the evidence the gate exists to create.
//
// # Where each half lives
//
//   - capabilities.preflight — what the BRIDGE advertises about its host
//     (Surface). Facts, supplied by the party that can measure them.
//   - metadata.preflight_gate — whether the DEPLOYMENT turns the gate on for
//     this actor (Gate). Configuration, supplied by the operator.
//
// The split is why CheckConfiguration can refuse at configuration time:
// enabling a gate against an actor whose registration advertises no surface
// is refused when the actor is registered, not discovered later when a run
// stalls. The gate is per-actor and DEFAULT-OFF — an absent
// `preflight_gate` block is every actor registered before this shipped, and
// they dispatch exactly as they did.
//
// # Ledger authority
//
// The document this package composes is DERIVED: a deterministic function of
// the advertised host state and the task declaration, computed by the engine
// (an origin the ledger admits derived authority from). The acknowledgement
// is PROPOSED by the actor. Neither is observed, and no actor promotes its
// own claim — an acknowledgement is a completion-claim-shaped statement
// ("I was told and I understood"), never evidence that it was understood.
package preflight
