package runners_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/runners"
)

const testActorID = "runner_lambda"

func testContract() runners.NodeContract {
	return runners.NodeContract{
		NodeID:         "run-tests",
		SuccessOutcome: "passed",
		FailureOutcome: "failed",
		ActorID:        testActorID,
		ActorRevision:  contracts.Digest([]byte("revision")),
		RunID:          "run_01JAV3QK2M0000000000000001",
		NodeRunID:      "nr_01JAV3QK2M0000000000000008",
		AttemptID:      "att_01JAV3QK2M0000000000000010",
	}
}

// lambdaShapedResult is a result shaped the way the Lambda adapter produces
// them: platform facts measured, everything about the workspace and the
// process's own exit unmeasured.
func lambdaShapedResult(exitCode *int) runners.Result {
	res := minimalResult()
	res.Exit = &runners.Exit{Code: exitCode}
	res.Environment.PlatformRequestID = "8f5a4d2c-0000-4000-8000-000000000001"
	billed := 1300
	res.Timing.BilledDurationMs = &billed
	maxMemory := 128.0
	res.ResourceUsage = &runners.ResourceUsage{MaxMemoryMiB: &maxMemory}
	memory := 2048
	res.Environment.MemoryMiB = &memory

	res.Observations = runners.Observations{
		ExitStatus:    runners.Observation{Measured: false, Complete: false, Method: "function_reported_payload"},
		ChangedPaths:  runners.Observation{Measured: false, Complete: false},
		Logs:          runners.Observation{Measured: true, Complete: false, Method: "lambda_invoke_log_tail"},
		ResourceUsage: runners.Observation{Measured: true, Complete: false, Method: "lambda_report_line"},
		Additional: map[string]runners.Observation{
			"handler_completion":  {Measured: true, Complete: true, Method: "lambda_invoke_function_error"},
			"platform_request_id": {Measured: true, Complete: true, Method: "lambda_invoke_response_metadata"},
			"image_digest":        {Measured: true, Complete: true, Method: "lambda_get_function_resolved_image"},
			"duration":            {Measured: true, Complete: true, Method: "lambda_report_line"},
			"workspace_snapshot":  {Measured: false, Complete: false},
		},
	}
	return res
}

func intPtr(v int) *int { return &v }

// TestBuildCompletionMapsExitZeroToTheSuccessOutcome is the ordinary path.
func TestBuildCompletionMapsExitZeroToTheSuccessOutcome(t *testing.T) {
	completion, err := runners.BuildCompletion(lambdaShapedResult(intPtr(0)), testContract())
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	if completion.TechStatus != engine.StatusSucceeded {
		t.Errorf("TechStatus = %q, want succeeded", completion.TechStatus)
	}
	if completion.Outcome != "passed" {
		t.Errorf("Outcome = %q, want passed", completion.Outcome)
	}

	var output runners.CodeNodeOutput
	if err := json.Unmarshal(completion.Output, &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output.ExitCode == nil || *output.ExitCode != 0 {
		t.Errorf("output exit code = %v, want 0", output.ExitCode)
	}
	if output.PlatformRequestID == "" {
		t.Error("output does not carry the platform request id")
	}
}

// TestNonzeroExitIsADomainOutcomeWhenTheNodeDeclaresOne is PRD §3.4 in a
// test: a code node whose contract names an outcome for failure produced a
// domain answer, not an engine failure, and the engine must route it.
func TestNonzeroExitIsADomainOutcomeWhenTheNodeDeclaresOne(t *testing.T) {
	completion, err := runners.BuildCompletion(lambdaShapedResult(intPtr(1)), testContract())
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	if completion.TechStatus != engine.StatusSucceeded {
		t.Errorf("TechStatus = %q; a test suite that ran and reported failures dispatched successfully", completion.TechStatus)
	}
	if completion.Outcome != "failed" {
		t.Errorf("Outcome = %q, want the node's declared failure outcome", completion.Outcome)
	}
}

// TestNonzeroExitIsATechnicalFailureWhenTheNodeDeclaresNoOutcome is the other
// half of the same rule: a node with no domain answer for failure gets one
// technical failure, which the engine's retry policy governs.
func TestNonzeroExitIsATechnicalFailureWhenTheNodeDeclaresNoOutcome(t *testing.T) {
	contract := testContract()
	contract.FailureOutcome = ""

	completion, err := runners.BuildCompletion(lambdaShapedResult(intPtr(1)), contract)
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	if completion.TechStatus != engine.StatusFailed {
		t.Errorf("TechStatus = %q, want failed", completion.TechStatus)
	}
	if completion.Outcome != "" {
		t.Errorf("Outcome = %q; a technical failure has no domain answer to route", completion.Outcome)
	}
}

// TestNullExitNeverBecomesZero guards the fabrication that would be easiest
// to write and hardest to notice.
func TestNullExitNeverBecomesZero(t *testing.T) {
	completion, err := runners.BuildCompletion(lambdaShapedResult(nil), testContract())
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	if completion.TechStatus != engine.StatusFailed {
		t.Errorf("TechStatus = %q, want failed when there is no honest exit code", completion.TechStatus)
	}
	if completion.Outcome != "" {
		t.Errorf("Outcome = %q, want none", completion.Outcome)
	}
}

func TestBuildCompletionMapsStates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state runners.State
		err   *runners.ResultError
		want  engine.TechStatus
	}{
		{"timed out", runners.StateTimedOut, &runners.ResultError{Kind: runners.ErrorTimeout}, engine.StatusTimedOut},
		{"cancelled", runners.StateCancelled, nil, engine.StatusCancelled},
		{"failed", runners.StateFailed, &runners.ResultError{Kind: runners.ErrorExecutionFailure}, engine.StatusFailed},
		{"rejected by policy", runners.StateRejected, &runners.ResultError{Kind: runners.ErrorAuthOrPolicy}, engine.StatusPolicyDenied},
		{"rejected input", runners.StateRejected, &runners.ResultError{Kind: runners.ErrorRejectedInput}, engine.StatusContractRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := lambdaShapedResult(nil)
			res.State = tc.state
			res.Exit = nil
			res.Error = tc.err

			completion, err := runners.BuildCompletion(res, testContract())
			if err != nil {
				t.Fatalf("BuildCompletion: %v", err)
			}
			if completion.TechStatus != tc.want {
				t.Errorf("TechStatus = %q, want %q", completion.TechStatus, tc.want)
			}
		})
	}
}

