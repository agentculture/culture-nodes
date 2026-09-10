package repair_test

import (
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/repair"
)

// The lane-liveness routing record (plan loop-closure t10, spec c26/c33,
// decision c43). It shares this package's routing-record shape — a derived
// `decision` with question/selected/options/reason/rationale/router — so a
// reader of a run's ledger reads a liveness reroute the same way they read
// a gate-failure routing, and so PriorAttempts never mistakes one for a
// repair round (it selects on RouterCollectionMethod, which this is not).

func livenessInput() repair.LivenessInput {
	return repair.LivenessInput{
		RunID: "run-1", NodeRunID: "node-run-1", AttemptID: "attempt-1", NodeID: "analyze",
		Lane: repair.LivenessLane{
			ActorKey: "company/primary", ActorRef: "actor://company/primary@sha256:aa",
			SessionOK: false, Reason: "refresh_token_spent", Mode: "LOCK", Locked: true,
			CheckedAt: time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC),
		},
		FallbackActorKey: "company/fallback", FallbackActorRef: "company/fallback",
		FreshnessWindow: 5 * time.Minute,
		RouterActorID:   "engine-dispatch-gate", RouterRevision: "rev",
		Now: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC),
	}
}

func TestLivenessFallbackRecordNamesBothActorsAndTheRouter(t *testing.T) {
	in := livenessInput()
	rec, err := repair.LivenessRouting{Selected: repair.LivenessSelectedFallback}.Record(in)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if rec.RecordType != ledger.RecordDecision || rec.Authority != ledger.AuthorityDerived || rec.Origin.Kind != ledger.OriginValidator {
		t.Fatalf("record envelope = type %s authority %s origin %s, want derived validator decision", rec.RecordType, rec.Authority, rec.Origin.Kind)
	}
	if rec.RunID != "run-1" || string(rec.NodeRunID) != "node-run-1" {
		t.Fatalf("record is attached to run %q node run %q", rec.RunID, rec.NodeRunID)
	}
	if string(rec.AttemptID) != "" {
		t.Fatalf("record envelope names attempt %q; the attempt row does not exist at dispatch time and the column is a foreign key", rec.AttemptID)
	}
	data, err := rec.DataMap()
	if err != nil {
		t.Fatal(err)
	}
	if data["attempt_id"] != "attempt-1" {
		t.Errorf("payload attempt_id = %v, want the attempt named in the payload instead", data["attempt_id"])
	}
	if data["router"] != repair.LivenessRouterMethod || data["router"] == repair.RouterCollectionMethod {
		t.Errorf("router = %v, want %q and never the gate-failure router", data["router"], repair.LivenessRouterMethod)
	}
	if data["selected"] != "fallback" || data["reason"] != string(repair.ReasonLaneNotLive) {
		t.Errorf("selected/reason = %v/%v", data["selected"], data["reason"])
	}
	opts, _ := data["options"].([]any)
	if len(opts) != 2 || opts[0] != "fallback" || opts[1] != "proceed" {
		t.Errorf("options = %v, want [fallback proceed]", data["options"])
	}
	if data["lane_actor_key"] != "company/primary" || data["fallback_actor_key"] != "company/fallback" {
		t.Errorf("record names lane %v and fallback %v, want both actors", data["lane_actor_key"], data["fallback_actor_key"])
	}
	if data["dispatched"] != true {
		t.Errorf("dispatched = %v, want true: unlike a gate-failure routing this control plane DID dispatch the routed choice", data["dispatched"])
	}
	liveness, _ := data["liveness"].(map[string]any)
	if liveness["session_ok"] != false || liveness["reason"] != "refresh_token_spent" || liveness["locked"] != true || liveness["mode"] != "LOCK" {
		t.Errorf("liveness fact in record = %v", liveness)
	}
	// A liveness reroute is not a repair round.
	if got := repair.PriorAttempts([]ledger.Record{rec}); got != 0 {
		t.Errorf("PriorAttempts counted a liveness routing as %d repair round(s)", got)
	}
}

func TestLivenessProceedRecordIsAWarningWithoutAFallback(t *testing.T) {
	in := livenessInput()
	in.FallbackActorKey, in.FallbackActorRef = "", ""
	rec, err := repair.LivenessRouting{Selected: repair.LivenessSelectedProceed}.Record(in)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	data, _ := rec.DataMap()
	if data["selected"] != "proceed" || data["severity"] != "warning" {
		t.Errorf("selected/severity = %v/%v, want proceed/warning", data["selected"], data["severity"])
	}
	if _, present := data["fallback_actor_key"]; present {
		t.Error("fallback_actor_key present on a record for an actor that registers none; absent means absent")
	}
	if rationale, _ := data["rationale"].(string); rationale == "" {
		t.Error("rationale is empty; the record must say why the dispatch proceeded into a lane that is not live")
	}
}

func TestLivenessRecordRefusesAnAnonymousRouterOrRun(t *testing.T) {
	in := livenessInput()
	in.RouterActorID = ""
	if _, err := (repair.LivenessRouting{Selected: repair.LivenessSelectedFallback}).Record(in); err == nil {
		t.Error("an anonymous router composed a derived record")
	}
	in = livenessInput()
	in.RunID = ""
	if _, err := (repair.LivenessRouting{Selected: repair.LivenessSelectedFallback}).Record(in); err == nil {
		t.Error("a routing with no run composed a record")
	}
}
