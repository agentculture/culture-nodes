package worker_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// Task t7's acceptance over real rows: a downstream node binding
// /nodes/<id>/evidence receives exactly that node's evidence records —
// selected by node run through the node_runs join, not by SubjectRef (node
// evidence carries none). The run appends evidence from two different nodes
// plus a claim from the bound one, so the assertion is discriminating in
// both dimensions: node identity and record type.
func TestWorkerResolvesNodeEvidenceBindingFromRealRows(t *testing.T) {
	// The delta's origin actor must be a registered actor row (the ledger's
	// foreign key), created after the harness exists; the handler reads it
	// under the harness mutex so the HTTP goroutine sees the write.
	var agentActorID string

	h := newHarness(t, func(h *harness, w http.ResponseWriter, req actors.InvocationRequest) {
		h.mu.Lock()
		actorID := agentActorID
		h.mu.Unlock()

		switch req.Node.ID {
		case "gather":
			// One evidence record and one claim: the binding must carry the
			// former and never the latter.
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"outcome": "completed",
				"output": {"found": 1},
				"ledger_delta": {"records": [
					{
						"record_type": "evidence",
						"origin": {"kind": "agent", "actor_id": %q},
						"data": {"collection_method": "gather_self_report", "completeness": "partial"}
					},
					{
						"record_type": "claim",
						"origin": {"kind": "agent", "actor_id": %q},
						"data": {"statement": "gather says it measured something"}
					}
				]}
			}`, actorID, actorID)
		case "distract":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"outcome": "completed",
				"output": {"noise": true},
				"ledger_delta": {"records": [{
					"record_type": "evidence",
					"origin": {"kind": "agent", "actor_id": %q},
					"data": {"collection_method": "distract_self_report", "completeness": "partial"}
				}]}
			}`, actorID)
		case "verify":
			writeSyncResult(w, "completed", `{"ok":true}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	h.mu.Lock()
	agentActorID = mustAgentActor(t, h.store, h.ns.ID)
	h.mu.Unlock()

	run := h.createRun("evidence.workflow.yaml", `{}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if final := h.run(run.ID); final.State != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed (worker errors: %v)", final.State, h.workerErrors())
	}

	// The verify node's dispatched input is what the binding resolved to.
	var verifyInput json.RawMessage
	for _, inv := range h.invocations() {
		if inv.Node.ID == "verify" {
			verifyInput = inv.Input
		}
	}
	if verifyInput == nil {
		t.Fatal("the verify node was never invoked")
	}

	var input struct {
		TestEvidence []ledger.Record `json:"testEvidence"`
	}
	if err := json.Unmarshal(verifyInput, &input); err != nil {
		t.Fatalf("decode verify input %s: %v", verifyInput, err)
	}
	if len(input.TestEvidence) != 1 {
		t.Fatalf("testEvidence carries %d records (%s), want exactly gather's one evidence record",
			len(input.TestEvidence), verifyInput)
	}

	rec := input.TestEvidence[0]
	if rec.RecordType != ledger.RecordEvidence {
		t.Errorf("record type = %q, want evidence: gather's claim must not ride along", rec.RecordType)
	}
	var method struct {
		CollectionMethod string `json:"collection_method"`
	}
	if err := json.Unmarshal(rec.Data, &method); err != nil {
		t.Fatalf("decode evidence data %s: %v", rec.Data, err)
	}
	if method.CollectionMethod != "gather_self_report" {
		t.Errorf("collection_method = %q, want gather_self_report — distract's evidence leaked into gather's surface",
			method.CollectionMethod)
	}

	// Evidence identity is the node run: the record must carry gather's node
	// run id (stamped by the engine, never claimed by the actor) and no
	// SubjectRef — nothing sets one on node evidence.
	var gatherNodeRunID string
	if err := h.store.Pool().QueryRow(h.ctx,
		`SELECT id FROM node_runs WHERE run_id = $1 AND node_key = 'gather'`, run.ID,
	).Scan(&gatherNodeRunID); err != nil {
		t.Fatalf("read gather's node run: %v", err)
	}
	if rec.NodeRunID.String() != gatherNodeRunID {
		t.Errorf("node_run_id = %q, want gather's node run %q", rec.NodeRunID, gatherNodeRunID)
	}
	if rec.SubjectRef.String() != "" {
		t.Errorf("subject_ref = %q, want none: node evidence is selected by node run, not subject", rec.SubjectRef)
	}
}
