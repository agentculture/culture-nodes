package handover_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/handover"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

const (
	gateCandidateSHA = "774d5153c32a2e2fdb86f699d814977d111f1408"
	gateBaseSHA      = "0d3a04a74911079b706d7f253482bdfe2f592387"
	gateValidator    = "merge_gate_validator"
)

func gatePayload(t *testing.T, rec ledger.Record) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		t.Fatalf("decode gate payload: %v", err)
	}
	return data
}

func notApplicable() handover.GateNotApplicable {
	return handover.GateNotApplicable{
		RunID:                  "01M05ZGNT86MAFDHATB6W5VYPN",
		Gate:                   "go-test",
		Suite:                  "go test ./...",
		Instrument:             "go test",
		Reason:                 handover.ReasonInstrumentUnavailable,
		UncoveredPaths:         []string{"internal/api/gatereports.go"},
		ChangedFilesConsidered: []string{"internal/api/gatereports.go", "docs/x.md"},
		CommitSHA:              gateCandidateSHA,
		ValidatorActorID:       gateValidator,
	}
}

// TestNotApplicableGateIsDerivedFromAValidator: criterion 3. Every gate
// record, including the ones that measured nothing, is derived and
// validator-origin — never an agent's proposal.
func TestNotApplicableGateIsDerivedFromAValidator(t *testing.T) {
	rec, err := notApplicable().Record()
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if rec.Authority != ledger.AuthorityDerived {
		t.Errorf("Authority = %q, want %q", rec.Authority, ledger.AuthorityDerived)
	}
	if rec.Origin.Kind != ledger.OriginValidator || rec.Origin.ActorID != gateValidator {
		t.Errorf("Origin = %+v, want a validator origin", rec.Origin)
	}
}

// TestNotApplicableGateCarriesNoVerdictAndNamesWhatItMissed is criterion 4's
// per-gate half. The record must be impossible to read as a pass: no `verdict`
// key at all, an explicit `not_applicable` status, a reason from the closed
// vocabulary, and the files the instrument did not cover.
func TestNotApplicableGateCarriesNoVerdictAndNamesWhatItMissed(t *testing.T) {
	rec, err := notApplicable().Record()
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	data := gatePayload(t, rec)

	if _, present := data["verdict"]; present {
		t.Errorf("payload carries a verdict key (%v); a gate that measured nothing reached no verdict, "+
			"and writing one makes it sortable beside gates that did", data["verdict"])
	}
	if data["gate_status"] != handover.GateStatusNotApplicable {
		t.Errorf("gate_status = %v, want %q", data["gate_status"], handover.GateStatusNotApplicable)
	}
	if data["reason"] != handover.ReasonInstrumentUnavailable {
		t.Errorf("reason = %v, want %q", data["reason"], handover.ReasonInstrumentUnavailable)
	}
	paths, _ := data["uncovered_paths"].([]any)
	if len(paths) != 1 || paths[0] != "internal/api/gatereports.go" {
		t.Errorf("uncovered_paths = %v, want the one changed file the instrument did not cover", data["uncovered_paths"])
	}
	if data["commit_sha"] != gateCandidateSHA {
		t.Errorf("commit_sha = %v, want %s", data["commit_sha"], gateCandidateSHA)
	}
}

// TestNotApplicableGateRefusesAnUnnamedMiss: "not applicable" with no subject
// is indistinguishable from "nobody ran this", and would read as a pass by
// omission.
func TestNotApplicableGateRefusesAnUnnamedMiss(t *testing.T) {
	g := notApplicable()
	g.UncoveredPaths = nil
	if _, err := g.Record(); err == nil {
		t.Fatal("Record accepted a not-applicable gate that named no uncovered file; want a refusal")
	}
}

