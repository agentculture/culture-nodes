package events_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/events"
)

func TestNewStampsULIDAndDefaults(t *testing.T) {
	env, err := events.New(events.NewInput{
		Type: events.TypeRunCreated,
		Data: map[string]string{"run_id": "run_01J000000000000000000000"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if len(env.ID) != 26 {
		t.Fatalf("ID = %q, want a 26-character ULID", env.ID)
	}
	if env.Source != "nodes" {
		t.Fatalf("Source = %q, want the default %q", env.Source, "nodes")
	}
	if env.SpecVersion != "1.0" {
		t.Fatalf("SpecVersion = %q, want %q", env.SpecVersion, "1.0")
	}
	if env.DataContentType != "application/json" {
		t.Fatalf("DataContentType = %q, want %q", env.DataContentType, "application/json")
	}
	if env.Time.IsZero() {
		t.Fatal("Time was not defaulted")
	}
	if !strings.HasPrefix(env.Type, "dev.culture.nodes.") {
		t.Fatalf("Type = %q, want the dev.culture.nodes. prefix", env.Type)
	}

	var data map[string]string
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal Data: %v", err)
	}
	if data["run_id"] != "run_01J000000000000000000000" {
		t.Fatalf("Data = %s, want it to round-trip the input", env.Data)
	}
}

func TestNewTwoCallsMintDistinctIDs(t *testing.T) {
	a, err := events.New(events.NewInput{Type: events.TypeRunCreated})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := events.New(events.NewInput{Type: events.TypeRunCreated})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("two New calls minted the same ID %q, want distinct IDs", a.ID)
	}
}

func TestNewRejectsTypeWithoutPrefix(t *testing.T) {
	_, err := events.New(events.NewInput{Type: "run.created"})
	if err == nil {
		t.Fatal("New accepted a Type without the dev.culture.nodes. prefix, want an error")
	}
}

func TestNewNilDataDefaultsToEmptyObject(t *testing.T) {
	env, err := events.New(events.NewInput{Type: events.TypeRunWaiting})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if string(env.Data) != "{}" {
		t.Fatalf("Data = %s, want {} for nil input data", env.Data)
	}
}

func TestValidateRejectsMissingRequiredAttributes(t *testing.T) {
	cases := []struct {
		name string
		env  events.Envelope
	}{
		{"missing id", events.Envelope{Source: "nodes", SpecVersion: "1.0", Type: events.TypeRunCreated, DataContentType: "application/json"}},
		{"missing source", events.Envelope{ID: "x", SpecVersion: "1.0", Type: events.TypeRunCreated, DataContentType: "application/json"}},
		{"missing type", events.Envelope{ID: "x", Source: "nodes", SpecVersion: "1.0", DataContentType: "application/json"}},
		{"wrong type prefix", events.Envelope{ID: "x", Source: "nodes", SpecVersion: "1.0", Type: "run.created", DataContentType: "application/json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.env.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want an error for %s", tc.name)
			}
		})
	}
}

func TestEnvelopeTypeConstantsCarryThePrefix(t *testing.T) {
	types := []string{
		events.TypeRunCreated,
		events.TypeTokenEntered,
		events.TypeNodeRunReady,
		events.TypeAttemptStarted,
		events.TypeActorAccepted,
		events.TypeAttemptCompleted,
		events.TypeLedgerRecordAppended,
		events.TypeLedgerReviewCommitted,
		events.TypeRunnerOperationCompleted,
		events.TypeContractRejected,
		events.TypeTokenTransitioned,
		events.TypeRunWaiting,
		events.TypeRunCompleted,
	}
	for _, typ := range types {
		if !strings.HasPrefix(typ, "dev.culture.nodes.") {
			t.Errorf("type constant %q does not carry the dev.culture.nodes. prefix", typ)
		}
	}
}
