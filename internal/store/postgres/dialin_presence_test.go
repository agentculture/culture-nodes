package postgres_test

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// insertPresenceActor registers one actor identity and returns its key. The
// key is ULID-suffixed because inbound_authentication is NOT namespaced —
// its primary key is (party_kind, party_key) across the whole database — so
// two tests using a fixed key would collide even in separate namespaces.
func insertPresenceActor(t *testing.T, s *postgres.Store, nsID, name string) string {
	t.Helper()
	key := "test/" + name + "-" + store.NewULID()
	_, err := s.Pool().Exec(context.Background(),
		`INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		 VALUES ($1, $2, $3, 1, 'agent', 'http')`, store.NewULID(), nsID, key)
	if err != nil {
		t.Fatalf("insert actor %s: %v", key, err)
	}
	return key
}

func presenceFor(t *testing.T, rows []postgres.DialInPresenceRow, key string) postgres.DialInPresenceRow {
	t.Helper()
	for _, row := range rows {
		if row.ActorKey == key {
			return row
		}
	}
	t.Fatalf("actor %q is missing from the dial-in presence view (%d rows)", key, len(rows))
	return postgres.DialInPresenceRow{}
}

// TestDialInPresenceListsAbsentActorsToo is the store half of acceptance
// criteria 1 and 2. Today exactly one actor in production has ever dialled
// in and ten have not, so a view built from presence rows alone would answer
// "who is connected" with a list that silently omits every actor an operator
// is worried about.
func TestDialInPresenceListsAbsentActorsToo(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "dialin-presence")

	connected := insertPresenceActor(t, s, ns.ID, "connected")
	dropped := insertPresenceActor(t, s, ns.ID, "dropped")
	never := insertPresenceActor(t, s, ns.ID, "never")

	now := time.Now().UTC()
	droppedAt := now.Add(-10 * time.Minute)
	for key, seen := range map[string]time.Time{connected: now, dropped: droppedAt} {
		if _, err := s.Pool().Exec(ctx,
			`INSERT INTO inbound_actor_presence (namespace_id, actor_key, last_seen_at) VALUES ($1,$2,$3)`,
			ns.ID, key, seen); err != nil {
			t.Fatalf("insert presence for %s: %v", key, err)
		}
	}

	rows, err := s.DialInPresence(ctx, ns.ID, actors.DialInPresenceCutoff(now))
	if err != nil {
		t.Fatalf("DialInPresence: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (every registered actor, dialled in or not)", len(rows))
	}

	if row := presenceFor(t, rows, connected); !row.Connected || row.LastSeenAt == nil {
		t.Fatalf("connected actor: connected=%v last_seen=%v", row.Connected, row.LastSeenAt)
	}

	// Criterion 2: dropped carries the last-seen instant; never-dialled has
	// none at all, and the two are not the same row shape.
	stale := presenceFor(t, rows, dropped)
	if stale.Connected {
		t.Fatal("an actor last seen 10 minutes ago is reported as connected")
	}
	if stale.LastSeenAt == nil {
		t.Fatal("a dropped connection must carry its last-seen instant")
	}
	if got := stale.LastSeenAt.Sub(droppedAt); got > time.Second || got < -time.Second {
		t.Fatalf("last_seen_at = %s, want ~%s", stale.LastSeenAt, droppedAt)
	}

	absent := presenceFor(t, rows, never)
	if absent.Connected || absent.LastSeenAt != nil {
		t.Fatalf("never-dialled actor: connected=%v last_seen=%v (want false/nil)", absent.Connected, absent.LastSeenAt)
	}
}

// TestDialInPresenceAgreesWithDispatchResolution is the design note's
// requirement made executable: the view and internal/worker/registry.go must
// not carry two definitions of "connected". Both compute their cutoff with
// actors.DialInPresenceCutoff and both compare with `last_seen_at >=`, so
// this walks a row across the boundary and asserts the two surfaces never
// disagree — including exactly AT the cutoff, where an exclusive comparison
// on one side would differ by one instant.
func TestDialInPresenceAgreesWithDispatchResolution(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "dialin-agree")
	key := insertPresenceActor(t, s, ns.ID, "boundary")

	now := time.Now().UTC()
	cutoff := actors.DialInPresenceCutoff(now)
	for _, lastSeen := range []time.Time{
		now,
		now.Add(-actors.DialInFreshness / 2),
		cutoff,
		cutoff.Add(-time.Millisecond),
		now.Add(-time.Hour),
	} {
		if _, err := s.Pool().Exec(ctx,
			`INSERT INTO inbound_actor_presence (namespace_id, actor_key, last_seen_at) VALUES ($1,$2,$3)
			 ON CONFLICT (namespace_id, actor_key) DO UPDATE SET last_seen_at = excluded.last_seen_at`,
			ns.ID, key, lastSeen); err != nil {
			t.Fatalf("set presence: %v", err)
		}

		dispatchable, err := s.InboundActorAvailable(ctx, ns.ID, key, cutoff)
		if err != nil {
			t.Fatalf("InboundActorAvailable: %v", err)
		}
		rows, err := s.DialInPresence(ctx, ns.ID, cutoff)
		if err != nil {
			t.Fatalf("DialInPresence: %v", err)
		}
		viewed := presenceFor(t, rows, key).Connected
		if viewed != dispatchable {
			t.Fatalf("last_seen=%s: the presence view says connected=%v while dispatch resolution says %v",
				lastSeen, viewed, dispatchable)
		}
	}
}

