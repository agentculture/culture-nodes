package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

func TestInsertOutboxDefaults(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-outbox-defaults")

	row, err := s.InsertOutbox(ctx, postgres.InsertOutboxInput{
		NamespaceID: ns.ID,
		Topic:       "dev.culture.nodes.run.created",
	})
	if err != nil {
		t.Fatalf("InsertOutbox: %v", err)
	}

	if row.ID == "" {
		t.Fatal("InsertOutbox did not assign an ID")
	}
	if row.Status != "pending" {
		t.Fatalf("Status = %q, want %q", row.Status, "pending")
	}
	if row.PublishedAt != nil {
		t.Fatalf("PublishedAt = %v, want nil for a freshly inserted row", row.PublishedAt)
	}
	if row.Attempts != 0 {
		t.Fatalf("Attempts = %d, want 0", row.Attempts)
	}
	if string(row.Payload) != "{}" {
		t.Fatalf("Payload = %s, want {} (the zero-value default)", row.Payload)
	}
	if row.AvailableAt.IsZero() {
		t.Fatal("AvailableAt was not defaulted to now()")
	}
	if row.CreatedAt.IsZero() {
		t.Fatal("CreatedAt was not set")
	}
}

func TestInsertOutboxCarriesPayloadAndAvailableAt(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-outbox-payload")
	payload := json.RawMessage(`{"run_id":"run_01J000000000000000000000"}`)
	availableAt := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Millisecond)

	row, err := s.InsertOutbox(ctx, postgres.InsertOutboxInput{
		NamespaceID: ns.ID,
		Topic:       "dev.culture.nodes.run.waiting",
		Payload:     payload,
		AvailableAt: availableAt,
	})
	if err != nil {
		t.Fatalf("InsertOutbox: %v", err)
	}

	// JSONB round-trips through Postgres's own canonical text form (e.g. it
	// adds a space after ":"), so compare decoded values rather than raw
	// bytes.
	var got, want map[string]any
	if err := json.Unmarshal(row.Payload, &got); err != nil {
		t.Fatalf("unmarshal returned payload %s: %v", row.Payload, err)
	}
	if err := json.Unmarshal(payload, &want); err != nil {
		t.Fatalf("unmarshal expected payload: %v", err)
	}
	if len(got) != len(want) || got["run_id"] != want["run_id"] {
		t.Fatalf("Payload = %s, want %s", row.Payload, payload)
	}
	if !row.AvailableAt.Equal(availableAt) {
		t.Fatalf("AvailableAt = %v, want %v", row.AvailableAt, availableAt)
	}
}
