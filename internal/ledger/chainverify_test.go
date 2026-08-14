package ledger_test

import (
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// VerifyClaimChain (task t24, issue #45, honesty h19): every live claim
// carries a source, every live decision/task traces to a motivating claim.
// These tests build records the way an agent's own LedgerDelta would (the
// same shape internal/engine's prepareRecord accepts and internal/devague's
// MapPlanShow already builds via ProvenanceRefs) — never through a
// domain-specific helper — because the function under test is domain-
// agnostic and the tests should prove that by construction, not by comment.

func chainVerifyClaimRecord(id string, sources []map[string]string) ledger.Record {
	data := map[string]any{"statement": "a claim"}
	if sources != nil {
		data["sources"] = sources
	}
	raw, _ := json.Marshal(data)
	return ledger.Record{
		ID:         id,
		RecordType: ledger.RecordClaim,
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: "actor_test"},
		Authority:  ledger.AuthorityProposed,
		Data:       raw,
	}
}

func chainVerifyDecisionRecord(id string, claimRefs []string) ledger.Record {
	raw, _ := json.Marshal(map[string]any{"selected": "an option"})
	return ledger.Record{
		ID:             id,
		RecordType:     ledger.RecordDecision,
		Origin:         ledger.Origin{Kind: ledger.OriginAgent, ActorID: "actor_test"},
		Authority:      ledger.AuthorityProposed,
		Data:           raw,
		ProvenanceRefs: claimRefs,
	}
}

func TestVerifyClaimChainPassesWhenEveryClaimIsSourcedAndEveryDecisionIsMotivated(t *testing.T) {
	records := []ledger.Record{
		chainVerifyClaimRecord("claim-1", []map[string]string{{"url": "https://example.com/a"}}),
		chainVerifyClaimRecord("claim-2", []map[string]string{{"url": "https://example.com/b"}}),
		chainVerifyDecisionRecord("decision-1", []string{"claim-1"}),
		chainVerifyDecisionRecord("decision-2", []string{"claim-1", "claim-2"}),
	}

	got := ledger.VerifyClaimChain(records)

	if !got.Passed {
		t.Fatalf("VerifyClaimChain = %+v, want passed", got)
	}
	if len(got.Claims) != 2 {
		t.Fatalf("claims = %+v, want 2", got.Claims)
	}
	for _, c := range got.Claims {
		if !c.Sourced || c.SourceCount != 1 {
			t.Errorf("claim %s = %+v, want sourced with 1 source", c.ClaimID, c)
		}
	}
	if len(got.Motivated) != 2 {
		t.Fatalf("motivated = %+v, want 2", got.Motivated)
	}
	for _, m := range got.Motivated {
		if !m.Motivated {
			t.Errorf("record %s = %+v, want motivated", m.RecordID, m)
		}
	}
}

func TestVerifyClaimChainFailsOnAnUnsourcedClaim(t *testing.T) {
	records := []ledger.Record{
		chainVerifyClaimRecord("claim-1", []map[string]string{{"url": "https://example.com/a"}}),
		chainVerifyClaimRecord("claim-2", nil), // no sources at all
		chainVerifyDecisionRecord("decision-1", []string{"claim-1", "claim-2"}),
	}

	got := ledger.VerifyClaimChain(records)

	if got.Passed {
		t.Fatalf("VerifyClaimChain = %+v, want not passed: claim-2 has no sources", got)
	}
	var sawUnsourced bool
	for _, c := range got.Claims {
		if c.ClaimID == "claim-2" {
			sawUnsourced = true
			if c.Sourced || c.SourceCount != 0 {
				t.Errorf("claim-2 = %+v, want unsourced", c)
			}
		}
	}
	if !sawUnsourced {
		t.Fatalf("claims = %+v, want claim-2 present", got.Claims)
	}
}

func TestVerifyClaimChainFailsOnADecisionWithNoMotivatingClaim(t *testing.T) {
	records := []ledger.Record{
		chainVerifyClaimRecord("claim-1", []map[string]string{{"url": "https://example.com/a"}}),
		chainVerifyDecisionRecord("decision-1", nil),           // no provenance at all
		chainVerifyDecisionRecord("decision-2", []string{"x"}), // refers to nothing live
	}

	got := ledger.VerifyClaimChain(records)

	if got.Passed {
		t.Fatalf("VerifyClaimChain = %+v, want not passed: neither decision names a live claim", got)
	}
	if len(got.Motivated) != 2 {
		t.Fatalf("motivated = %+v, want 2", got.Motivated)
	}
	for _, m := range got.Motivated {
		if m.Motivated {
			t.Errorf("record %s = %+v, want not motivated", m.RecordID, m)
		}
		if len(m.ClaimRefs) != 0 {
			t.Errorf("record %s claim_refs = %v, want none", m.RecordID, m.ClaimRefs)
		}
	}
}

// A task record is checked exactly like a decision: t22's own MapPlanShow
// connects a task to the claims it covers through ProvenanceRefs
// (internal/devague/plan.go's coveredClaimRefs), so a generic verifier must
// treat task and decision identically or it would silently miss t22's own
// instance of the pattern it is meant to generalize.
func TestVerifyClaimChainTreatsTaskRecordsLikeDecisions(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"goal": "do the thing"})
	task := ledger.Record{
		ID:             "task-1",
		RecordType:     ledger.RecordTask,
		Origin:         ledger.Origin{Kind: ledger.OriginAgent, ActorID: "actor_test"},
		Authority:      ledger.AuthorityProposed,
		Data:           raw,
		ProvenanceRefs: []string{"claim-1"},
	}
	records := []ledger.Record{
		chainVerifyClaimRecord("claim-1", []map[string]string{{"url": "https://example.com/a"}}),
		task,
	}

	got := ledger.VerifyClaimChain(records)

	if !got.Passed {
		t.Fatalf("VerifyClaimChain = %+v, want passed", got)
	}
	if len(got.Motivated) != 1 || got.Motivated[0].RecordType != ledger.RecordTask {
		t.Fatalf("motivated = %+v, want the task record present", got.Motivated)
	}
}

func TestVerifyClaimChainIgnoresSupersededRecords(t *testing.T) {
	superseded := chainVerifyClaimRecord("claim-1", nil)
	replacement := chainVerifyClaimRecord("claim-2", []map[string]string{{"url": "https://example.com/a"}})
	replacement.Supersedes = ledger.NullableID("claim-1")

	records := []ledger.Record{
		superseded,
		replacement,
		chainVerifyDecisionRecord("decision-1", []string{"claim-2"}),
	}

	got := ledger.VerifyClaimChain(records)

	if !got.Passed {
		t.Fatalf("VerifyClaimChain = %+v, want passed: the unsourced claim was superseded", got)
	}
	if len(got.Claims) != 1 || got.Claims[0].ClaimID != "claim-2" {
		t.Fatalf("claims = %+v, want only the live replacement", got.Claims)
	}
}

func TestVerifyClaimChainPassesVacuouslyOnAnEmptyRun(t *testing.T) {
	got := ledger.VerifyClaimChain(nil)
	if !got.Passed {
		t.Fatalf("VerifyClaimChain(nil) = %+v, want passed (nothing to fail)", got)
	}
	if len(got.Claims) != 0 || len(got.Motivated) != 0 {
		t.Fatalf("VerifyClaimChain(nil) = %+v, want no claims or motivated records", got)
	}
}
