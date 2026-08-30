package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/handover"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
)

// createSuiteVerdictReq mirrors components.schemas.CreateSuiteVerdictRequest
// — this package encodes the documented wire shape rather than reaching for
// internal/api's unexported request type (see api_test.go's package doc).
//
// ExitCode is a pointer for the same reason the handler's own field is: an
// absent exit code and a zero one must not be the same request.
type createSuiteVerdictReq struct {
	Suite            string   `json:"suite"`
	Command          []string `json:"command,omitempty"`
	ExitCode         *int     `json:"exit_code,omitempty"`
	CommitSHA        string   `json:"commit_sha"`
	Ref              string   `json:"ref,omitempty"`
	ValidatorActorID string   `json:"validator_actor_id"`
	NodeRunRef       string   `json:"node_run_ref,omitempty"`
	AttemptRef       string   `json:"attempt_ref,omitempty"`
	RequiresGrants   []string `json:"requires_grants,omitempty"`
	ImplicatedPaths  []string `json:"implicated_paths,omitempty"`
}

const (
	gateCommit    = "774d5153c32a2e2fdb86f699d814977d111f1408"
	otherCommit   = "0d3a04a74911079b706d7f253482bdfe2f592387"
	gateHandoverR = "refs/culture-nodes/01M04CJT84WD20GDQEN266J9J6/" +
		"01M04CJT86JEGZC5N9VBTV3Q9D-att_01M04CJTG8VT0TJRPCJ1Z7P7J9-20260816T041727Z-4ba48f"
)

func exitCode(n int) *int { return &n }

func passingGateReq(validator string) createSuiteVerdictReq {
	return createSuiteVerdictReq{
		Suite:            "go test ./...",
		Command:          []string{"go", "test", "./..."},
		ExitCode:         exitCode(0),
		CommitSHA:        gateCommit,
		Ref:              gateHandoverR,
		ValidatorActorID: validator,
	}
}

func verdictPayload(t *testing.T, rec ledger.Record) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		t.Fatalf("decode verdict payload: %v", err)
	}
	return data
}

// seedHandoverEvidence appends the observed evidence record a real handover
// fetch would have written for this run, so the tests below can exercise the
// cross-check against a control plane that HAS measured the run's handover.
// It goes through handover.Observer.BuildRecord and the real ledger, not a
// hand-rolled payload — the same record the callback path writes.
func seedHandoverEvidence(t *testing.T, f *fixture, runID, commit string) ledger.Record {
	t.Helper()
	runnerActor := f.insertActorKind("handover-fetch", "runner")
	obs := &handover.Observer{ActorID: runnerActor}
	rec, manifest, err := obs.BuildRecord(
		handover.Claim{RunID: runID, Ref: gateHandoverR},
		handover.Measurement{
			Ref:          gateHandoverR,
			CommitSHA:    commit,
			ChangedPaths: []string{"docs/handover-probe-r8.md"},
			Source:       "ssh://example.invalid/repo",
			FetchedAt:    time.Now().UTC(),
		})
	if err != nil {
		t.Fatalf("BuildRecord: %v", err)
	}
	appended, err := f.api.Ledger.Append(context.Background(), rec, ledger.WithRunnerManifest(manifest))
	if err != nil {
		t.Fatalf("append handover evidence: %v", err)
	}
	return appended
}

// TestSuiteVerdictLandsDerivedFromAValidator is task t11's headline: the
// merge gate posts what the suite did, and the ledger gains a `derived`
// record from a validator — not an operator's say-so that a tick was green.
func TestSuiteVerdictLandsDerivedFromAValidator(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")

	var out suiteVerdictOut
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), decisionAuthSecret,
		passingGateReq(validator), &out)
	requireStatus(t, resp, body, http.StatusCreated)

	if out.Verdict.Authority != ledger.AuthorityDerived {
		t.Errorf("authority = %q, want %q", out.Verdict.Authority, ledger.AuthorityDerived)
	}
	if out.Verdict.Origin.Kind != ledger.OriginValidator || out.Verdict.Origin.ActorID != validator {
		t.Errorf("origin = %+v, want validator origin actor %s", out.Verdict.Origin, validator)
	}
	data := verdictPayload(t, out.Verdict)
	if data["suite"] != "go test ./..." || data["exit_code"] != float64(0) || data["commit_sha"] != gateCommit {
		t.Errorf("payload = %v, want it to name the suite, the exit code and the commit", data)
	}
	if data["verdict"] != "confirm" {
		t.Errorf("verdict = %v, want confirm", data["verdict"])
	}
}

