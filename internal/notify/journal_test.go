package notify_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/agentculture/culture-nodes/internal/notify"
)

// TestJournalEntryHasExactlyThreeFields pins JournalEntry's shape: Event,
// RunID, Outcome and nothing else — never a URL field, never a payload
// field, structurally matching the spec's "journal receives only {event,
// run id, outcome}".
func TestJournalEntryHasExactlyThreeFields(t *testing.T) {
	typ := reflect.TypeOf(notify.JournalEntry{})
	wantFields := []string{"Event", "RunID", "Outcome"}
	if typ.NumField() != len(wantFields) {
		t.Fatalf("JournalEntry has %d fields, want exactly %d (%v)", typ.NumField(), len(wantFields), wantFields)
	}
	for i, name := range wantFields {
		if got := typ.Field(i).Name; got != name {
			t.Errorf("field %d = %q, want %q", i, got, name)
		}
	}
}

func TestNotifyDisabledMakesNoNetworkCallAndDoesNotJournal(t *testing.T) {
	hit := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hit = true
	}))
	defer server.Close()
	// Deliberately not registered as the webhook URL — TestMain already
	// leaves both env vars unset, so ResolveWebhook reports disabled and
	// Notify must never reach out to this (or any) server.

	journaled := 0
	result := notify.Notify(context.Background(), notify.Payload{RunID: "run_1", Event: "run.completed"}, func(notify.JournalEntry) {
		journaled++
	})

	if result != notify.Disabled {
		t.Fatalf("want Disabled, got %v", result)
	}
	if hit {
		t.Fatalf("disabled Notify must never make a network call")
	}
	if journaled != 0 {
		t.Fatalf("disabled Notify must not journal, got %d journal calls", journaled)
	}
}

func TestNotifyPostsAndJournalsOutcomeWithoutURLOrPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv(envPrimary, server.URL)

	var entries []notify.JournalEntry
	payload := notify.Payload{RunID: "run_42", Workflow: "deliver-change", Event: "run.completed", Actor: "codex-thor", DashboardLink: "https://dashboard.example/runs/run_42"}

	result := notify.Notify(context.Background(), payload, func(e notify.JournalEntry) {
		entries = append(entries, e)
	})

	if result != notify.Posted {
		t.Fatalf("want Posted, got %v", result)
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly one journal entry, got %d: %+v", len(entries), entries)
	}
	entry := entries[0]
	if entry.RunID != payload.RunID || entry.Event != payload.Event || entry.Outcome != notify.Posted {
		t.Errorf("journal entry = %+v, want RunID=%q Event=%q Outcome=Posted", entry, payload.RunID, payload.Event)
	}
}

func TestNotifyFailedJournalsFailedOutcome(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	t.Setenv(envPrimary, server.URL)

	var entries []notify.JournalEntry
	payload := notify.Payload{RunID: "run_43", Event: "run.completed"}

	result := notify.Notify(context.Background(), payload, func(e notify.JournalEntry) {
		entries = append(entries, e)
	})

	if result != notify.Failed {
		t.Fatalf("want Failed, got %v", result)
	}
	if len(entries) != 1 || entries[0].Outcome != notify.Failed {
		t.Fatalf("want exactly one Failed journal entry, got %+v", entries)
	}
}

func TestNotifyNilJournalFuncIsSafe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv(envPrimary, server.URL)

	result := notify.Notify(context.Background(), notify.Payload{RunID: "run_44", Event: "run.completed"}, nil)
	if result != notify.Posted {
		t.Fatalf("want Posted, got %v", result)
	}
}
