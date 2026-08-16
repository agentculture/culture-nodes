package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store"
)

// Task t6 (issues #136, #121): retiring the stored participant address does
// not retire the question the address answered. These tests own the operator
// surface that answers it — "which bridges are dialled in right now" — and
// its three acceptance criteria:
//
//  1. an operator can list current dial-in presence without dispatching;
//  2. never-dialled and dropped are distinguishable, and dropped carries the
//     last-seen instant;
//  3. the answer comes from the control plane, never from probing a
//     participant.

type dialInPresenceItem struct {
	ActorKey             string   `json:"actor_key"`
	ActorID              string   `json:"actor_id"`
	Revision             int32    `json:"revision"`
	Kind                 string   `json:"kind"`
	Presence             string   `json:"presence"`
	LastSeenAt           *string  `json:"last_seen_at"`
	SecondsSinceLastSeen *float64 `json:"seconds_since_last_seen"`
	Credential           *struct {
		Issued        bool    `json:"issued"`
		IssuedAt      *string `json:"issued_at"`
		IssuanceCount int     `json:"issuance_count"`
		Revoked       bool    `json:"revoked"`
		RevokedAt     *string `json:"revoked_at"`
		LockedOut     bool    `json:"locked_out"`
		LockedUntil   *string `json:"locked_until"`
		FailureCount  int     `json:"failure_count"`
	} `json:"credential"`
}

type dialInPresenceList struct {
	ObservedAt    time.Time            `json:"observed_at"`
	WindowSeconds float64              `json:"window_seconds"`
	Connected     int                  `json:"connected"`
	Disconnected  int                  `json:"disconnected"`
	NeverDialled  int                  `json:"never_dialled"`
	Items         []dialInPresenceItem `json:"items"`
}

// registerDialInActor inserts one actor identity with the given endpoint_ref
// and returns its actor_key. The endpoint is supplied so a test can register
// an address the presence handler must NOT contact.
func (f *fixture) registerDialInActor(name, endpointRef string) string {
	f.t.Helper()
	key := "test/" + name + "-" + store.NewULID()
	_, err := f.store.Pool().Exec(context.Background(),
		`INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref)
		 VALUES ($1, $2, $3, 1, 'agent', 'http', $4)`,
		store.NewULID(), f.nsID, key, endpointRef)
	if err != nil {
		f.t.Fatalf("insert actor %s: %v", key, err)
	}
	return key
}

func (f *fixture) setDialInPresence(actorKey string, lastSeen time.Time) {
	f.t.Helper()
	_, err := f.store.Pool().Exec(context.Background(),
		`INSERT INTO inbound_actor_presence (namespace_id, actor_key, last_seen_at) VALUES ($1,$2,$3)
		 ON CONFLICT (namespace_id, actor_key) DO UPDATE SET last_seen_at = excluded.last_seen_at`,
		f.nsID, actorKey, lastSeen)
	if err != nil {
		f.t.Fatalf("set presence for %s: %v", actorKey, err)
	}
}

func (f *fixture) getDialInPresence() dialInPresenceList {
	f.t.Helper()
	resp, err := f.client.Get(f.url("/v1alpha1/dial-in-presence"))
	if err != nil {
		f.t.Fatalf("GET /v1alpha1/dial-in-presence: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("GET /v1alpha1/dial-in-presence: status = %d, want 200", resp.StatusCode)
	}
	var out dialInPresenceList
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		f.t.Fatalf("decode presence list: %v", err)
	}
	return out
}

func itemFor(t *testing.T, list dialInPresenceList, key string) dialInPresenceItem {
	t.Helper()
	for _, item := range list.Items {
		if item.ActorKey == key {
			return item
		}
	}
	t.Fatalf("actor %q missing from the dial-in presence view (%d items)", key, len(list.Items))
	return dialInPresenceItem{}
}

