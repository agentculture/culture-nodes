package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The capacity circuit breaker's read and clear surfaces (task t9, honesty
// condition h38: the pause is "visible on the actors surface with reason and
// until-when, and clearable without touching the database").
//
// The breaker itself is proved in internal/worker/breaker_test.go against a
// real dispatch. What these tests own is the operator's half: can they SEE
// the pause, and can they LIFT it through the API.

// actorKeyOfRow reads the actor_key the fixture's insertActor minted (it
// suffixes the id, so the key is not knowable from the caller's argument).
func actorKeyOfRow(t *testing.T, f *fixture, actorID string) string {
	t.Helper()
	var key string
	if err := f.store.Pool().QueryRow(context.Background(),
		`SELECT actor_key FROM actors WHERE id = $1`, actorID).Scan(&key); err != nil {
		t.Fatalf("read actor_key: %v", err)
	}
	return key
}

func pauseActorKey(t *testing.T, f *fixture, actorKey string, until time.Duration, retryAfter time.Duration) {
	t.Helper()
	if _, err := f.store.PauseActor(context.Background(), postgres.PauseActorInput{
		NamespaceID: f.nsID,
		ActorKey:    actorKey,
		PausedUntil: time.Now().UTC().Add(until),
		Reason:      "capacity_exhausted",
		RetryAfter:  retryAfter,
		Detail:      "weekly session limit reached",
		RunID:       "run-tripped",
		NodeRunID:   "nr-tripped",
		AttemptID:   "att-tripped",
	}); err != nil {
		t.Fatalf("PauseActor: %v", err)
	}
}

// actorAvailabilityWire mirrors components.schemas.ActorAvailability. It is
// declared here rather than imported so the test reads the JSON contract,
// not the Go struct that produces it.
type actorAvailabilityWire struct {
	Paused             bool   `json:"paused"`
	PausedUntil        string `json:"paused_until"`
	Reason             string `json:"reason"`
	RetryAfterSeconds  *int32 `json:"retry_after_seconds"`
	Detail             string `json:"detail"`
	TrippedAt          string `json:"tripped_at"`
	TrippedByRunID     string `json:"tripped_by_run_id"`
	TrippedByAttemptID string `json:"tripped_by_attempt_id"`
	ClearedAt          string `json:"cleared_at"`
	ClearedBy          string `json:"cleared_by"`
}

type actorWire struct {
	ID           string                 `json:"id"`
	ActorKey     string                 `json:"actor_key"`
	Availability *actorAvailabilityWire `json:"availability"`
}

type actorListWire struct {
	Items []actorWire `json:"items"`
}

func TestActorSurfaceRendersTheCapacityPause(t *testing.T) {
	f := newFixture(t)
	actorID := f.insertActor("paused-analyzer")
	actorKey := actorKeyOfRow(t, f, actorID)
	pauseActorKey(t, f, actorKey, 30*time.Minute, 120*time.Second)

	var got actorWire
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors/"+actorID), nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET actor: %d: %s", resp.StatusCode, body)
	}
	if got.Availability == nil {
		t.Fatalf("actor payload carries no availability block: %s", body)
	}
	if !got.Availability.Paused {
		t.Errorf("availability.paused = false, want true: %s", body)
	}
	if got.Availability.Reason != "capacity_exhausted" {
		t.Errorf("availability.reason = %q, want capacity_exhausted", got.Availability.Reason)
	}
	if got.Availability.PausedUntil == "" {
		t.Error("availability carries no paused_until; h38 asks for reason AND until-when")
	}
	if got.Availability.RetryAfterSeconds == nil || *got.Availability.RetryAfterSeconds != 120 {
		t.Errorf("availability.retry_after_seconds = %v, want 120", got.Availability.RetryAfterSeconds)
	}
	if got.Availability.TrippedByAttemptID != "att-tripped" {
		t.Errorf("availability.tripped_by_attempt_id = %q, want the tripping attempt",
			got.Availability.TrippedByAttemptID)
	}

	// The list surface renders the same block, resolved by actor_key.
	var list actorListWire
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors"), nil, &list)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET actors: %d: %s", resp.StatusCode, body)
	}
	var found bool
	for _, item := range list.Items {
		if item.ID != actorID {
			continue
		}
		found = true
		if item.Availability == nil || !item.Availability.Paused {
			t.Errorf("listed actor availability = %+v, want a live pause", item.Availability)
		}
	}
	if !found {
		t.Fatalf("the paused actor is missing from the list: %s", body)
	}
}

