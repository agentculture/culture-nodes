package handover_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/handover"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

const (
	testCommit      = "774d5153c32a2e2fdb86f699d814977d111f1408"
	testHandoverRef = "refs/culture-nodes/01M04CJT84WD20GDQEN266J9J6/" +
		"01M04CJT86JEGZC5N9VBTV3Q9D-att_01M04CJTG8VT0TJRPCJ1Z7P7J9-20260816T041727Z-4ba48f"
)

func passingVerdict() handover.SuiteVerdict {
	return handover.SuiteVerdict{
		RunID:            "01M04CJT84WD20GDQEN266J9J6",
		NodeRunID:        "01M04CJT86JEGZC5N9VBTV3Q9D",
		AttemptID:        "01M04CKW1Y1P0ATRBF07FDNWAZ",
		Suite:            "go test ./...",
		Command:          []string{"go", "test", "./..."},
		ExitCode:         0,
		CommitSHA:        testCommit,
		Ref:              testHandoverRef,
		ValidatorActorID: "culture-nodes/merge-gate",
		EvaluatedAt:      time.Date(2026, 8, 16, 4, 20, 0, 0, time.UTC),
	}
}

// TestAVerdictIsDerivedFromAValidatorNotProposedByAnAgent pins the authority
// vocabulary this whole task turns on (PRD §10.4): a suite's pass/fail is a
// deterministic validator's output, so the record is `derived` with validator
// origin — never `proposed` (which is what an agent's word is worth), never
// `observed` (which belongs to a trusted runner measuring a fact), and never
// `confirmed` (which only a human review transaction produces).
func TestAVerdictIsDerivedFromAValidatorNotProposedByAnAgent(t *testing.T) {
	rec, err := passingVerdict().Record()
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if rec.Authority != ledger.AuthorityDerived {
		t.Errorf("authority = %q, want %q", rec.Authority, ledger.AuthorityDerived)
	}
	if rec.Origin.Kind != ledger.OriginValidator {
		t.Errorf("origin kind = %q, want %q", rec.Origin.Kind, ledger.OriginValidator)
	}
	if rec.Origin.ActorID != "culture-nodes/merge-gate" {
		t.Errorf("origin actor = %q, want the named validator", rec.Origin.ActorID)
	}
	if rec.RecordType != ledger.RecordReview {
		t.Errorf("record_type = %q, want %q", rec.RecordType, ledger.RecordReview)
	}
}

// TestAVerdictNamesTheSuiteTheExitCodeAndTheCommitItRanAgainst is task t11's
// own contract: those three facts, in the payload, readable without a second
// lookup. A verdict that does not name what it tested is not evidence.
func TestAVerdictNamesTheSuiteTheExitCodeAndTheCommitItRanAgainst(t *testing.T) {
	rec, err := passingVerdict().Record()
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if data["suite"] != "go test ./..." {
		t.Errorf("suite = %v, want the suite that ran", data["suite"])
	}
	if data["exit_code"] != float64(0) {
		t.Errorf("exit_code = %v (%T), want 0", data["exit_code"], data["exit_code"])
	}
	if data["commit_sha"] != testCommit {
		t.Errorf("commit_sha = %v, want %s", data["commit_sha"], testCommit)
	}
	if data["verdict"] != "confirm" {
		t.Errorf("verdict = %v, want confirm for exit code 0", data["verdict"])
	}
	if data["ref"] != testHandoverRef {
		t.Errorf("ref = %v, want the handover ref that carried the commit", data["ref"])
	}
}

// TestANonZeroExitCodeRejects: the verdict label is computed from the exit
// code alone, never supplied.
func TestANonZeroExitCodeRejects(t *testing.T) {
	v := passingVerdict()
	v.ExitCode = 2
	rec, err := v.Record()
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if data["verdict"] != "reject" {
		t.Errorf("verdict = %v, want reject for a non-zero exit code", data["verdict"])
	}
	if data["exit_code"] != float64(2) {
		t.Errorf("exit_code = %v, want 2", data["exit_code"])
	}
}

// TestAVerdictWithoutAWellFormedCommitIsRefused is the load-bearing refusal.
// "go test passed" with no commit named — or with "HEAD", or a branch name,
// or an abbreviated sha — is a sentence, not evidence: nothing later can
// check what it ran against. Two suites this cycle passed while testing
// nothing, and a verdict that cannot name its subject is how that stays
// invisible.
func TestAVerdictWithoutAWellFormedCommitIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, sha string }{
		{"empty", ""},
		{"HEAD", "HEAD"},
		{"a branch name", "main"},
		{"abbreviated", "774d515"},
		{"uppercase", strings.ToUpper(testCommit)},
		{"not hex", "zzzz5153c32a2e2fdb86f699d814977d111f1408"},
		{"too long", testCommit + "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := passingVerdict()
			v.CommitSHA = tc.sha
			if _, err := v.Record(); err == nil {
				t.Fatalf("Record() accepted commit_sha %q; a verdict that does not name what it tested is not evidence", tc.sha)
			}
			var verr *handover.VerdictError
			if _, err := v.Record(); !errors.As(err, &verr) || verr.Field != "/commit_sha" {
				t.Fatalf("error = %v, want a VerdictError on /commit_sha", err)
			}
		})
	}
}

