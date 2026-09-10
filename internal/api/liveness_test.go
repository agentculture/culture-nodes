package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/mesh"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Actor liveness on the read surfaces and the resume lane (plan
// loop-closure t10; spec c23/c33, decision c43's AND rule).
//
// The acceptance these carry:
//
//  1. resume ALONE does not reopen a lane: the lock clears but session_ok
//     stays false, and the router's verdict (`live`) stays false;
//  2. resume FOLLOWED BY a healthy collector write does reopen it;
//  3. GET /v1alpha1/mesh and GET /v1alpha1/actors{,/{id}} show session_ok,
//     reason, mode per actor, from the persisted row;
//  4. the collector's sink adapter really writes a source=collector row.

type livenessWire struct {
	SessionOK *bool  `json:"session_ok"`
	Reason    string `json:"reason"`
	Mode      string `json:"mode"`
	Source    string `json:"source"`
	Locked    bool   `json:"locked"`
	Live      bool   `json:"live"`
}

type actorLivenessWire struct {
	ID       string        `json:"id"`
	ActorKey string        `json:"actor_key"`
	Liveness *livenessWire `json:"liveness"`
}

func lockLane(t *testing.T, f *fixture, actorKey string) {
	t.Helper()
	if _, err := f.store.LockActorLiveness(context.Background(), postgres.LockActorLivenessInput{
		NamespaceID: f.nsID, ActorKey: actorKey, Reason: "refresh_token_spent",
		CheckedAt: time.Now().UTC(), RunID: "run-x", AttemptID: "attempt-x",
	}); err != nil {
		t.Fatalf("LockActorLiveness: %v", err)
	}
}

func TestResumeAloneDoesNotReopenALockedLaneButAHealthyCollectorWriteAfterItDoes(t *testing.T) {
	f := newFixtureWithActorRegistrationAuth(t, actorRegistrationSecret)
	actorID := f.insertActor("locked-lane")
	actorKey := actorKeyOfRow(t, f, actorID)
	lockLane(t, f, actorKey)

	var got actorLivenessWire
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/actors/"+actorID+"/resume"), actorRegistrationSecret, nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST resume: %d: %s", resp.StatusCode, body)
	}
	if got.Liveness == nil {
		t.Fatalf("resume response carries no liveness block: %s", body)
	}
	if got.Liveness.Locked {
		t.Errorf("locked = true after resume, want false: %s", body)
	}
	if got.Liveness.SessionOK == nil || *got.Liveness.SessionOK {
		t.Errorf("session_ok = %v after resume, want still false (resume is not a measurement): %s", got.Liveness.SessionOK, body)
	}
	if got.Liveness.Source != postgres.LivenessSourceResume {
		t.Errorf("source = %q, want resume", got.Liveness.Source)
	}
	if got.Liveness.Live {
		t.Errorf("live = true after resume alone, want false: the last fact is a fresh session_ok=false, and c43's unlock is an AND: %s", body)
	}
	// The dispatch site agrees.
	row, ok, err := f.store.ActorLiveness(context.Background(), f.nsID, actorKey)
	if err != nil || !ok {
		t.Fatalf("ActorLiveness = (%v, %v)", ok, err)
	}
	if row.Live(time.Now().UTC()) {
		t.Error("store row reads live after resume alone")
	}

	// Now the bridge's next probe says the session is fine: the collector's
	// sink writes it, and the lane is live.
	sink := api.CollectorLivenessSink(f.store, f.nsID)
	okTrue := true
	if err := sink.RecordLiveness(context.Background(), actorKey, mesh.Liveness{
		SessionOK: &okTrue, Reason: "ok", Mode: "LOCK", CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("sink.RecordLiveness: %v", err)
	}
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors/"+actorID), nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET actor: %d: %s", resp.StatusCode, body)
	}
	if got.Liveness == nil || !got.Liveness.Live || got.Liveness.Locked || got.Liveness.Source != postgres.LivenessSourceCollector {
		t.Fatalf("after resume + healthy fact, liveness = %+v, want live, unlocked, source=collector", got.Liveness)
	}
}

