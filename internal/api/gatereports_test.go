package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/handover"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The gate-report wire shapes, encoded here rather than reached for out of
// internal/api, for the reason api_test.go's package doc gives: a test that
// shares the handler's own structs cannot catch a change to the documented
// wire contract.

type gateReportReq struct {
	CommitSHA        string         `json:"commit_sha"`
	PackageCommitSHA string         `json:"package_commit_sha,omitempty"`
	BaseSHA          string         `json:"base_sha,omitempty"`
	Ref              string         `json:"ref,omitempty"`
	ChangedFiles     []string       `json:"changed_files,omitempty"`
	ValidatorActorID string         `json:"validator_actor_id"`
	NodeRunRef       string         `json:"node_run_ref,omitempty"`
	AttemptRef       string         `json:"attempt_ref,omitempty"`
	Gates            []gateEntryReq `json:"gates"`
}

type gateEntryReq struct {
	Gate              string          `json:"gate"`
	Suite             string          `json:"suite,omitempty"`
	Command           []string        `json:"command,omitempty"`
	Instrument        string          `json:"instrument,omitempty"`
	InstrumentVersion string          `json:"instrument_version,omitempty"`
	ExitCode          *int            `json:"exit_code,omitempty"`
	NotApplicable     *gateNAReq      `json:"not_applicable,omitempty"`
	Considered        []string        `json:"changed_files_considered,omitempty"`
	Measurement       *gateMeasureReq `json:"measurement,omitempty"`
	Repair            *gateRepairReq  `json:"repair,omitempty"`
}

type gateNAReq struct {
	Reason         string   `json:"reason"`
	UncoveredPaths []string `json:"uncovered_paths,omitempty"`
}

type gateMeasureReq struct {
	Value     *float64        `json:"value,omitempty"`
	Unit      string          `json:"unit,omitempty"`
	Threshold json.RawMessage `json:"threshold,omitempty"`
}

type gateRepairReq struct {
	RequiresGrants  []string `json:"requires_grants,omitempty"`
	ImplicatedPaths []string `json:"implicated_paths,omitempty"`
	RepairActorID   string   `json:"repair_actor_id,omitempty"`
}

type gateReportOut struct {
	Gates         []ledger.Record     `json:"gates"`
	Aggregate     ledger.Record       `json:"aggregate"`
	Counts        handover.GateCounts `json:"counts"`
	Outcome       string              `json:"outcome"`
	ExitCode      int                 `json:"exit_code"`
	Routings      []ledger.Record     `json:"routings"`
	RoutingErrors []string            `json:"routing_errors"`
}

func postGateReport(t *testing.T, f *fixture, runID string, req gateReportReq, want int) (gateReportOut, []byte) {
	t.Helper()
	var out gateReportOut
	resp, body := doJSONBearer(t, f.client, http.MethodPost,
		f.url("/v1alpha1/runs/"+runID+"/gate-reports"), decisionAuthSecret, req, &out)
	requireStatus(t, resp, body, want)
	return out, body
}

func insertCompletedAttempt(t *testing.T, f *fixture, nodeRunID string) string {
	t.Helper()
	attemptID := store.NewULID()
	es, err := storepg.NewEngineStore(f.store, f.nsID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	err = es.InTx(context.Background(), func(ctx context.Context, tx engine.Tx) error {
		return tx.InsertAttempt(ctx, engine.Attempt{
			ID: attemptID, NodeRunID: nodeRunID, Number: 1, Status: engine.StatusSucceeded,
		})
	})
	if err != nil {
		t.Fatalf("insert completed attempt: %v", err)
	}
	return attemptID
}

func TestGateReportKeepsAttemptRefWithoutInventingAnAttemptForeignKey(t *testing.T) {
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
			validator := f.insertActorKind("merge-gate-node", "validator")
			attemptRef := "att_" + store.NewULID()
			if tc.complete {
				attemptRef = insertCompletedAttempt(t, f, nodeRunID)
			}

			out, _ := postGateReport(t, f, run.ID, gateReportReq{
				CommitSHA: gateCommit, ValidatorActorID: validator,
				NodeRunRef: nodeRunID, AttemptRef: attemptRef,
				Gates: []gateEntryReq{{Gate: "go-test", ExitCode: exitCode(0)}},
			}, http.StatusCreated)

			for _, rec := range append(out.Gates, out.Aggregate) {
				if got := verdictPayload(t, rec)["attempt_ref"]; got != attemptRef {
					t.Errorf("record %s data.attempt_ref = %v, want %s", rec.ID, got, attemptRef)
				}
				wantAttemptID := ""
				if tc.complete {
					wantAttemptID = attemptRef
				}
				if rec.AttemptID.String() != wantAttemptID {
					t.Errorf("record %s attempt_id = %q, want %q", rec.ID, rec.AttemptID, wantAttemptID)
				}
			}
		})
	}
}