// TestDialInPresenceListsWhoIsConnectedWithoutDispatching is acceptance
// criterion 1: an operator gets the answer from one GET, and nothing about
// that GET creates a run, an attempt, or a mailbox row.
func TestDialInPresenceListsWhoIsConnectedWithoutDispatching(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	connected := f.registerDialInActor("connected", "")
	f.setDialInPresence(connected, time.Now().UTC())

	before := f.countRows(ctx, `SELECT count(*) FROM runs WHERE namespace_id=$1`)
	beforeMailbox := f.countRows(ctx, `SELECT count(*) FROM inbound_actor_mailbox WHERE namespace_id=$1`)

	list := f.getDialInPresence()

	if list.WindowSeconds != actors.DialInFreshness.Seconds() {
		t.Fatalf("window_seconds = %v, want %v (the same window dispatch resolution uses)",
			list.WindowSeconds, actors.DialInFreshness.Seconds())
	}
	if item := itemFor(t, list, connected); item.Presence != string(actors.DialInConnected) {
		t.Fatalf("presence = %q, want %q", item.Presence, actors.DialInConnected)
	}
	if list.Connected != 1 {
		t.Fatalf("connected count = %d, want 1", list.Connected)
	}

	if after := f.countRows(ctx, `SELECT count(*) FROM runs WHERE namespace_id=$1`); after != before {
		t.Fatalf("runs = %d after reading presence, was %d — the view dispatched something", after, before)
	}
	if after := f.countRows(ctx, `SELECT count(*) FROM inbound_actor_mailbox WHERE namespace_id=$1`); after != beforeMailbox {
		t.Fatalf("mailbox rows = %d after reading presence, was %d — the view enqueued work", after, beforeMailbox)
	}
}

// TestDialInPresenceDistinguishesNeverDialledFromDropped is acceptance
// criterion 2. Both actors are absent; only one of them is a regression.
func TestDialInPresenceDistinguishesNeverDialledFromDropped(t *testing.T) {
	f := newFixture(t)

	dropped := f.registerDialInActor("dropped", "")
	never := f.registerDialInActor("never", "")
	droppedAt := time.Now().UTC().Add(-17 * time.Minute)
	f.setDialInPresence(dropped, droppedAt)

	list := f.getDialInPresence()

	droppedItem := itemFor(t, list, dropped)
	if droppedItem.Presence != string(actors.DialInDisconnected) {
		t.Fatalf("dropped actor: presence = %q, want %q", droppedItem.Presence, actors.DialInDisconnected)
	}
	if droppedItem.LastSeenAt == nil {
		t.Fatal("a dropped connection must carry last_seen_at — when it dropped is the whole answer")
	}
	if droppedItem.SecondsSinceLastSeen == nil || *droppedItem.SecondsSinceLastSeen < 900 {
		t.Fatalf("seconds_since_last_seen = %v, want ~1020", droppedItem.SecondsSinceLastSeen)
	}

	neverItem := itemFor(t, list, never)
	if neverItem.Presence != string(actors.DialInNeverDialled) {
		t.Fatalf("never-dialled actor: presence = %q, want %q", neverItem.Presence, actors.DialInNeverDialled)
	}
	if neverItem.LastSeenAt != nil || neverItem.SecondsSinceLastSeen != nil {
		t.Fatalf("never-dialled actor carries last_seen_at=%v seconds=%v; absence must render as absence, never as 0",
			neverItem.LastSeenAt, neverItem.SecondsSinceLastSeen)
	}
	if neverItem.Presence == droppedItem.Presence {
		t.Fatal("never-dialled and dropped render identically")
	}
	if list.Disconnected != 1 || list.NeverDialled != 1 {
		t.Fatalf("counts: disconnected=%d never_dialled=%d, want 1 and 1", list.Disconnected, list.NeverDialled)
	}
}

// TestDialInPresenceIsServedWithoutProbingAnyParticipant is acceptance
// criterion 3, pinned two ways at once.
//
// The registered actor's endpoint_ref points at a live HTTP server that
// records every request it receives and fails the test if it gets one; and
// http.DefaultTransport is swapped for a RoundTripper that refuses (and
// records) every outbound request for the duration. The fixture's own client
// is httptest.Server.Client(), which carries its own transport, so the test's
// request to the API under test is unaffected.
//
// Together: if the handler contacted the registered address by any of the
// ordinary means, one of the two counters would be non-zero.
func TestDialInPresenceIsServedWithoutProbingAnyParticipant(t *testing.T) {
	var probed int64
	participant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&probed, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer participant.Close()

	outbound := &refusingTransport{}
	original := http.DefaultTransport
	http.DefaultTransport = outbound
	defer func() { http.DefaultTransport = original }()

	f := newFixture(t)
	// Every presence state, so no branch of the handler escapes the pin.
	live := f.registerDialInActor("live", participant.URL)
	stale := f.registerDialInActor("stale", participant.URL)
	f.registerDialInActor("absent", participant.URL)
	f.setDialInPresence(live, time.Now().UTC())
	f.setDialInPresence(stale, time.Now().UTC().Add(-time.Hour))

	list := f.getDialInPresence()
	if len(list.Items) < 3 {
		t.Fatalf("items = %d, want at least 3", len(list.Items))
	}

	if n := atomic.LoadInt64(&probed); n != 0 {
		t.Fatalf("the presence handler sent %d request(s) to a registered participant address; presence is read, never probed", n)
	}
	if n := outbound.count(); n != 0 {
		t.Fatalf("the presence handler made %d outbound request(s) through the default transport (%s); presence is read, never probed",
			n, outbound.lastURL())
	}

	// And the response contains no address at all — a view that leaked the
	// endpoint back would quietly re-create the thing #121 is retiring.
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), participant.URL) {
		t.Fatalf("the presence response carries the participant's address: %s", raw)
	}
}

