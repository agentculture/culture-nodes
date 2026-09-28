package api_test

// Task t38g (#328, review finding A2): the control plane alone emits the
// reactions (human.decision, code.result, agent.result), node.expired and
// the action.* results, under the declaration engine's emitter. No external
// ingress may deliver one of those names, or claim that emitter: a
// reaction's marker proves which firing it continues, not what it says.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

var reservedEventNames = []string{"timer", "human.decision", "human.requested", "code.result", "agent.result", "node.expired",
	"action.failed", "action.timed_out", "action.rejected", "action.capacity_exhausted", "action.budget_exhausted"}

func signalEventCount(t *testing.T, f *fixture) int {
	t.Helper()
	var n int
	if err := f.store.Pool().QueryRow(context.Background(), `SELECT count(*) FROM signal_events WHERE namespace_id=$1`, f.nsID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeliverEventRefusesTheControlPlanesReservedNamesAndEmitter(t *testing.T) {
	f := newFixtureWithEventAuth(t, eventTokenSecret)
	for _, name := range reservedEventNames {
		resp, body := postEvent(t, f, eventTokenSecret, deliverEventReq{Name: name, Emitter: "human-inbox",
			Payload: []byte(`{"outcome":"approved","origin":{"marker":"cn1:x","artifact_kind":"human.decision","artifact_id":"task-1"}}`)}, nil)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), name) {
			t.Errorf("POST %s: %d %s, want 403 naming the reserved kind", name, resp.StatusCode, body)
		}
	}
	resp, body := postEvent(t, f, eventTokenSecret, deliverEventReq{Name: "ordinary", Emitter: "engine_declaration_engine"}, nil)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "engine_declaration_engine") {
		t.Errorf("POST timer as the engine's emitter: %d %s, want 403 naming the emitter", resp.StatusCode, body)
	}
	if n := signalEventCount(t, f); n != 0 {
		t.Fatalf("refused deliveries appended %d signal events, want 0", n)
	}
	if resp, body := postEvent(t, f, eventTokenSecret, deliverEventReq{Name: "ordinary", Emitter: "schedule:forged"}, nil); resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "schedule:forged") {
		t.Errorf("POST forged schedule emitter: %d %s, want 403", resp.StatusCode, body)
	}
	// An ordinary event is still delivered.
	if resp, body := postEvent(t, f, eventTokenSecret, deliverEventReq{Name: "ordinary", Emitter: "human-inbox"}, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST ordinary: %d %s, want 201", resp.StatusCode, body)
	}
}

// Every other ingress reaches signal_events through the store's delivery
// (webhooks, ticket replies, actor callbacks, schedules), which refuses the
// same; a schedule declaring one is refused at creation.
func TestStoreDeliveryAndSchedulesRefuseReservedEvents(t *testing.T) {
	f := newFixture(t)
	for _, name := range reservedEventNames {
		_, err := f.store.DeliverSignalEvent(context.Background(), storepg.DeliverSignalEventInput{NamespaceID: f.nsID, Name: name, Emitter: "jira-webhook"})
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("DeliverSignalEvent %s: err = %v, want the reserved-name refusal", name, err)
		}
		if name == "timer" {
			continue
		}
		resp, _ := createSchedule(t, f, map[string]any{"name": "s-" + strings.ReplaceAll(name, ".", "-"), "event_name": name, "interval_seconds": 60})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST /schedules event_name=%s: %d, want 400", name, resp.StatusCode)
		}
	}
	if resp, body := createSchedule(t, f, map[string]any{"name": "timer-check", "event_name": "timer", "interval_seconds": 60}); resp.StatusCode != http.StatusCreated {
		t.Errorf("POST timer schedule: %d %+v, want 201", resp.StatusCode, body)
	}
	if _, err := f.store.DeliverSignalEvent(context.Background(), storepg.DeliverSignalEventInput{NamespaceID: f.nsID, Name: "timer", Emitter: "engine_declaration_engine"}); err == nil {
		t.Error("DeliverSignalEvent accepted the engine's own emitter from outside the engine")
	}
	if n := signalEventCount(t, f); n != 0 {
		t.Fatalf("refused deliveries appended %d signal events, want 0", n)
	}
}
