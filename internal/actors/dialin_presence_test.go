package actors_test

import (
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
)

// TestNeverDialledIsNotTheSameAsDropped is task t6's acceptance criterion 2
// at its smallest: an actor that has never dialled in must be
// DISTINGUISHABLE from one whose connection dropped. A two-valued
// present/absent answer would report a bridge nobody ever configured and a
// bridge that died an hour ago identically, which is precisely the
// indistinguishability issue #136 is evidence for.
func TestNeverDialledIsNotTheSameAsDropped(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	dropped := now.Add(-10 * time.Minute)

	never := actors.ClassifyDialInPresence(nil, now)
	if never != actors.DialInNeverDialled {
		t.Fatalf("no presence row: state = %q, want %q", never, actors.DialInNeverDialled)
	}
	stale := actors.ClassifyDialInPresence(&dropped, now)
	if stale != actors.DialInDisconnected {
		t.Fatalf("stale presence row: state = %q, want %q", stale, actors.DialInDisconnected)
	}
	if never == stale {
		t.Fatal("never-dialled and disconnected classified identically")
	}
}

// TestPresenceWithinTheWindowIsConnected pins the freshness boundary,
// including that it is INCLUSIVE — Store.InboundActorAvailable's SQL
// predicate is `last_seen_at >= $3`, and a classification that disagreed at
// the boundary would let the view and dispatch resolution differ by exactly
// one instant.
func TestPresenceWithinTheWindowIsConnected(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		lastSeen time.Time
		want     actors.DialInPresenceState
	}{
		{"just polled", now, actors.DialInConnected},
		{"one poll cycle ago", now.Add(-25 * time.Second), actors.DialInConnected},
		{"exactly at the cutoff", actors.DialInPresenceCutoff(now), actors.DialInConnected},
		{"one nanosecond past the cutoff", actors.DialInPresenceCutoff(now).Add(-time.Nanosecond), actors.DialInDisconnected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen := tc.lastSeen
			if got := actors.ClassifyDialInPresence(&seen, now); got != tc.want {
				t.Fatalf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDialInFreshnessCoversAPollCycle guards the one property that makes the
// window a number rather than an opinion: handleInboundPoll holds each long
// poll open for 25 seconds and refreshes presence at the start of the next
// one, so a window shorter than that cadence would report a perfectly
// healthy bridge as disconnected between two polls.
func TestDialInFreshnessCoversAPollCycle(t *testing.T) {
	const pollDeadline = 25 * time.Second
	if actors.DialInFreshness <= pollDeadline {
		t.Fatalf("DialInFreshness = %s, which is not longer than the %s inbound poll deadline: a healthy bridge would flicker absent between polls",
			actors.DialInFreshness, pollDeadline)
	}
}