// TestDialInPresenceSeparatesLockedOutFromAbsent covers the operator's other
// 03:00 question. A revoked or locked-out bridge is absent from presence for
// a reason that has nothing to do with its process being down, and the two
// need completely different remedies.
func TestDialInPresenceSeparatesLockedOutFromAbsent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	revoked := f.registerDialInActor("revoked", "")
	lockedOut := f.registerDialInActor("locked", "")
	plainlyDown := f.registerDialInActor("down", "")

	now := time.Now().UTC()
	digest := make([]byte, 32)
	digest[0] = 7
	if _, err := f.store.Pool().Exec(ctx, `
		INSERT INTO inbound_authentication (party_kind, party_key, verifier_sha256, revoked_at, issued_at, issuance_count)
		VALUES ('actor', $1, $2, $3, $4, 2)`, revoked, digest, now.Add(-time.Hour), now.Add(-72*time.Hour)); err != nil {
		t.Fatalf("insert revoked credential: %v", err)
	}
	defer f.store.Pool().Exec(ctx, `DELETE FROM inbound_authentication WHERE party_key=$1`, revoked)
	if _, err := f.store.Pool().Exec(ctx, `
		INSERT INTO inbound_authentication (party_kind, party_key, verifier_sha256, locked_until, failure_count, issued_at, issuance_count)
		VALUES ('actor', $1, $2, $3, 5, $4, 1)`, lockedOut, digest, now.Add(2*time.Minute), now.Add(-time.Hour)); err != nil {
		t.Fatalf("insert locked credential: %v", err)
	}
	defer f.store.Pool().Exec(ctx, `DELETE FROM inbound_authentication WHERE party_key=$1`, lockedOut)

	list := f.getDialInPresence()

	// All three are absent from presence — that is exactly the point.
	for _, key := range []string{revoked, lockedOut, plainlyDown} {
		if item := itemFor(t, list, key); item.Presence != string(actors.DialInNeverDialled) {
			t.Fatalf("%s: presence = %q, want %q", key, item.Presence, actors.DialInNeverDialled)
		}
	}

	revokedItem := itemFor(t, list, revoked)
	if revokedItem.Credential == nil || !revokedItem.Credential.Revoked || revokedItem.Credential.RevokedAt == nil {
		t.Fatalf("revoked actor: credential = %+v, want revoked with an instant", revokedItem.Credential)
	}
	lockedItem := itemFor(t, list, lockedOut)
	if lockedItem.Credential == nil || !lockedItem.Credential.LockedOut || lockedItem.Credential.FailureCount != 5 {
		t.Fatalf("locked-out actor: credential = %+v, want locked_out with failure_count 5", lockedItem.Credential)
	}
	if lockedItem.Credential.Revoked {
		t.Fatal("a locked-out credential must not also report itself revoked")
	}
	if item := itemFor(t, list, plainlyDown); item.Credential != nil {
		t.Fatalf("actor with no credential record: credential = %+v, want absent", item.Credential)
	}

	// No verifier material anywhere in the rendering.
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{"verifier", "digest", "sha256", "env_name"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("the presence response renders %q; a read surface must carry no verifier material: %s", forbidden, raw)
		}
	}
}

// refusingTransport fails every request and remembers that it was asked.
type refusingTransport struct {
	calls atomic.Int64
	url   atomic.Value // string
}

func (t *refusingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	t.url.Store(r.URL.String())
	return nil, errors.New("outbound HTTP is not allowed while reading dial-in presence")
}

func (t *refusingTransport) count() int64 { return t.calls.Load() }

func (t *refusingTransport) lastURL() string {
	if v, ok := t.url.Load().(string); ok {
		return v
	}
	return ""
}

// countRows runs a single-count query scoped to this fixture's namespace.
func (f *fixture) countRows(ctx context.Context, query string) int {
	f.t.Helper()
	var n int
	if err := f.store.Pool().QueryRow(ctx, query, f.nsID).Scan(&n); err != nil {
		f.t.Fatalf("count (%s): %v", query, err)
	}
	return n
}
