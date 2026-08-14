package devague_test

import (
	"testing"

	"github.com/agentculture/culture-nodes/internal/devague"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TestVerifyClaimChainComposesWithMapPlanShowUnchanged is task t24's
// genericity proof for the code-plan instance: internal/ledger.VerifyClaimChain
// (the generic decompose-pipeline verification surface, issue #45) is
// exercised against MapPlanShow's REAL, unmodified output — the same
// testdata/plan-show.json fixture plan_show_test.go itself walks — with zero
// devague-specific code in VerifyClaimChain and zero changes to
// MapPlanShow. Genericity is proven by reuse, not asserted by a comment.
//
// MapPlanShow already connects a task record to the claims it `covers`
// through ProvenanceRefs (plan_show.go's coveredClaimRefs, unchanged by
// t24) -- that connection is exactly what VerifyClaimChain's "motivated"
// half checks. The frame side is a small hand-built frameShow carrying only
// the two claims t1 and t3 (the fixture's own tasks) cover -- c1
// (requirement) and c3 (after_state) -- under the SAME "t22fixture" slug
// plan-show.json uses, so MapFrameClaims and MapPlanShow's deterministic ids
// line up without this test threading anything between them (see
// record.go's doc comment on why a plan's slug always equals its frame's).
//
// devague's own claims never carry a `sources` property (devague has no
// such concept), so the "sourced" half of the verdict is honestly expected
// to fail here -- proving VerifyClaimChain is generic, not that every
// devague import happens to pass it. Passing is the newsletter instance's
// job (internal/ledger/chainverify_test.go and tests/e2e's newsletter
// workflow); this test's job is proving the SAME function correctly reads
// t22's own connection pattern with no new code path for it.
func TestVerifyClaimChainComposesWithMapPlanShowUnchanged(t *testing.T) {
	frameJSON := []byte(`{
		"slug": "t22fixture",
		"title": "t22 fixture plan",
		"claims": [
			{"id": "c1", "kind": "requirement", "text": "Fixture t22 plan-show ships real dependency edges", "origin": "llm", "status": "proposed"},
			{"id": "c3", "kind": "after_state", "text": "Fixture reaches a stable demo state", "origin": "llm", "status": "proposed"}
		]
	}`)

	frameRecords, err := devague.MapFrameClaims(frameJSON)
	if err != nil {
		t.Fatalf("MapFrameClaims: %v", err)
	}
	planRecords, err := devague.MapPlanShow(readTestdata(t, "plan-show.json"))
	if err != nil {
		t.Fatalf("MapPlanShow: %v", err)
	}

	records := append(append([]ledger.Record(nil), frameRecords...), planRecords...)

	got := ledger.VerifyClaimChain(records)

	// The "sourced" half: devague claims carry no `sources` property, so
	// this is honestly unsourced -- not a bug in VerifyClaimChain, the
	// accurate report that a code-plan import (unlike the newsletter
	// instance) never populates it.
	if got.Passed {
		t.Fatalf("VerifyClaimChain = %+v, want not passed: devague claims carry no sources", got)
	}
	if len(got.Claims) != 2 {
		t.Fatalf("claims = %+v, want 2 (c1, c3)", got.Claims)
	}
	for _, c := range got.Claims {
		if c.Sourced {
			t.Errorf("claim %s = %+v, want unsourced: devague's frameClaim has no sources field", c.ClaimID, c)
		}
	}

	// The "motivated" half: t1 covers c1, t3 covers c3 (testdata/plan-show.json;
	// see plan_show_test.go's own TestParsePlanShow_Fixture). MapPlanShow's
	// coveredClaimRefs must have carried both through as ProvenanceRefs,
	// unaided by anything this test adds.
	motivated := map[string]ledger.MotivatedCheck{}
	for _, m := range got.Motivated {
		motivated[m.RecordID] = m
	}
	t1 := motivated["dv_t22fixture_t1"]
	if !t1.Motivated || len(t1.ClaimRefs) != 1 || t1.ClaimRefs[0] != "dv_t22fixture_c1" {
		t.Errorf("t1 motivated check = %+v, want motivated by dv_t22fixture_c1", t1)
	}
	t3 := motivated["dv_t22fixture_t3"]
	if !t3.Motivated || len(t3.ClaimRefs) != 1 || t3.ClaimRefs[0] != "dv_t22fixture_c3" {
		t.Errorf("t3 motivated check = %+v, want motivated by dv_t22fixture_c3", t3)
	}

	// t2 covers c2, which this test's minimal frame never declared -- an
	// honest "not motivated", proving the check does not silently pass
	// everything.
	t2 := motivated["dv_t22fixture_t2"]
	if t2.Motivated {
		t.Errorf("t2 motivated check = %+v, want NOT motivated: c2 was never declared in this test's minimal frame", t2)
	}
}