func TestSuiteVerdictKeepsAttemptRefWithoutInventingAnAttemptForeignKey(t *testing.T) {
	for _, tc := range []struct {
		name     string
		complete bool
	}{
		{name: "in flight"},
		{name: "completed", complete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
			run, nodeRunID := createMinimalRun(t, f)
			validator := f.insertActorKind("merge-gate", "validator")
			attemptRef := "att_" + store.NewULID()
			if tc.complete {
				attemptRef = insertCompletedAttempt(t, f, nodeRunID)
			}
			req := passingGateReq(validator)
			req.NodeRunRef, req.AttemptRef = nodeRunID, attemptRef

			var out suiteVerdictOut
			resp, body := doJSONBearer(t, f.client, http.MethodPost,
				f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), decisionAuthSecret, req, &out)
			requireStatus(t, resp, body, http.StatusCreated)

			if got := verdictPayload(t, out.Verdict)["attempt_ref"]; got != attemptRef {
				t.Errorf("data.attempt_ref = %v, want %s", got, attemptRef)
			}
			wantAttemptID := ""
			if tc.complete {
				wantAttemptID = attemptRef
			}
			if out.Verdict.AttemptID.String() != wantAttemptID {
				t.Errorf("attempt_id = %q, want %q", out.Verdict.AttemptID, wantAttemptID)
			}
		})
	}
}

// TestSuiteVerdictRecordsAFailingSuiteToo: a red gate is evidence in exactly
// the same way a green one is. The record is appended, and it says reject.
func TestSuiteVerdictRecordsAFailingSuiteToo(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")

	req := passingGateReq(validator)
	req.ExitCode = exitCode(1)

	var out suiteVerdictOut
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), decisionAuthSecret, req, &out)
	requireStatus(t, resp, body, http.StatusCreated)
	if verdictPayload(t, out.Verdict)["verdict"] != "reject" {
		t.Errorf("verdict = %v, want reject", verdictPayload(t, out.Verdict)["verdict"])
	}
}

// TestSuiteVerdictRefusesACommitTheRunDidNotHandOver is the load-bearing
// enforcement. The control plane measured this run's handover at one commit;
// a verdict naming a different one is a suite that ran against something
// else, and recording it would produce exactly the false green this task
// exists to stop. It is refused rather than recorded-with-a-caveat.
func TestSuiteVerdictRefusesACommitTheRunDidNotHandOver(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")
	seedHandoverEvidence(t, f, run.ID, gateCommit)

	req := passingGateReq(validator)
	req.CommitSHA = otherCommit

	var errBody apiErrorBody
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), decisionAuthSecret, req, &errBody)
	requireStatus(t, resp, body, http.StatusBadRequest)
	if !strings.Contains(errBody.Message, gateCommit) || !strings.Contains(errBody.Message, otherCommit) {
		t.Errorf("message = %q, want both the measured and the tested commit named", errBody.Message)
	}

	// And nothing was written: a refused verdict leaves no half-record.
	var records struct {
		Items []ledger.Record `json:"items"`
	}
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID+"/ledger"), nil, &records)
	requireStatus(t, resp, body, http.StatusOK)
	for _, r := range records.Items {
		if r.Authority == ledger.AuthorityDerived {
			t.Fatalf("a derived record was appended despite the refusal: %+v", r)
		}
	}
}

// TestSuiteVerdictPointsAtTheHandoverEvidenceItJudged: when the commit does
// match what the control plane measured, the verdict names that evidence
// record as its subject and its provenance — the shape
// internal/worker/acceptance.go's appendAcceptanceVerdict uses.
func TestSuiteVerdictPointsAtTheHandoverEvidenceItJudged(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")
	evidence := seedHandoverEvidence(t, f, run.ID, gateCommit)

	var out suiteVerdictOut
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), decisionAuthSecret,
		passingGateReq(validator), &out)
	requireStatus(t, resp, body, http.StatusCreated)

	if out.Verdict.SubjectRef.String() != evidence.ID {
		t.Errorf("subject_ref = %q, want the handover evidence record %s", out.Verdict.SubjectRef, evidence.ID)
	}
	if len(out.Verdict.ProvenanceRefs) != 1 || out.Verdict.ProvenanceRefs[0] != evidence.ID {
		t.Errorf("provenance_refs = %v, want [%s]", out.Verdict.ProvenanceRefs, evidence.ID)
	}
}