func TestRejectedResultCarriesRunnerMessageAsDiagnostic(t *testing.T) {
	res := lambdaShapedResult(nil)
	res.State = runners.StateRejected
	res.Exit = nil
	res.Error = &runners.ResultError{
		Kind:    runners.ErrorRejectedInput,
		Message: "runner rejected X",
	}

	completion, err := runners.BuildCompletion(res, testContract())
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	var output struct {
		Error struct {
			Class  string `json:"class"`
			Detail string `json:"detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal(completion.Output, &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output.Error.Class != "runner" || output.Error.Detail != "runner rejected X" {
		t.Fatalf("output error = %+v, want runner / runner rejected X", output.Error)
	}
}

// TestLedgerDeltaPassesAuthorityWithItsManifest is the property the seam
// exists to guarantee: what BuildCompletion writes is exactly what the
// manifest it returns declares, so the ledger's §10.4 producer matrix admits
// it without anything being widened by hand.
func TestLedgerDeltaPassesAuthorityWithItsManifest(t *testing.T) {
	completion, err := runners.BuildCompletion(lambdaShapedResult(intPtr(0)), testContract())
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	if len(completion.LedgerDelta) != 1 {
		t.Fatalf("delta has %d records, want exactly one evidence record", len(completion.LedgerDelta))
	}
	record := completion.LedgerDelta[0]

	if record.RecordType != ledger.RecordEvidence {
		t.Errorf("record type = %q, want evidence", record.RecordType)
	}
	if record.Origin.Kind != ledger.OriginRunner || record.Authority != ledger.AuthorityObserved {
		t.Errorf("origin/authority = %s/%s, want runner/observed", record.Origin.Kind, record.Authority)
	}
	if completion.RunnerManifest == nil {
		t.Fatal("BuildCompletion returned a runner-origin delta with no manifest")
	}
	if err := ledger.CheckAuthority(record, completion.RunnerManifest); err != nil {
		t.Fatalf("the delta does not pass its own manifest: %v", err)
	}
}

// TestUnmeasuredFactsStayOutOfTheEvidence is the honesty test. Every field
// the observations mark unmeasured must be absent from the payload, and the
// scope must say so rather than leave the absence to be inferred.
func TestUnmeasuredFactsStayOutOfTheEvidence(t *testing.T) {
	completion, err := runners.BuildCompletion(lambdaShapedResult(intPtr(0)), testContract())
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	data, err := completion.LedgerDelta[0].DataMap()
	if err != nil {
		t.Fatalf("DataMap: %v", err)
	}

	measurements, _ := data["measurements"].(map[string]any)
	if _, present := measurements["exit_code"]; present {
		t.Error("the evidence claims an exit code Lambda never measured")
	}
	for _, absent := range []string{"changed_paths", "snapshot_digest", "artifact_refs"} {
		if _, present := data[absent]; present {
			t.Errorf("the evidence carries %q, which no observation measured", absent)
		}
	}

	if got := data["completeness"]; got != "partial" {
		t.Errorf("completeness = %v, want partial when some observations are unmeasured", got)
	}
	scope, _ := data["covered_scope"].(string)
	for _, name := range []string{"changed_paths", "exit_status", "workspace_snapshot"} {
		if !strings.Contains(scope, name) {
			t.Errorf("covered_scope does not name the unmeasured observation %q: %q", name, scope)
		}
	}

	for _, measured := range []string{"platform_request_id", "billed_duration_ms", "max_memory_mib", "duration_ms"} {
		if _, present := measurements[measured]; !present {
			t.Errorf("the evidence omits %q, which the observations say was measured", measured)
		}
	}
}

// TestManifestRefusesAFabricatedField proves the manifest is a real
// constraint rather than a rubber stamp: adding a field the builder did not
// write makes the ledger refuse the record.
func TestManifestRefusesAFabricatedField(t *testing.T) {
	completion, err := runners.BuildCompletion(lambdaShapedResult(intPtr(0)), testContract())
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	record := completion.LedgerDelta[0]

	data, err := record.DataMap()
	if err != nil {
		t.Fatalf("DataMap: %v", err)
	}
	measurements, _ := data["measurements"].(map[string]any)
	measurements["exit_code"] = 0 // the fabrication
	tampered, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	record.Data = tampered

	err = ledger.CheckAuthority(record, completion.RunnerManifest)
	if err == nil {
		t.Fatal("the ledger admitted a measurement the manifest never declared")
	}
	var authErr *ledger.AuthorityError
	if !errors.As(err, &authErr) {
		t.Fatalf("error is not an *AuthorityError: %v", err)
	}
	if authErr.Rule != ledger.RuleRunnerFieldNotDeclared {
		t.Errorf("rule = %q, want %q", authErr.Rule, ledger.RuleRunnerFieldNotDeclared)
	}
	if authErr.Field != "/measurements/exit_code" {
		t.Errorf("refusal names field %q, want /measurements/exit_code", authErr.Field)
	}
}

// TestEvidenceRecordValidatesAgainstTheLedgerSchema keeps the seam's output
// admissible to the store, not merely to the authority matrix.
func TestEvidenceRecordValidatesAgainstTheLedgerSchema(t *testing.T) {
	completion, err := runners.BuildCompletion(lambdaShapedResult(intPtr(0)), testContract())
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	record := completion.LedgerDelta[0]
	record.ID = "ledger_01JAV3QK2M0000000000000002"
	record.SchemaVersion = ledger.SchemaVersion
	record.CreatedAt = minimalResult().Timing.FinishedAt
	digest, err := record.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	record.ContentDigest = digest

	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := newValidator(t).ValidateJSON(contracts.SchemaLedgerEvidence, encoded); err != nil {
		t.Fatalf("evidence record does not validate: %v\n%s", err, encoded)
	}
}

// TestNoActorMeansNoLedgerDelta: an unattributed observation is worse than
// none, so the seam declines to emit one.
func TestNoActorMeansNoLedgerDelta(t *testing.T) {
	contract := testContract()
	contract.ActorID = ""

	completion, err := runners.BuildCompletion(lambdaShapedResult(intPtr(0)), contract)
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	if len(completion.LedgerDelta) != 0 || completion.RunnerManifest != nil {
		t.Error("an anonymous runner produced evidence; observed authority requires a named producer")
	}
}

func TestBuildCompletionRequiresASuccessOutcome(t *testing.T) {
	contract := testContract()
	contract.SuccessOutcome = ""
	if _, err := runners.BuildCompletion(lambdaShapedResult(intPtr(0)), contract); err == nil {
		t.Fatal("BuildCompletion accepted a node with no success outcome")
	}
}

// TestCompletenessIsUnknownWhenNothingWasMeasured proves the third value of
// the completeness field is reachable, not decorative.
func TestCompletenessIsUnknownWhenNothingWasMeasured(t *testing.T) {
	res := lambdaShapedResult(intPtr(0))
	res.Observations = runners.Observations{}

	completion, err := runners.BuildCompletion(res, testContract())
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	data, err := completion.LedgerDelta[0].DataMap()
	if err != nil {
		t.Fatalf("DataMap: %v", err)
	}
	if got := data["completeness"]; got != "unknown" {
		t.Errorf("completeness = %v, want unknown", got)
	}
}

// workspaceSnapshotShapedResult is a result shaped like a runner that CAN
// directly compare the workspace (task t12, spec claim c15) — headspace-cli
// 0.11.0 and the Lambda adapter cannot (see their own package docs), which
// is exactly why this seam has to be proven with a result that measures it,
// not with either adapter's own shape.
func workspaceSnapshotShapedResult() runners.Result {
	res := minimalResult()
	res.Changes = runners.Changes{
		Complete:        true,
		Paths:           []string{"internal/worker/hooks.go", "internal/runners/dispatch.go"},
		SnapshotDigest:  "sha256:" + strings.Repeat("c", 64),
		DiffArtifactRef: "artifact://diff/att_01JAV3QK2M0000000000000010",
	}
	res.Artifacts = &runners.Artifacts{
		StdoutRef: "artifact://logs/stdout",
	}
	res.Observations = runners.Observations{
		ExitStatus:    runners.Observation{Measured: true, Complete: true, Method: "wait4"},
		ChangedPaths:  runners.Observation{Measured: true, Complete: true, Method: "workspace_snapshot_diff"},
		Logs:          runners.Observation{Measured: true, Complete: true},
		ResourceUsage: runners.Observation{Measured: false, Complete: false},
	}
	return res
}

// TestBuildCompletionSurfacesWorkspaceSnapshotEvidenceWhenMeasured is the
// standard post_run workspace-snapshot pattern's evidence half (task t12,
// spec claim c15, honesty condition h10): when a runner's changed_paths
// observation says it directly measured the workspace comparison, the
// changed files, the snapshot digest, and every artifact ref — including the
// diff artifact ref — land in the same observed evidence record every other
// runner-measured fact does, admissible against its own manifest.
func TestBuildCompletionSurfacesWorkspaceSnapshotEvidenceWhenMeasured(t *testing.T) {
	completion, err := runners.BuildCompletion(workspaceSnapshotShapedResult(), testContract())
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	data, err := completion.LedgerDelta[0].DataMap()
	if err != nil {
		t.Fatalf("DataMap: %v", err)
	}

	paths, ok := data["changed_paths"].([]any)
	if !ok || len(paths) != 2 {
		t.Fatalf("changed_paths = %v, want the two measured paths", data["changed_paths"])
	}
	wantDigest := "sha256:" + strings.Repeat("c", 64)
	if got := data["snapshot_digest"]; got != wantDigest {
		t.Errorf("snapshot_digest = %v, want %q", got, wantDigest)
	}

	refs, ok := data["artifact_refs"].([]any)
	if !ok {
		t.Fatalf("artifact_refs missing or not an array: %v", data["artifact_refs"])
	}
	wantRefs := map[string]bool{
		"artifact://logs/stdout":                         true,
		"artifact://diff/att_01JAV3QK2M0000000000000010": true,
	}
	if len(refs) != len(wantRefs) {
		t.Fatalf("artifact_refs = %v, want exactly %v", refs, wantRefs)
	}
	for _, r := range refs {
		if !wantRefs[r.(string)] {
			t.Errorf("artifact_refs contains unexpected ref %v", r)
		}
	}

	if err := ledger.CheckAuthority(completion.LedgerDelta[0], completion.RunnerManifest); err != nil {
		t.Fatalf("the delta does not pass its own manifest: %v", err)
	}

	scope, _ := data["covered_scope"].(string)
	if strings.Contains(scope, "changed_paths") {
		t.Errorf("covered_scope still names changed_paths as unmeasured: %q", scope)
	}
}

// TestDiffArtifactRefStaysOutWhenTheWorkspaceComparisonIsUnmeasured proves
// the diff artifact ref does not leak into artifact_refs merely because some
// OTHER artifact is present — it rides the same changed_paths measured gate
// as changed_paths and snapshot_digest themselves, never a looser one.
func TestDiffArtifactRefStaysOutWhenTheWorkspaceComparisonIsUnmeasured(t *testing.T) {
	res := workspaceSnapshotShapedResult()
	res.Observations.ChangedPaths = runners.Observation{Measured: false, Complete: false}

	completion, err := runners.BuildCompletion(res, testContract())
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	data, err := completion.LedgerDelta[0].DataMap()
	if err != nil {
		t.Fatalf("DataMap: %v", err)
	}
	if _, present := data["changed_paths"]; present {
		t.Error("changed_paths present despite an unmeasured workspace comparison")
	}
	if _, present := data["snapshot_digest"]; present {
		t.Error("snapshot_digest present despite an unmeasured workspace comparison")
	}
	refs, _ := data["artifact_refs"].([]any)
	for _, r := range refs {
		if r.(string) == res.Changes.DiffArtifactRef {
			t.Errorf("artifact_refs leaked the diff artifact ref despite an unmeasured comparison: %v", refs)
		}
	}
	if len(refs) != 1 || refs[0].(string) != "artifact://logs/stdout" {
		t.Errorf("artifact_refs = %v, want only the plain stdout ref", refs)
	}
}
