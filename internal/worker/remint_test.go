package worker_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

func TestTechnicalTriggerFailureRemintUsesConfiguredProducerActorID(t *testing.T) {
	now := time.Now().UTC()
	producerID := "engine_remint_" + store.NewULID()
	h := newClockedHarness(t, func() time.Time { return now }, func(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
		w.WriteHeader(http.StatusInternalServerError)
	}, func(opts *worker.Options) {
		opts.RemintProducerActorID = producerID
	})

	if _, err := h.store.Pool().Exec(h.ctx, `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		VALUES ($1, $2, $1, 1, 'engine', 'internal')`, producerID, h.ns.ID); err != nil {
		t.Fatalf("register configured re-mint producer: %v", err)
	}
	publishTriggerWorkflow(t, h, "trigger-agent.workflow.yaml")
	runID := h.triggerRun("test.jira-ready", "SCRUM-REMINT", `{"subject":"SCRUM-REMINT"}`).RunID
	h.runUntil(3*time.Second, func() bool { return h.run(runID).State == "failed" })

	now = now.Add(storepg.RemintBackoff + time.Second)
	if _, err := h.worker.Tick(h.ctx); err != nil {
		t.Fatalf("Tick admitting due re-mint: %v", err)
	}
	var actorID string
	if err := h.store.Pool().QueryRow(h.ctx, `
		SELECT origin_actor_id FROM ledger_records
		WHERE authority='derived' AND data->>'kind'='trigger_remint'
		ORDER BY created_at DESC LIMIT 1`).Scan(&actorID); err != nil {
		t.Fatalf("read derived re-mint record: %v", err)
	}
	if actorID != producerID {
		t.Fatalf("derived origin.actor_id = %q, want configured %q", actorID, producerID)
	}
}