// An actor that was never paused carries no availability block at all —
// structurally distinct from one whose pause lapsed, which renders with
// paused: false. Neither may impersonate the other.
func TestActorNeverPausedCarriesNoAvailabilityBlock(t *testing.T) {
	f := newFixture(t)
	clean := f.insertActor("never-paused")
	lapsed := f.insertActor("lapsed")
	pauseActorKey(t, f, actorKeyOfRow(t, f, lapsed), -time.Minute, 0)

	var got actorWire
	if resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors/"+clean), nil, &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET actor: %d: %s", resp.StatusCode, body)
	}
	if got.Availability != nil {
		t.Errorf("availability = %+v, want absent for an actor that has never been paused", got.Availability)
	}

	var lapsedOut actorWire
	if resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors/"+lapsed), nil, &lapsedOut); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET lapsed actor: %d: %s", resp.StatusCode, body)
	}
	if lapsedOut.Availability == nil {
		t.Fatal("a lapsed pause disappeared entirely; it must still render as history")
	}
	if lapsedOut.Availability.Paused {
		t.Errorf("lapsed availability.paused = true, want false")
	}
	if lapsedOut.Availability.RetryAfterSeconds != nil {
		t.Errorf("retry_after_seconds = %v, want absent when the provider named none",
			*lapsedOut.Availability.RetryAfterSeconds)
	}
}

func TestResumeActorClearsThePause(t *testing.T) {
	f := newFixtureWithActorRegistrationAuth(t, actorRegistrationSecret)
	actorID := f.insertActor("resumable")
	actorKey := actorKeyOfRow(t, f, actorID)
	pauseActorKey(t, f, actorKey, time.Hour, 0)

	var got actorWire
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/actors/"+actorID+"/resume"), actorRegistrationSecret,
		map[string]string{"cleared_by": "ori"}, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST resume: %d: %s", resp.StatusCode, body)
	}
	if got.Availability == nil {
		t.Fatalf("resume response carries no availability block: %s", body)
	}
	if got.Availability.Paused {
		t.Errorf("availability.paused = true after a resume, want false: %s", body)
	}
	if got.Availability.ClearedBy != "ori" || got.Availability.ClearedAt == "" {
		t.Errorf("cleared_by/cleared_at = %q/%q, want the operator recorded — an expiry and a human clear must stay distinguishable",
			got.Availability.ClearedBy, got.Availability.ClearedAt)
	}
	// The dispatch site agrees: nothing is paused any more.
	if _, active, err := f.store.ActivePause(context.Background(), f.nsID, actorKey); err != nil || active {
		t.Errorf("ActivePause after resume = (%v, %v), want (false, nil)", active, err)
	}

	// Repeating it is a no-op, not an error: the caller's intent is already
	// satisfied, and two operators racing to clear one pause both succeed.
	resp, body = doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/actors/"+actorID+"/resume"), actorRegistrationSecret, nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second POST resume: %d: %s", resp.StatusCode, body)
	}
}

// Resuming an actor that was never paused is 200 with its current state, not
// a 404 on a pause that does not exist: the operator asked for the actor to
// be dispatchable, and it is.
func TestResumeActorThatWasNeverPausedSucceeds(t *testing.T) {
	f := newFixtureWithActorRegistrationAuth(t, actorRegistrationSecret)
	actorID := f.insertActor("never-paused-resume")

	var got actorWire
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/actors/"+actorID+"/resume"), actorRegistrationSecret, nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST resume: %d: %s", resp.StatusCode, body)
	}
	if got.Availability != nil {
		t.Errorf("availability = %+v, want absent: clearing a pause that never existed invents nothing", got.Availability)
	}
}

// The clear is a privileged operation on the same secret registration uses:
// it restores exactly the standing registration granted.
func TestResumeActorRequiresTheRegistrationToken(t *testing.T) {
	f := newFixtureWithActorRegistrationAuth(t, actorRegistrationSecret)
	actorID := f.insertActor("guarded")
	actorKey := actorKeyOfRow(t, f, actorID)
	pauseActorKey(t, f, actorKey, time.Hour, 0)

	for _, tc := range []struct{ name, token string }{
		{"no token", ""},
		{"wrong token", "not-the-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doJSONBearer(t, f.client, http.MethodPost,
				f.url("/v1alpha1/actors/"+actorID+"/resume"), tc.token, nil, nil)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("POST resume: %d: %s, want 401", resp.StatusCode, body)
			}
		})
	}
	// ...and the pause is untouched by the refused attempts.
	if _, active, err := f.store.ActivePause(context.Background(), f.nsID, actorKey); err != nil || !active {
		t.Errorf("ActivePause after refused resumes = (%v, %v), want it still paused", active, err)
	}
}

// A resume against an unknown actor id is a 404 on the ACTOR, not a silent
// success that would leave an operator believing they had cleared something.
func TestResumeUnknownActorIs404(t *testing.T) {
	f := newFixtureWithActorRegistrationAuth(t, actorRegistrationSecret)
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/actors/no-such-actor/resume"), actorRegistrationSecret, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST resume: %d: %s, want 404", resp.StatusCode, body)
	}
}
