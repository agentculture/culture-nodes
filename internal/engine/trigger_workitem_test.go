package engine_test

import (
	"encoding/json"
	"testing"

	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// deliverPayloadEvent delivers one test.subject-event fact carrying the given
// payload through the same Store.DeliverSignalEvent path the API's event
// handler uses, so what this proves is the engine's real minting path
// (createTriggeredRunTx), not a stand-in.
func deliverPayloadEvent(t *testing.T, f *fixture, subject, sourceKey string, payload json.RawMessage) storepg.SignalDelivery {
	t.Helper()
	delivery, err := f.store.DeliverSignalEvent(f.ctx, storepg.DeliverSignalEventInput{
		NamespaceID: f.ns.ID,
		Name:        "test.subject-event",
		Payload:     payload,
		Emitter:     "test",
		Subject:     subject,
		SourceKey:   sourceKey,
		Watermark:   json.RawMessage(`{"seq":"` + sourceKey + `"}`),
		Trigger:     f.engine,
	})
	if err != nil {
		t.Fatalf("DeliverSignalEvent(%s): %v", sourceKey, err)
	}
	if len(delivery.Triggered) != 1 || delivery.Triggered[0].Attached {
		t.Fatalf("delivery %s: want exactly one NEW triggered run, got %+v", sourceKey, delivery.Triggered)
	}
	return delivery
}

// TestTriggeredRunStampsWorkItemFromPayload (plan loop-closure-claude-codex
// t1): the engine's event->run minting stamps runs.work_item from the
// payload's string `work_item` when present, so a sweep-emitted fact lands
// keyed without a second write. Anything else — no field, or a non-string
// value — stamps nothing, and category is never touched (decision c41).
func TestTriggeredRunStampsWorkItemFromPayload(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)
	es, err := storepg.NewEngineStore(f.store, f.ns.ID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}

	cases := []struct {
		name, subject string
		payload       string
		want          string
	}{
		{"string work_item", "SCRUM-9", `{"work_item":"SCRUM-9","action":"opened"}`, "SCRUM-9"},
		{"no work_item", "SCRUM-10", `{"action":"opened"}`, ""},
		{"non-string work_item", "SCRUM-11", `{"work_item":42}`, ""},
		{"empty string work_item", "SCRUM-12", `{"work_item":""}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delivery := deliverPayloadEvent(t, f, tc.subject, tc.subject, json.RawMessage(tc.payload))
			run, err := es.Run(f.ctx, delivery.Triggered[0].RunID)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if run.WorkItem != tc.want {
				t.Errorf("WorkItem = %q, want %q", run.WorkItem, tc.want)
			}
			if run.Category != "" {
				t.Errorf("Category = %q, want untouched (read-through metadata is not populated, and never from work_item)", run.Category)
			}
			if run.Subject != tc.subject {
				t.Errorf("Subject = %q, want %q (work_item must not displace the correlation subject)", run.Subject, tc.subject)
			}
		})
	}
}