// TestGateReportRecordsEveryGateAsDerivedFromAValidator is criterion 3: every
// record the merge path reads is derived, from a validator — the aggregate
// included. Nothing an agent proposed appears anywhere in it.
func TestGateReportRecordsEveryGateAsDerivedFromAValidator(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate-node", "validator")

	out, _ := postGateReport(t, f, run.ID, gateReportReq{
		CommitSHA:        gateCommit,
		BaseSHA:          otherCommit,
		ChangedFiles:     []string{"internal/api/gatereports.go"},
		ValidatorActorID: validator,
		Gates: []gateEntryReq{
			{Gate: "go-test", Suite: "go test ./...", Command: []string{"go", "test", "./..."}, ExitCode: exitCode(0)},
			{
				Gate:          "coverage",
				Instrument:    "coverage.py",
				NotApplicable: &gateNAReq{Reason: handover.ReasonInstrumentNotReachingTree, UncoveredPaths: []string{"internal/api/gatereports.go"}},
			},
		},
	}, http.StatusCreated)

	if len(out.Gates) != 2 {
		t.Fatalf("recorded %d gate record(s), want 2", len(out.Gates))
	}
	for _, rec := range append(append([]ledger.Record{}, out.Gates...), out.Aggregate) {
		if rec.Authority != ledger.AuthorityDerived {
			t.Errorf("record %s authority = %q, want %q", rec.ID, rec.Authority, ledger.AuthorityDerived)
		}
		if rec.Origin.Kind != ledger.OriginValidator || rec.Origin.ActorID != validator {
			t.Errorf("record %s origin = %+v, want validator %s", rec.ID, rec.Origin, validator)
		}
	}
}

// TestGateReportCountsAreComputedByTheControlPlane is criterion 4: the four
// counts and the outcome come from the per-gate statuses, so an empty scan
// cannot look green no matter what the caller believed.
func TestGateReportCountsAreComputedByTheControlPlane(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate-node", "validator")

	out, _ := postGateReport(t, f, run.ID, gateReportReq{
		CommitSHA:        gateCommit,
		ValidatorActorID: validator,
		Gates: []gateEntryReq{
			{
				Gate:          "coverage",
				Instrument:    "coverage.py",
				NotApplicable: &gateNAReq{Reason: handover.ReasonInstrumentNotReachingTree, UncoveredPaths: []string{"internal/x.go"}},
			},
			{
				Gate:          "complexity",
				Instrument:    "sonar",
				NotApplicable: &gateNAReq{Reason: handover.ReasonInstrumentNotReachingTree, UncoveredPaths: []string{"internal/x.go"}},
			},
		},
	}, http.StatusCreated)

	if out.Counts.Applicable != 0 || out.Counts.NotApplicable != 2 || out.Counts.Declared != 2 {
		t.Errorf("counts = %+v, want two declared gates, neither applicable", out.Counts)
	}
	if out.Outcome != handover.OutcomeMeasurementIncomplete {
		t.Errorf("outcome = %q, want %q — a scan that measured nothing is not a pass",
			out.Outcome, handover.OutcomeMeasurementIncomplete)
	}
	if out.ExitCode != 2 {
		t.Errorf("exit_code = %d, want 2", out.ExitCode)
	}
	data := verdictPayload(t, out.Aggregate)
	if _, present := data["verdict"]; present {
		t.Errorf("aggregate carries a verdict key (%v) for a report that reached none", data["verdict"])
	}
}

// TestGateReportRefusesAGateThatStatesNoFinding closes the two shapes in which
// an unmeasured gate could pass itself off as a measured one.
func TestGateReportRefusesAGateThatStatesNoFinding(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate-node", "validator")

	for name, gate := range map[string]gateEntryReq{
		"neither": {Gate: "go-test", Suite: "go test ./..."},
		"both": {
			Gate:          "go-test",
			Suite:         "go test ./...",
			ExitCode:      exitCode(0),
			NotApplicable: &gateNAReq{Reason: handover.ReasonNoSourceFiles},
		},
	} {
		t.Run(name, func(t *testing.T) {
			postGateReport(t, f, run.ID, gateReportReq{
				CommitSHA:        gateCommit,
				ValidatorActorID: validator,
				Gates:            []gateEntryReq{gate},
			}, http.StatusBadRequest)
		})
	}
}

// TestGateReportRefusesAnUnnamedNotApplicableMiss: criterion 4 again, at the
// wire. A not-applicable gate must name the files it did not cover.
func TestGateReportRefusesAnUnnamedNotApplicableMiss(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate-node", "validator")

	postGateReport(t, f, run.ID, gateReportReq{
		CommitSHA:        gateCommit,
		ValidatorActorID: validator,
		Gates: []gateEntryReq{{
			Gate:          "go-test",
			NotApplicable: &gateNAReq{Reason: handover.ReasonInstrumentUnavailable},
		}},
	}, http.StatusBadRequest)
}