// TestNotApplicableGateRefusesAFreeTextReason keeps the vocabulary closed, so
// "which trees does no instrument reach" stays a query rather than a read.
func TestNotApplicableGateRefusesAFreeTextReason(t *testing.T) {
	g := notApplicable()
	g.Reason = "we did not get round to it"
	_, err := g.Record()
	if err == nil {
		t.Fatal("Record accepted a free-text reason; want a refusal")
	}
	if !strings.Contains(err.Error(), handover.ReasonInstrumentUnavailable) {
		t.Errorf("refusal does not name the accepted vocabulary: %v", err)
	}
}

// TestNotApplicableGateAllowsNoSourceFilesToNameNothing is the one reason with
// nothing to name: a docs-only change genuinely has no uncovered source file.
func TestNotApplicableGateAllowsNoSourceFilesToNameNothing(t *testing.T) {
	g := notApplicable()
	g.Reason = handover.ReasonNoSourceFiles
	g.UncoveredPaths = nil
	if _, err := g.Record(); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// TestNotApplicableGateRefusesAnUnnamedCommit inherits verdict.go's fence: a
// finding that does not name what it was about is not evidence.
func TestNotApplicableGateRefusesAnUnnamedCommit(t *testing.T) {
	g := notApplicable()
	g.CommitSHA = "HEAD"
	if _, err := g.Record(); err == nil {
		t.Fatal("Record accepted a symbolic commit; want a refusal")
	}
}

func aggregate(results handover.GateResults) handover.GateAggregate {
	return handover.GateAggregate{
		RunID:            "01M05ZGNT86MAFDHATB6W5VYPN",
		Results:          results,
		BaseSHA:          gateBaseSHA,
		CommitSHA:        gateCandidateSHA,
		ChangedFiles:     []string{"internal/api/gatereports.go"},
		ValidatorActorID: gateValidator,
	}
}

// TestAggregateCountsAreComputedNotAsserted is criterion 4's aggregate half.
// The four counts come from the per-gate statuses, so nothing a caller says
// can make an empty scan look green.
func TestAggregateCountsAreComputedNotAsserted(t *testing.T) {
	rec, err := aggregate(handover.GateResults{
		{Gate: "go-test", Status: handover.GateStatusPassed},
		{Gate: "pytest", Status: handover.GateStatusPassed},
		{Gate: "coverage", Status: handover.GateStatusNotApplicable, Reason: handover.ReasonInstrumentNotReachingTree},
		{Gate: "web-build", Status: handover.GateStatusFailed},
	}).Record()
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	data := gatePayload(t, rec)

	for key, want := range map[string]float64{
		"declared_gate_count":       4,
		"applicable_gate_count":     3,
		"passed_gate_count":         2,
		"failed_gate_count":         1,
		"not_applicable_gate_count": 1,
	} {
		if data[key] != want {
			t.Errorf("%s = %v, want %v", key, data[key], want)
		}
	}
	if data["gate_status"] != handover.OutcomeChangesRequired {
		t.Errorf("gate_status = %v, want %q — a failing gate is a domain answer that follows an edge",
			data["gate_status"], handover.OutcomeChangesRequired)
	}
	if data["verdict"] != "changes_requested" {
		t.Errorf("verdict = %v, want changes_requested", data["verdict"])
	}
}

// TestEmptyScanIsNeverGreen is the rule the whole aggregate exists for: zero
// failures out of zero measurements is arithmetically a pass and
// substantively nothing.
func TestEmptyScanIsNeverGreen(t *testing.T) {
	results := handover.GateResults{
		{Gate: "coverage", Status: handover.GateStatusNotApplicable, Reason: handover.ReasonInstrumentNotReachingTree},
		{Gate: "complexity", Status: handover.GateStatusNotApplicable, Reason: handover.ReasonNoSourceFiles},
	}
	if got := results.Outcome(); got != handover.OutcomeMeasurementIncomplete {
		t.Fatalf("Outcome = %q, want %q", got, handover.OutcomeMeasurementIncomplete)
	}
	rec, err := aggregate(results).Record()
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	data := gatePayload(t, rec)
	if _, present := data["verdict"]; present {
		t.Errorf("an aggregate that measured nothing carries a verdict key (%v); it reached none", data["verdict"])
	}
}

// TestAnUnavailableInstrumentIsIncompleteNotGreenAndNotRed: criterion 5's
// mechanical half. A host without the toolchain must not produce a passing
// gate (a false green) and must not produce a failing one (a manufactured
// defect the repair router would then act on).
func TestAnUnavailableInstrumentIsIncompleteNotGreenAndNotRed(t *testing.T) {
	results := handover.GateResults{
		{Gate: "markdownlint", Status: handover.GateStatusPassed},
		{Gate: "go-test", Status: handover.GateStatusNotApplicable, Reason: handover.ReasonInstrumentUnavailable},
	}
	if got := results.Outcome(); got != handover.OutcomeMeasurementIncomplete {
		t.Fatalf("Outcome = %q, want %q", got, handover.OutcomeMeasurementIncomplete)
	}
}

func TestAllSkippedGoSuiteIsIncompleteNotGreen(t *testing.T) {
	results := handover.GateResults{
		{Gate: "lint", Status: handover.GateStatusPassed},
		{Gate: "go-test", Status: handover.GateStatusNotApplicable, Reason: handover.ReasonNoTestsExecuted},
	}
	if got := results.Outcome(); got != handover.OutcomeMeasurementIncomplete {
		t.Fatalf("Outcome = %q, want %q; all-skipped tests are not green", got, handover.OutcomeMeasurementIncomplete)
	}
}

// TestKnownInstrumentGapsDoNotBlockAPassingGate is the counterweight. The
// coverage and complexity instruments genuinely do not reach `internal/` yet
// (issue #88); if that recorded gap made every run incomplete, the outcome
// would carry no signal at all.
func TestKnownInstrumentGapsDoNotBlockAPassingGate(t *testing.T) {
	results := handover.GateResults{
		{Gate: "go-test", Status: handover.GateStatusPassed},
		{Gate: "coverage", Status: handover.GateStatusNotApplicable, Reason: handover.ReasonInstrumentNotReachingTree},
	}
	if got := results.Outcome(); got != handover.OutcomeGatesPassed {
		t.Fatalf("Outcome = %q, want %q", got, handover.OutcomeGatesPassed)
	}
}

// TestAggregateRefusesZeroGates: an aggregate over nothing is not a finding.
func TestAggregateRefusesZeroGates(t *testing.T) {
	if _, err := aggregate(nil).Record(); err == nil {
		t.Fatal("Record accepted an aggregate over zero gates; want a refusal")
	}
}

// TestAggregateProvenanceNamesEveryRecordItCounted makes the derivation
// re-performable: a reader can fetch the counted records rather than trust the
// arithmetic.
func TestAggregateProvenanceNamesEveryRecordItCounted(t *testing.T) {
	rec, err := aggregate(handover.GateResults{
		{Gate: "go-test", Status: handover.GateStatusPassed, RecordID: "lr_1"},
		{Gate: "pytest", Status: handover.GateStatusPassed, RecordID: "lr_2"},
	}).Record()
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	joined := strings.Join(rec.ProvenanceRefs, ",")
	if !strings.Contains(joined, "lr_1") || !strings.Contains(joined, "lr_2") {
		t.Errorf("ProvenanceRefs = %v, want both counted records", rec.ProvenanceRefs)
	}
}

// TestGateExitCodesAreThePublishedContract pins the exit-status contract the
// gate program, the worker's outcome table and the example workflow all speak.
func TestGateExitCodesAreThePublishedContract(t *testing.T) {
	for outcome, want := range map[string]int{
		handover.OutcomeGatesPassed:           0,
		handover.OutcomeChangesRequired:       1,
		handover.OutcomeMeasurementIncomplete: 2,
	} {
		got, ok := handover.GateExitCode(outcome)
		if !ok || got != want {
			t.Errorf("GateExitCode(%q) = (%d, %v), want (%d, true)", outcome, got, ok, want)
		}
	}
	if _, ok := handover.GateExitCode("passed"); ok {
		t.Error("GateExitCode accepted an outcome outside the gate vocabulary")
	}
}