// The other order: a healthy bridge fact WITHOUT a resume leaves the lock in
// place. The bridge is an input that can set the row and a precondition for
// clearing it; it is not a party that clears the control-plane lock.
func TestAHealthyCollectorWriteAloneDoesNotClearTheLock(t *testing.T) {
	f := newFixture(t)
	actorID := f.insertActor("still-locked")
	actorKey := actorKeyOfRow(t, f, actorID)
	lockLane(t, f, actorKey)
	okTrue := true
	if err := api.CollectorLivenessSink(f.store, f.nsID).RecordLiveness(context.Background(), actorKey, mesh.Liveness{
		SessionOK: &okTrue, Reason: "ok", Mode: "LOCK", CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	var got actorLivenessWire
	if resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors/"+actorID), nil, &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET actor: %d: %s", resp.StatusCode, body)
	}
	if got.Liveness == nil || !got.Liveness.Locked || got.Liveness.Live {
		t.Fatalf("liveness = %+v, want still locked and not live until a human resumes", got.Liveness)
	}
	if got.Liveness.SessionOK == nil || !*got.Liveness.SessionOK {
		t.Errorf("session_ok = %v, want true: the fact itself IS updated by the bridge", got.Liveness.SessionOK)
	}
}

func TestActorsListAndMeshShowLivenessPerActor(t *testing.T) {
	f := newFixture(t)
	observed := f.insertActor("observed-lane")
	observedKey := actorKeyOfRow(t, f, observed)
	silent := f.insertActor("silent-lane")
	okFalse := false
	if _, err := f.store.RecordActorLiveness(context.Background(), postgres.RecordActorLivenessInput{
		NamespaceID: f.nsID, ActorKey: observedKey, SessionOK: &okFalse, Reason: "credential_expired", Mode: "CHECK",
		CheckedAt: time.Now().UTC().Add(-time.Hour), Source: postgres.LivenessSourceCollector,
	}); err != nil {
		t.Fatal(err)
	}

	var list struct {
		Items []actorLivenessWire `json:"items"`
	}
	if resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors"), nil, &list); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET actors: %d: %s", resp.StatusCode, body)
	}
	byID := map[string]actorLivenessWire{}
	for _, item := range list.Items {
		byID[item.ID] = item
	}
	if l := byID[observed].Liveness; l == nil || l.SessionOK == nil || *l.SessionOK || l.Reason != "credential_expired" || l.Mode != "CHECK" {
		t.Fatalf("observed actor liveness in list = %+v, want session_ok=false reason=credential_expired mode=CHECK", l)
	}
	if !byID[observed].Liveness.Live {
		t.Error("an hour-old session_ok=false must read live: stale rows never refuse")
	}
	if byID[silent].Liveness != nil {
		t.Errorf("never-observed actor carries liveness %+v, want absent", byID[silent].Liveness)
	}

	// The mesh read model renders the same row per actor, beside the
	// bridge's own fact.
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"preflight":{"host":{"hostname":"h","deployment":{},` +
			`"liveness":{"session_ok":false,"reason":"refresh_token_spent","checked_at":"2026-09-07T10:00:00Z","mode":"LOCK"}}}}`))
	}))
	defer bridge.Close()
	collector := mesh.New(mesh.Config{Interval: time.Hour, ProbeTimeout: time.Second, MaxConcurrency: 1,
		Liveness: api.CollectorLivenessSink(f.store, f.nsID)})
	collector.SetTargets([]mesh.Target{{Key: observedKey, URL: bridge.URL}})
	collector.Collect(context.Background())

	srv, err := api.NewServer(f.store, f.nsID, api.WithMeshCollector(collector))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/v1alpha1/mesh")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var meshOut struct {
		Actors []struct {
			ActorKey string        `json:"actor_key"`
			Liveness *livenessWire `json:"liveness"`
			Bridge   struct {
				Liveness *livenessWire `json:"liveness"`
			} `json:"bridge"`
		} `json:"actors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meshOut); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range meshOut.Actors {
		if a.ActorKey != observedKey {
			continue
		}
		found = true
		// The collector's probe just persisted the bridge fact, so the
		// control-plane row now carries it: refresh_token_spent, LOCK.
		if a.Liveness == nil || a.Liveness.Reason != "refresh_token_spent" || a.Liveness.Mode != "LOCK" || a.Liveness.Source != postgres.LivenessSourceCollector {
			t.Errorf("mesh actor liveness = %+v, want the persisted collector row", a.Liveness)
		}
		if a.Bridge.Liveness == nil || a.Bridge.Liveness.Reason != "refresh_token_spent" {
			t.Errorf("mesh bridge.liveness = %+v, want the bridge's own fact", a.Bridge.Liveness)
		}
	}
	if !found {
		t.Fatalf("observed actor %s missing from mesh actors", observedKey)
	}
}
