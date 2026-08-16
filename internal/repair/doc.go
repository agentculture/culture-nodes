// Package repair closes the gate → repair → gate loop that issue #102
// describes and nothing has ever executed: it decides, mechanically, where a
// failing merge gate goes next, and it states the bound it decided under.
//
// # What it replaces
//
// The operator. Nine packages in the own-the-work-end-to-end batch (#87)
// failed their gate, and every one of them was repaired by hand in the
// operator's own interactive session — the most expensive lane in the
// deployment, and the only one whose work leaves no ledger record. #102's
// tally of what the operator wrote rather than the actor is the evidence: a
// fixture spliced at the wrong indentation, a pause path that never re-armed
// its timer, an unbounded request body. Small, well-specified, testable
// changes, all of them invisible afterwards.
//
// The failure was never that a machine did not do the fixing. It was that
// **the system already had the vocabulary to express this and did not use
// it**: a failing gate is a domain outcome, not an engine failure (PRD §3.4),
// and a domain outcome is a thing a graph routes.
//
// # The bound is the feature
//
// A repair loop whose ceiling is implicit is the failure mode that makes
// people disable the whole thing, and #102's own third complaint is exactly
// that: "nobody declared how many repair rounds a package gets before it goes
// back to a human, so the answer is 'until the operator gets tired'".
//
// So the bound here is two numbers and a stated behaviour at the ceiling, all
// three enforced by Decide and all three carried in the record it composes:
//
//   - MaxAttempts — how many repair rounds one run gets. Two.
//   - Window — how long the loop stays live, measured from the run's FIRST
//     gate rejection. Twenty-four hours.
//   - At either ceiling: DestinationHuman. Never a stall, never a silent
//     stop, never one more round.
//
// The two bounds are separate because they fail differently. The attempt
// count bounds SPEND — each repair round is a billable session. The window
// bounds STALENESS — a repair dispatched against a day-old rejection is
// repairing a diagnosis that may no longer hold, because main moved, the
// actor's session window rolled, or the branch it was measured against is
// gone. A run can hit either without hitting the other, and
// TestTheWindowIsEnforcedSeparatelyFromTheCeiling pins that they are really
// two.
//
// # Routed, not unattended — deliberately
//
// This package DECIDES and RECORDS. It does not dispatch, and that is a
// decision rather than an unfinished edge.
//
// The plan carried a recorded risk against this work: the last cycle's four
// gate failures were all cases where the actor could not run the tool that
// would have shown them, and a repair node on the same host cannot run it
// either. That risk is now measured rather than suspected —
// docs/baselines/dispatch-posture.md and #119 — and the measurement is what
// this package encodes: a lane whose advertised capability surface does not
// show it can WRITE and can RUN THE FAILING SUITE is refused a repair and
// sent to a person, because a repair that cannot verify its own fix produces
// a second unverified claim, which is precisely what the merge gate exists to
// stop being enough.
//
// Two further reasons the dispatch itself stays a human-triggered step:
//
//  1. The write path through the codex bridges is unproven. #18 stays open
//     until a `workspace-write` dispatch actually lands a patch; the probe
//     that proved shell exec works there was read-only.
//  2. A repair dispatch is a dispatch, so it inherits the workflow-scope
//     boundary. A gate that failed because of CI configuration is a case the
//     loop must hand to a person — see GuardedPathPrefixes.
//
// What the loop is NOT allowed to be is "routed to the operator's session by
// default". Today the whole decision — is this repairable, by whom, how many
// times, and when do we stop — lives in a person's head at the moment they
// read a red gate. After this, the decision is made by a deterministic
// function over recorded facts, it is written down under its own bound, and
// the human step is reduced to executing a dispatch the system already
// chose and justified. That is the difference this package delivers, and it
// is smaller than an autonomous loop and more honest than one.
//
// # Why the record is derived
//
// Decide is a pure function of already-recorded facts: a suite's exit code
// (itself a derived record from internal/handover), the run's own prior
// routings, the paths the handover commit changed, and the lane's advertised
// capability surface. Same inputs, same answer, every time. PRD §10.4 admits
// `derived` records from exactly that producer, so the routing is composed as
// a validator-origin decision record and nothing else. It is not `proposed`
// (an agent's word), not `observed` (nothing was measured here that was not
// already measured elsewhere), and emphatically not `confirmed` — a human
// deciding to merge is still a human's transaction, and this package never
// pretends to have made one.
package repair
