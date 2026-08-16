package actors

import "time"

// Dial-in presence: the answer to "is this bridge connected right now".
//
// Retiring `actors.endpoint_ref` (migration 0036, issue #121) removes the
// decayed-address failure, but it does not remove the QUESTION the address
// used to answer. "Is this bridge reachable?" becomes "is this bridge dialled
// in right now?" — and until this file existed nothing answered it outside
// dispatch resolution, so an un-dialled bridge was discovered by a failing
// dispatch. That is the same blind spot the stored address had, wearing
// different clothes: issue #136 records five actors going unreachable with
// nothing detecting it, surfaced only because an unrelated deploy script hit
// an ssh error.
//
// The definition of "connected" lives HERE, in one place, because there must
// be exactly one of it. internal/worker/registry.go decides whether an actor
// is dispatchable through dial-in; internal/api's read-only presence view
// tells an operator whether it is connected. If those two used different
// windows, the view would say connected while dispatch said otherwise — and a
// view that disagrees with the mechanism it describes is worse than no view.

// DialInFreshness is how recently a bridge must have polled
// POST /v1alpha1/inbound/poll for the control plane to treat it as connected.
//
// Its size is set by the poll itself, not by taste: handleInboundPoll holds a
// long poll open for 25 seconds and touches presence at the START of each
// one, so a healthy bridge refreshes its row roughly every 25 seconds plus
// one round trip. 30 seconds is that cadence plus a small margin — long
// enough that a healthy bridge never flickers absent between polls, short
// enough that a dead one is legible within about half a minute.
const DialInFreshness = 30 * time.Second

// DialInPresenceCutoff is the instant a presence row must be at or after to
// count as current. Both the dispatch-resolution path (DBRegistry.Resolve,
// via Store.InboundActorAvailable) and the read-only presence view call this
// rather than each subtracting their own window.
//
// The boundary is INCLUSIVE — a row exactly at the cutoff counts as present —
// matching Store.InboundActorAvailable's `last_seen_at >= $3`.
func DialInPresenceCutoff(now time.Time) time.Time {
	return now.Add(-DialInFreshness)
}

// DialInPresenceState is what an operator is actually asking about. The three
// values are deliberately not two: an actor that has NEVER dialled in is a
// different fact from one whose connection dropped, and collapsing them would
// report a bridge that was never configured and a bridge that died an hour
// ago identically.
type DialInPresenceState string

const (
	// DialInNeverDialled: no presence row exists at all. This bridge has not
	// dialled in once since the row would have been created — a configuration
	// or deployment fact, not an outage.
	DialInNeverDialled DialInPresenceState = "never_dialled"
	// DialInConnected: the last poll is within DialInFreshness. Dispatch
	// through the mailbox will resolve for this actor.
	DialInConnected DialInPresenceState = "connected"
	// DialInDisconnected: this bridge HAS dialled in before and has stopped.
	// The last-seen instant is the interesting number and is always reported
	// alongside this state.
	DialInDisconnected DialInPresenceState = "disconnected"
)

// ClassifyDialInPresence maps a last-seen instant (nil when no presence row
// exists) to the state an operator reads. now is passed rather than read so
// the classification is testable and so one response classifies every actor
// against a single observation instant.
func ClassifyDialInPresence(lastSeen *time.Time, now time.Time) DialInPresenceState {
	if lastSeen == nil {
		return DialInNeverDialled
	}
	if lastSeen.Before(DialInPresenceCutoff(now)) {
		return DialInDisconnected
	}
	return DialInConnected
}