// TestAVerdictWithoutASuiteOrAValidatorIsRefused: the other two identifying
// facts. An unnamed suite cannot be re-run, and an unidentified producer is
// exactly what PRD §10.4 refuses to accept derived authority from.
func TestAVerdictWithoutASuiteOrAValidatorIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mut   func(*handover.SuiteVerdict)
		field string
	}{
		{"no suite", func(v *handover.SuiteVerdict) { v.Suite = "" }, "/suite"},
		{"blank suite", func(v *handover.SuiteVerdict) { v.Suite = "   " }, "/suite"},
		{"no validator", func(v *handover.SuiteVerdict) { v.ValidatorActorID = "" }, "/validator_actor_id"},
		{"no run", func(v *handover.SuiteVerdict) { v.RunID = "" }, "/run_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := passingVerdict()
			tc.mut(&v)
			_, err := v.Record()
			var verr *handover.VerdictError
			if !errors.As(err, &verr) {
				t.Fatalf("Record() error = %v, want a VerdictError", err)
			}
			if verr.Field != tc.field {
				t.Fatalf("VerdictError field = %q, want %q", verr.Field, tc.field)
			}
		})
	}
}

// TestAVerdictRefusesARefOutsideTheHandoverFence reuses ValidateRef rather
// than growing a second opinion about what a handover ref is: a verdict that
// claims to have tested `refs/heads/main` is naming something the collector
// is never allowed to have fetched.
func TestAVerdictRefusesARefOutsideTheHandoverFence(t *testing.T) {
	v := passingVerdict()
	v.Ref = "refs/heads/main"
	_, err := v.Record()
	var verr *handover.VerdictError
	if !errors.As(err, &verr) || verr.Field != "/ref" {
		t.Fatalf("Record() error = %v, want a VerdictError on /ref", err)
	}
}

// TestAVerdictWithNoRefIsStillEvidence: the ref is how the commit travelled,
// not what was tested. A gate run over a commit already in the operator's
// repository still names a sha, and that is the fact that matters.
func TestAVerdictWithNoRefIsStillEvidence(t *testing.T) {
	v := passingVerdict()
	v.Ref = ""
	rec, err := v.Record()
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if _, present := data["ref"]; present {
		t.Errorf("ref key present with no ref collected: %v — absence must read as absence", data["ref"])
	}
	if data["commit_sha"] != testCommit {
		t.Errorf("commit_sha = %v, want it still named", data["commit_sha"])
	}
}

// TestAVerdictPointsAtTheHandoverEvidenceItJudged mirrors
// internal/worker/acceptance.go's appendAcceptanceVerdict: the derived
// verdict's SubjectRef is the observed evidence record it was computed
// against, and that record is also its provenance.
func TestAVerdictPointsAtTheHandoverEvidenceItJudged(t *testing.T) {
	v := passingVerdict()
	v.EvidenceRecordID = "ledger_01M04CKW1Y1P0ATRBF07FDNWB0"
	rec, err := v.Record()
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if rec.SubjectRef.String() != v.EvidenceRecordID {
		t.Errorf("subject_ref = %q, want the handover evidence record %s", rec.SubjectRef, v.EvidenceRecordID)
	}
	if len(rec.ProvenanceRefs) != 1 || rec.ProvenanceRefs[0] != v.EvidenceRecordID {
		t.Errorf("provenance_refs = %v, want [%s]", rec.ProvenanceRefs, v.EvidenceRecordID)
	}
}

// TestAVerdictRecordIsAcceptedByTheRealAuthorityMatrix runs the composed
// record through ledger.CheckAuthority itself, so this package cannot be
// wrong about what §10.4 admits while its own tests agree with it.
func TestAVerdictRecordIsAcceptedByTheRealAuthorityMatrix(t *testing.T) {
	rec, err := passingVerdict().Record()
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := ledger.CheckAuthority(rec, nil); err != nil {
		t.Fatalf("CheckAuthority rejected a validator-origin derived review: %v", err)
	}
}

// TestAPassingVerdictDoesNotClaimTheChangeIsCorrect: the reason line states
// the boundary of what an exit code establishes, so a reader cannot mistake
// a green suite for a review.
func TestAPassingVerdictDoesNotClaimTheChangeIsCorrect(t *testing.T) {
	rec, err := passingVerdict().Record()
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	reason, _ := data["reason"].(string)
	if !strings.Contains(reason, "go test ./...") || !strings.Contains(reason, testCommit) {
		t.Errorf("reason = %q, want it to name both the suite and the commit", reason)
	}
	if data["collection_method"] != handover.VerdictCollectionMethod {
		t.Errorf("collection_method = %v, want %q", data["collection_method"], handover.VerdictCollectionMethod)
	}
}