// TestDialInPresenceSurfacesRevocationAndLockout covers the third state an
// operator at 03:00 has to tell apart from an outage: a revoked or locked-out
// credential looks exactly as absent as a crashed bridge in presence alone,
// and has a completely different remedy.
func TestDialInPresenceSurfacesRevocationAndLockout(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "dialin-credential")

	revoked := insertPresenceActor(t, s, ns.ID, "revoked")
	lockedOut := insertPresenceActor(t, s, ns.ID, "locked")
	noCredential := insertPresenceActor(t, s, ns.ID, "uncredentialed")

	now := time.Now().UTC()
	digest := sha256.Sum256([]byte("verifier"))
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO inbound_authentication (party_kind, party_key, verifier_sha256, revoked_at, issued_at, issuance_count)
		VALUES ('actor', $1, $2, $3, $4, 1)`, revoked, digest[:], now.Add(-time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("insert revoked credential: %v", err)
	}
	defer s.Pool().Exec(ctx, `DELETE FROM inbound_authentication WHERE party_key=$1`, revoked)
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO inbound_authentication (party_kind, party_key, verifier_sha256, locked_until, failure_count)
		VALUES ('actor', $1, $2, $3, 5)`, lockedOut, digest[:], now.Add(time.Minute)); err != nil {
		t.Fatalf("insert locked credential: %v", err)
	}
	defer s.Pool().Exec(ctx, `DELETE FROM inbound_authentication WHERE party_key=$1`, lockedOut)

	rows, err := s.DialInPresence(ctx, ns.ID, actors.DialInPresenceCutoff(now))
	if err != nil {
		t.Fatalf("DialInPresence: %v", err)
	}

	revokedRow := presenceFor(t, rows, revoked).Credential
	if revokedRow == nil || revokedRow.RevokedAt == nil {
		t.Fatalf("revoked credential: %+v, want a revoked_at instant", revokedRow)
	}
	if !revokedRow.Issued || revokedRow.IssuanceCount != 1 {
		t.Fatalf("issuance provenance: issued=%v count=%d", revokedRow.Issued, revokedRow.IssuanceCount)
	}

	lockedRow := presenceFor(t, rows, lockedOut).Credential
	if lockedRow == nil || lockedRow.LockedUntil == nil || lockedRow.FailureCount != 5 {
		t.Fatalf("locked-out credential: %+v, want locked_until and failure_count 5", lockedRow)
	}
	// A hand-provisioned record (issued_at NULL) is refused as
	// `not_control_plane_issued`, so this flag is itself a reason a bridge
	// will never dial in.
	if lockedRow.Issued {
		t.Fatal("a credential with no issued_at must not report itself as control-plane issued")
	}

	if row := presenceFor(t, rows, noCredential); row.Credential != nil {
		t.Fatalf("actor with no inbound_authentication row: credential = %+v, want nil", row.Credential)
	}
}

// TestDialInPresenceDoesNotCreatePresence pins the read-only constraint at
// the layer that could violate it: reading must never touch
// inbound_actor_presence. A view that refreshed last_seen_at would report
// every actor connected the moment an operator looked at it.
func TestDialInPresenceDoesNotCreatePresence(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "dialin-readonly")
	key := insertPresenceActor(t, s, ns.ID, "untouched")

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if _, err := s.DialInPresence(ctx, ns.ID, actors.DialInPresenceCutoff(now)); err != nil {
			t.Fatalf("DialInPresence: %v", err)
		}
	}
	var presenceRows int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM inbound_actor_presence WHERE namespace_id=$1 AND actor_key=$2`,
		ns.ID, key).Scan(&presenceRows); err != nil {
		t.Fatalf("count presence rows: %v", err)
	}
	if presenceRows != 0 {
		t.Fatalf("presence rows = %d after reading the view, want 0 — the read created presence", presenceRows)
	}
}