// TestGateReportRefusesAHumanValidator mirrors the single-suite endpoint: a
// person who ran the suites and typed the result is making a claim about them,
// not producing a deterministic validator's output (PRD §10.4).
func TestGateReportRefusesAHumanValidator(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	human := f.insertActorKind("the-operator", "human")

	postGateReport(t, f, run.ID, gateReportReq{
		CommitSHA:        gateCommit,
		ValidatorActorID: human,
		Gates:            []gateEntryReq{{Gate: "go-test", ExitCode: exitCode(0)}},
	}, http.StatusBadRequest)
}

// TestGateReportRefusesAReportAgainstAnotherCommit: a gate that ran against
// something other than what the run handed over is refused outright rather
// than recorded with a caveat.
func TestGateReportRefusesAReportAgainstAnotherCommit(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate-node", "validator")
	seedHandoverEvidence(t, f, run.ID, gateCommit)

	postGateReport(t, f, run.ID, gateReportReq{
		CommitSHA:        otherCommit,
		ValidatorActorID: validator,
		Gates:            []gateEntryReq{{Gate: "go-test", ExitCode: exitCode(0)}},
	}, http.StatusBadRequest)
}

func TestGateReportRecordsVerdictOnCombinationBoundToMeasuredPackage(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate-node", "validator")
	seedHandoverEvidence(t, f, run.ID, gateCommit)

	out, _ := postGateReport(t, f, run.ID, gateReportReq{
		CommitSHA:        otherCommit,
		PackageCommitSHA: gateCommit,
		BaseSHA:          gateCommit,
		ValidatorActorID: validator,
		Gates:            []gateEntryReq{{Gate: "combination", ExitCode: exitCode(1)}},
	}, http.StatusCreated)

	if out.Outcome != handover.OutcomeChangesRequired {
		t.Fatalf("outcome = %q, want %q", out.Outcome, handover.OutcomeChangesRequired)
	}
	data := verdictPayload(t, out.Aggregate)
	if data["commit_sha"] != otherCommit || data["handover_evidence_ref"] == "" {
		t.Fatalf("aggregate payload = %v, want candidate commit bound to handover evidence", data)
	}
}

// TestGateReportRoutesEachFailingGate proves a red gate inside a report
// reaches the same bounded repair routing (task t32) a singly-posted verdict
// does — and that the aggregate is `changes_required`, a domain answer that
// follows an edge rather than an engine failure.
func TestGateReportRoutesEachFailingGate(t *testing.T) {
	f := newRoutingFixture(t)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate-node", "validator")

	out, body := postGateReport(t, f, run.ID, gateReportReq{
		CommitSHA:        gateCommit,
		ValidatorActorID: validator,
		Gates: []gateEntryReq{
			{Gate: "go-test", Suite: "go test ./...", ExitCode: exitCode(1)},
			{Gate: "pytest", Suite: "uv run pytest -n auto", ExitCode: exitCode(0)},
		},
	}, http.StatusCreated)

	if out.Outcome != handover.OutcomeChangesRequired {
		t.Errorf("outcome = %q, want %q", out.Outcome, handover.OutcomeChangesRequired)
	}
	if out.ExitCode != 1 {
		t.Errorf("exit_code = %d, want 1", out.ExitCode)
	}
	if len(out.Routings) != 1 {
		t.Errorf("routings = %d, want one per rejecting gate (routing_errors: %v)\n%s",
			len(out.Routings), out.RoutingErrors, body)
	}
}

// TestGateReportPerGateVerdictKeepsTheSuiteVerdictShape is the "build on it"
// half: a gate that ran produces exactly the record
// scripts/collect-handover.py --gate writes today, plus the gate matrix's own
// annotations. Nothing about the existing shape had to change.
func TestGateReportPerGateVerdictKeepsTheSuiteVerdictShape(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	validator := f.insertActorKind("merge-gate-node", "validator")

	threshold := json.RawMessage(`{"maximum":0}`)
	failures := 0.0
	out, _ := postGateReport(t, f, run.ID, gateReportReq{
		CommitSHA:        gateCommit,
		ValidatorActorID: validator,
		Gates: []gateEntryReq{{
			Gate:        "go-test",
			Suite:       "go test ./...",
			Command:     []string{"go", "test", "./..."},
			Instrument:  "go test",
			ExitCode:    exitCode(0),
			Measurement: &gateMeasureReq{Value: &failures, Unit: "failures", Threshold: threshold},
		}},
	}, http.StatusCreated)

	data := verdictPayload(t, out.Gates[0])
	if data["verdict"] != "confirm" || data["exit_code"] != float64(0) || data["suite"] != "go test ./..." {
		t.Errorf("payload = %v, want the shipped suite-verdict shape", data)
	}
	if data["collection_method"] != handover.VerdictCollectionMethod {
		t.Errorf("collection_method = %v, want %q", data["collection_method"], handover.VerdictCollectionMethod)
	}
	if data["gate"] != "go-test" || data["gate_status"] != handover.GateStatusPassed {
		t.Errorf("payload = %v, want the gate matrix's own annotations", data)
	}
	if data["unit"] != "failures" {
		t.Errorf("unit = %v, want the declared unit", data["unit"])
	}
}