// TestSuiteVerdictWithoutMeasuredHandoverIsStillRecorded: after issue #120
// the deployed control plane has no handover evidence for most runs, and a
// gate that only worked once one existed would be a gate nobody could run.
// The verdict lands, with no subject to point at — it says what it tested,
// and does not imply the control plane corroborated it.
func TestSuiteVerdictWithoutMeasuredHandoverIsStillRecorded(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")

	var out suiteVerdictOut
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), decisionAuthSecret,
		passingGateReq(validator), &out)
	requireStatus(t, resp, body, http.StatusCreated)
	if out.Verdict.SubjectRef.String() != "" {
		t.Errorf("subject_ref = %q, want empty when the control plane measured no handover", out.Verdict.SubjectRef)
	}
	if verdictPayload(t, out.Verdict)["commit_sha"] != gateCommit {
		t.Error("the verdict must still name the commit it tested")
	}
}

// TestSuiteVerdictRefusesAnAbsentExitCode: an omitted exit code must not be
// read as a passing one. This is the single most dangerous default the wire
// shape could have.
func TestSuiteVerdictRefusesAnAbsentExitCode(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")

	req := passingGateReq(validator)
	req.ExitCode = nil

	var errBody apiErrorBody
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), decisionAuthSecret, req, &errBody)
	requireStatus(t, resp, body, http.StatusBadRequest)
	if !strings.Contains(errBody.Message, "exit_code") {
		t.Errorf("message = %q, want it to name exit_code", errBody.Message)
	}
}

// TestSuiteVerdictRefusesAVerdictNamingNoCommit is the handler's half of
// internal/handover's own refusal: a verdict that does not name what it
// tested never reaches the ledger.
func TestSuiteVerdictRefusesAVerdictNamingNoCommit(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")

	for _, sha := range []string{"", "HEAD", "main", "774d515"} {
		req := passingGateReq(validator)
		req.CommitSHA = sha
		var errBody apiErrorBody
		resp, body := doJSONBearer(t, f.client, http.MethodPost,
			f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), decisionAuthSecret, req, &errBody)
		requireStatus(t, resp, body, http.StatusBadRequest)
		if !strings.Contains(errBody.Message, "commit_sha") {
			t.Errorf("commit_sha %q: message = %q, want it to name commit_sha", sha, errBody.Message)
		}
	}
}

// TestSuiteVerdictRefusesAHumanValidator is checkReviewerIsHuman's mirror
// (internal/ledger, task t30). That one refuses a non-human deciding a
// claim; this one refuses a human being recorded as a deterministic
// validator. A person running a suite and reporting the result is making a
// claim about it — which is the operator-reading-a-green-tick this task set
// out to replace.
func TestSuiteVerdictRefusesAHumanValidator(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	human := f.insertActorKind("ops-person", "human")

	var errBody apiErrorBody
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), decisionAuthSecret,
		passingGateReq(human), &errBody)
	requireStatus(t, resp, body, http.StatusBadRequest)
	if !strings.Contains(errBody.Message, "human") {
		t.Errorf("message = %q, want it to explain that a human is not a deterministic validator", errBody.Message)
	}
}

// TestSuiteVerdictRefusesAnUnregisteredValidator: an unidentified producer
// cannot hold derived authority (PRD §10.4).
func TestSuiteVerdictRefusesAnUnregisteredValidator(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)

	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), decisionAuthSecret,
		passingGateReq("actor_that_was_never_registered"), nil)
	requireStatus(t, resp, body, http.StatusNotFound)
}

// TestSuiteVerdictRequiresTheDecisionBearer: whoever can write a verdict can
// decide a merge, so the route rides the same standing the review surface
// does rather than being open.
func TestSuiteVerdictRequiresTheDecisionBearer(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate", "validator")

	resp, body := doJSON(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), passingGateReq(validator), nil)
	requireStatus(t, resp, body, http.StatusUnauthorized)

	resp, body = doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+run.ID+"/suite-verdicts"), "not-the-secret",
		passingGateReq(validator), nil)
	requireStatus(t, resp, body, http.StatusUnauthorized)
}

// TestSuiteVerdictOnAnUnknownRunIs404 keeps the run check ahead of the
// append, like every other run-scoped route here.
func TestSuiteVerdictOnAnUnknownRunIs404(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	validator := f.insertActorKind("merge-gate", "validator")

	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/run_does_not_exist/suite-verdicts"), decisionAuthSecret,
		passingGateReq(validator), nil)
	requireStatus(t, resp, body, http.StatusNotFound)
}
