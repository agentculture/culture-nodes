package lambda_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	awscreds "github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/runners"
	runnerlambda "github.com/agentculture/culture-nodes/internal/runners/lambda"
	"github.com/agentculture/culture-nodes/schemas"
)

const (
	testARN       = "arn:aws:lambda:us-east-1:123456789012:function:nodes-run-tests"
	testKey       = "deliver-change/run-tests"
	testImage     = "sha256:0604fdb7edd7a8eacbcbdebdf7aad00db03650efd7f927f4b16f6cd0e0c3747e"
	testImageURI  = "123456789012.dkr.ecr.us-east-1.amazonaws.com/nodes-runner@" + testImage
	testOperation = "op_01JAV3QK2M0000000000000011"
)

// fixedClock makes the adapter's own wall-clock measurement deterministic, so
// a test can tell the platform's duration apart from the round-trip figure.
func fixedClock(start time.Time, step time.Duration) func() time.Time {
	calls := 0
	return func() time.Time {
		now := start.Add(time.Duration(calls) * step)
		calls++
		return now
	}
}

var clockStart = time.Date(2026, 8, 8, 15, 0, 0, 0, time.UTC)

func healthyFunction() functionDescription {
	return functionDescription{
		ResolvedImageURI: testImageURI,
		TimeoutSeconds:   900,
		MemoryMiB:        2048,
		EphemeralMiB:     2048,
		SubnetIDs:        []string{"subnet-0a1b2c3d"},
	}
}

func registryWith(t *testing.T, identity runners.FunctionIdentity) *runners.FunctionRegistry {
	t.Helper()
	registry := runners.NewFunctionRegistry()
	if err := registry.RegisterFunction(testKey, identity); err != nil {
		t.Fatalf("RegisterFunction: %v", err)
	}
	return registry
}

// newAdapter wires an adapter to the fake with a registry holding testKey.
func newAdapter(t *testing.T, fake *fakeLambda, mutate ...func(*runnerlambda.Config)) *runnerlambda.Adapter {
	t.Helper()
	cfg := runnerlambda.Config{
		Registry:    registryWith(t, runners.FunctionIdentity{ARN: testARN, ImageDigest: testImage}),
		Region:      "us-east-1",
		Endpoint:    fake.server.URL,
		Credentials: awscreds.NewStaticCredentialsProvider("fake-access-key", "fake-secret-key", ""),
		HTTPClient:  fake.server.Client(),
		MaxAttempts: 1,
		Clock:       fixedClock(clockStart, 2*time.Second),
	}
	for _, m := range mutate {
		m(&cfg)
	}
	adapter, err := runnerlambda.New(t.Context(), cfg)
	if err != nil {
		t.Fatalf("lambda.New: %v", err)
	}
	return adapter
}

// operation builds a schema-valid operation aimed at testKey.
func operation(mutate ...func(*runners.Operation)) runners.Operation {
	op := runners.Operation{
		OperationID:    testOperation,
		Runner:         runnerlambda.RunnerName,
		RunnerRevision: runnerlambda.DefaultRunnerRevision,
		Workspace: &runners.Workspace{
			SourceRef:    "artifact://workspace/input",
			SourceDigest: "sha256:104c92af170b72888c5be5b64aa08aff686010ae232cd101b8c1c61f7eff0636",
			WriteMode:    runners.WriteModeCopyOnWrite,
		},
		Execution: runners.Execution{
			Kind:        runners.ExecutionFunction,
			ImageRef:    testKey,
			ImageDigest: testImage,
		},
		Command: runners.Command{Argv: []string{"python", "-m", "pytest", "-q"}, EnvironmentRefs: []string{}},
		Policy: runners.Policy{
			TimeoutSeconds:     600,
			Network:            runners.NetworkNone,
			AllowedOutputPaths: []string{},
		},
		Evidence: runners.EvidenceRequest{
			SnapshotBefore: true,
			SnapshotAfter:  true,
			CaptureExit:    true,
			CaptureLogs:    true,
		},
	}
	for _, m := range mutate {
		m(&op)
	}
	return op
}

func validateResult(t *testing.T, res runners.Result) {
	t.Helper()
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatalf("contracts.NewValidator: %v", err)
	}
	if err := validator.ValidateJSON(contracts.SchemaRunnerResult, encoded); err != nil {
		t.Fatalf("result does not validate against %s: %v\n%s", contracts.SchemaRunnerResult, err, encoded)
	}
	// Belt and braces: the schema lives in the embedded FS this validator
	// reads, so a missing file would silently pass everything.
	if _, err := schemas.FS.ReadFile("runner/result.schema.json"); err != nil {
		t.Fatalf("runner result schema is not embedded: %v", err)
	}
}

// --- registry refusal (spec claim c41 / honesty condition h36) -------------

// TestUnregisteredIdentityIsRefusedWithoutAnyAWSCall is the load-bearing
// security test. The refusal must happen locally: not "AWS said no", not "IAM
// denied it", but "this process declined to ask".
func TestUnregisteredIdentityIsRefusedWithoutAnyAWSCall(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	adapter := newAdapter(t, fake)

	getBefore, invokeBefore := fake.counts()

	_, err := adapter.Execute(t.Context(), operation(func(op *runners.Operation) {
		op.Execution.ImageRef = "deliver-change/not-registered"
	}))
	if err == nil {
		t.Fatal("Execute dispatched to an unregistered identity")
	}
	if !errors.Is(err, runners.ErrUnregisteredFunction) {
		t.Errorf("error does not match ErrUnregisteredFunction: %v", err)
	}

	var dispatchErr *runners.DispatchError
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("error is not a *DispatchError: %v", err)
	}
	if dispatchErr.OperationID != testOperation {
		t.Errorf("refusal does not name the operation: %+v", dispatchErr)
	}
	if dispatchErr.Retryable() {
		t.Error("an unregistered identity is not a retryable condition")
	}

	getAfter, invokeAfter := fake.counts()
	if getAfter != getBefore || invokeAfter != invokeBefore {
		t.Errorf("the refusal cost AWS calls: GetFunction %d→%d, Invoke %d→%d",
			getBefore, getAfter, invokeBefore, invokeAfter)
	}
}

// TestDigestMismatchIsRefusedWithoutInvoking proves the pin is checked, not
// merely recorded.
func TestDigestMismatchIsRefusedWithoutInvoking(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	adapter := newAdapter(t, fake)
	_, invokeBefore := fake.counts()

	_, err := adapter.Execute(t.Context(), operation(func(op *runners.Operation) {
		op.Execution.ImageDigest = "sha256:" + strings.Repeat("b", 64)
	}))
	if !errors.Is(err, runners.ErrDigestMismatch) {
		t.Fatalf("error = %v, want ErrDigestMismatch", err)
	}
	if _, invokeAfter := fake.counts(); invokeAfter != invokeBefore {
		t.Error("a digest mismatch reached the network")
	}
}

// TestLoadRefusesADeployedImageThatIsNotThePinnedOne closes the other half of
// pinning: the registry's digest is checked against the deployment at load
// time, so a drifted function fails startup rather than running unnoticed.
func TestLoadRefusesADeployedImageThatIsNotThePinnedOne(t *testing.T) {
	fake := newFakeLambda(t)
	deployed := healthyFunction()
	deployed.ResolvedImageURI = "123456789012.dkr.ecr.us-east-1.amazonaws.com/nodes-runner@sha256:" + strings.Repeat("c", 64)
	fake.describe(testARN, deployed)

	_, err := runnerlambda.New(t.Context(), runnerlambda.Config{
		Registry:    registryWith(t, runners.FunctionIdentity{ARN: testARN, ImageDigest: testImage}),
		Region:      "us-east-1",
		Endpoint:    fake.server.URL,
		Credentials: awscreds.NewStaticCredentialsProvider("k", "s", ""),
		HTTPClient:  fake.server.Client(),
		MaxAttempts: 1,
	})
	if !errors.Is(err, runners.ErrDigestMismatch) {
		t.Fatalf("New accepted a drifted deployment: %v", err)
	}
}

func TestNewRefusesAZipPackageFunction(t *testing.T) {
	fake := newFakeLambda(t)
	zipped := healthyFunction()
	zipped.PackageType = "Zip"
	fake.describe(testARN, zipped)

	_, err := runnerlambda.New(t.Context(), runnerlambda.Config{
		Registry:    registryWith(t, runners.FunctionIdentity{ARN: testARN, ImageDigest: testImage}),
		Region:      "us-east-1",
		Endpoint:    fake.server.URL,
		Credentials: awscreds.NewStaticCredentialsProvider("k", "s", ""),
		HTTPClient:  fake.server.Client(),
		MaxAttempts: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "package type") {
		t.Fatalf("New accepted a zip-package function: %v", err)
	}
}

func TestNewRefusesAnEmptyRegistry(t *testing.T) {
	fake := newFakeLambda(t)
	_, err := runnerlambda.New(t.Context(), runnerlambda.Config{
		Registry: runners.NewFunctionRegistry(),
		Endpoint: fake.server.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "at least one registered function") {
		t.Fatalf("New accepted an empty registry: %v", err)
	}
}

// --- evidence honesty (spec claim c25 / honesty condition h22) -------------

// TestSuccessfulInvokeProducesHonestEvidence is the evidence-mapping test:
// the observed fields match the platform's own figures, every unobservable
// field is {measured:false, complete:false}, and the whole document validates
// against schemas/runner/result.schema.json.
func TestSuccessfulInvokeProducesHonestEvidence(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	fake.setInvoke(invokeResponse{
		Payload: `{"exit_code":0,"signal":null,"artifacts":{` +
			`"stdout_ref":"artifact://logs/stdout","output_workspace_ref":"artifact://workspace/output","junit_ref":"artifact://reports/junit"}}`,
		LogTail: reportLine(defaultRequestID, 1234.56, 1235, 2048, 128),
	})
	adapter := newAdapter(t, fake)

	res, err := adapter.Execute(t.Context(), operation())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	validateResult(t, res)

	// --- what Lambda observed ---
	if res.State != runners.StateCompleted {
		t.Errorf("State = %q, want completed", res.State)
	}
	if code, ok := res.ExitCode(); !ok || code != 0 {
		t.Errorf("exit code = %v/%v, want 0", code, ok)
	}
	if res.Environment.PlatformRequestID != defaultRequestID {
		t.Errorf("platform request id = %q, want the fake's %q", res.Environment.PlatformRequestID, defaultRequestID)
	}
	if res.Environment.ImageDigest != testImage {
		t.Errorf("image digest = %q, want the registered pin", res.Environment.ImageDigest)
	}
	if res.Environment.RunnerRevision != runnerlambda.DefaultRunnerRevision {
		t.Errorf("runner revision = %q", res.Environment.RunnerRevision)
	}
	if res.Environment.InputDigest == nil || *res.Environment.InputDigest != operation().Workspace.SourceDigest {
		t.Errorf("input digest = %v, want the operation's workspace digest", res.Environment.InputDigest)
	}
	if res.Environment.MemoryMiB == nil || *res.Environment.MemoryMiB != 2048 {
		t.Errorf("configured memory = %v, want 2048", res.Environment.MemoryMiB)
	}
	if res.Timing.DurationMs != 1234 {
		t.Errorf("duration = %dms, want Lambda's REPORT figure (1234), not the adapter's round trip", res.Timing.DurationMs)
	}
	if res.Timing.BilledDurationMs == nil || *res.Timing.BilledDurationMs != 1235 {
		t.Errorf("billed duration = %v, want 1235", res.Timing.BilledDurationMs)
	}
	if res.ResourceUsage == nil || res.ResourceUsage.MaxMemoryMiB == nil || *res.ResourceUsage.MaxMemoryMiB != 128 {
		t.Errorf("max memory = %v, want 128", res.ResourceUsage)
	}

	// --- what Lambda did not observe ---
	assertUnmeasured(t, res, runners.ObsExitStatus)
	assertUnmeasured(t, res, runners.ObsChangedPaths)
	assertUnmeasured(t, res, runnerlambda.ObsWorkspaceSnapshot)

	if res.Changes.Complete {
		t.Error("changes.complete is true; Lambda cannot compare a workspace (spec claim c25)")
	}
	if len(res.Changes.Paths) != 0 {
		t.Errorf("changes.paths = %v; an unobserved workspace yields no path list", res.Changes.Paths)
	}

	// --- what is measured but partial ---
	for _, name := range []string{runners.ObsLogs, runners.ObsResourceUsage} {
		obs, _ := res.Observations.Get(name)
		if !obs.Measured {
			t.Errorf("observation %q should be measured", name)
		}
		if obs.Complete {
			t.Errorf("observation %q claims completeness; Lambda's log tail and REPORT figures are bounded", name)
		}
	}
	for _, name := range []string{runnerlambda.ObsHandlerCompletion, runnerlambda.ObsPlatformRequestID, runnerlambda.ObsImageDigest, runnerlambda.ObsDuration} {
		obs, ok := res.Observations.Get(name)
		if !ok {
			t.Fatalf("observation %q missing", name)
		}
		if !obs.Measured || !obs.Complete {
			t.Errorf("observation %q = %+v, want measured and complete", name, obs)
		}
	}

	// Artifact refs are function-reported, carried but never observed.
	if res.Artifacts == nil || res.Artifacts.StdoutRef != "artifact://logs/stdout" {
		t.Errorf("artifacts = %+v, want the function's reported refs", res.Artifacts)
	}
	if res.Artifacts.Additional["junit_ref"] != "artifact://reports/junit" {
		t.Errorf("a runner-specific artifact ref was dropped: %+v", res.Artifacts.Additional)
	}
}

// TestPayloadIsTheTypedOperation proves the request body is the operation
// document itself, not an adapter-invented envelope.
func TestPayloadIsTheTypedOperation(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	fake.setInvoke(invokeResponse{Payload: `{"exit_code":0}`})
	adapter := newAdapter(t, fake)

	if _, err := adapter.Execute(t.Context(), operation()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	sent := fake.payload(t)
	if sent["operation_id"] != testOperation {
		t.Errorf("payload operation_id = %v", sent["operation_id"])
	}
	if sent["runner"] != runnerlambda.RunnerName {
		t.Errorf("payload runner = %v", sent["runner"])
	}
	encoded, err := json.Marshal(sent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatalf("contracts.NewValidator: %v", err)
	}
	if err := validator.ValidateJSON(contracts.SchemaRunnerOperation, encoded); err != nil {
		t.Errorf("the payload the function receives is not a valid operation: %v", err)
	}

	if names := fake.invokedNames; len(names) != 1 || names[0] != testARN {
		t.Errorf("invoked %v, want the registered ARN %q", names, testARN)
	}
}

// TestWithoutLogCaptureNothingIsMeasuredAboutResources: Lambda states billed
// duration and peak memory only in the REPORT line. No log tail, no
// measurement — and specifically not an inference from the configured memory
// size, which is a deployment setting rather than a reading.
func TestWithoutLogCaptureNothingIsMeasuredAboutResources(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	fake.setInvoke(invokeResponse{
		Payload: `{"exit_code":0}`,
		LogTail: reportLine(defaultRequestID, 1234.56, 1235, 2048, 128),
	})
	adapter := newAdapter(t, fake)

	res, err := adapter.Execute(t.Context(), operation(func(op *runners.Operation) {
		op.Evidence.CaptureLogs = false
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	validateResult(t, res)

	assertUnmeasured(t, res, runners.ObsLogs)
	assertUnmeasured(t, res, runners.ObsResourceUsage)
	assertUnmeasured(t, res, runnerlambda.ObsDuration)

	if res.Timing.BilledDurationMs != nil {
		t.Errorf("billed duration = %v; nothing measured it", res.Timing.BilledDurationMs)
	}
	if res.ResourceUsage != nil {
		t.Errorf("resource usage = %+v; nothing measured it", res.ResourceUsage)
	}
	// The duration that remains is the adapter's own round trip, and the
	// observation says so.
	if res.Timing.DurationMs != 2000 {
		t.Errorf("duration = %dms, want the adapter's 2s round trip", res.Timing.DurationMs)
	}
	obs, _ := res.Observations.Get(runnerlambda.ObsDuration)
	if !strings.Contains(obs.Note, "round trip") {
		t.Errorf("the duration observation does not disclose that it is a round-trip figure: %+v", obs)
	}
}

// TestRedeployBetweenLoadAndInvokeDowngradesTheDigestObservation: the digest
// stays in the result because the registry pins it, but it stops being an
// observation about this run.
func TestRedeployBetweenLoadAndInvokeDowngradesTheDigestObservation(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	fake.setInvoke(invokeResponse{Payload: `{"exit_code":0}`, ExecutedVersion: "9"})
	adapter := newAdapter(t, fake)

	res, err := adapter.Execute(t.Context(), operation())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	validateResult(t, res)

	assertUnmeasured(t, res, runnerlambda.ObsImageDigest)
	obs, _ := res.Observations.Get(runnerlambda.ObsImageDigest)
	if !strings.Contains(obs.Note, "redeployed") {
		t.Errorf("the downgrade does not explain itself: %+v", obs)
	}
	if res.Environment.ImageDigest != testImage {
		t.Errorf("the registry's pin was dropped from the result: %q", res.Environment.ImageDigest)
	}
}

func TestMissingRequestIDIsNotInvented(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	adapter := newAdapter(t, fake)
	// setInvoke fills in a default request id, so clear it afterwards: the
	// point of this test is a response that genuinely carries none.
	fake.setInvoke(invokeResponse{Payload: `{"exit_code":0}`})
	fake.clearRequestID()

	res, err := adapter.Execute(t.Context(), operation())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	validateResult(t, res)
	if res.Environment.PlatformRequestID != "" {
		t.Errorf("platform request id = %q, want empty when the response carried none", res.Environment.PlatformRequestID)
	}
	assertUnmeasured(t, res, runnerlambda.ObsPlatformRequestID)
}

// --- failure shapes --------------------------------------------------------

func TestHandlerErrorIsAResultNotADispatchError(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	fake.setInvoke(invokeResponse{
		FunctionError: "Unhandled",
		Payload:       `{"errorType":"RuntimeError","errorMessage":"the runner image crashed"}`,
		LogTail:       reportLine(defaultRequestID, 42.5, 43, 2048, 96),
	})
	adapter := newAdapter(t, fake)

	res, err := adapter.Execute(t.Context(), operation())
	if err != nil {
		t.Fatalf("a handler that raised is a result, not a dispatch failure: %v", err)
	}
	validateResult(t, res)

	if res.State != runners.StateFailed {
		t.Errorf("State = %q, want failed", res.State)
	}
	if res.Error == nil || res.Error.Kind != runners.ErrorExecutionFailure {
		t.Errorf("Error = %+v, want an execution failure", res.Error)
	}
	if res.Error.Retryable {
		t.Error("a crashed function image is not automatically retryable")
	}
	if !strings.Contains(res.Error.Message, "the runner image crashed") {
		t.Errorf("the error drops the platform's message: %q", res.Error.Message)
	}
	// The handler-completion observation is still measured: Lambda genuinely
	// told us the handler raised.
	obs, _ := res.Observations.Get(runnerlambda.ObsHandlerCompletion)
	if !obs.Measured {
		t.Error("handler completion should be measured even on a failure")
	}
}

func TestFunctionTimeoutBecomesATimedOutResult(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	fake.setInvoke(invokeResponse{
		FunctionError: "Unhandled",
		Payload:       `{"errorType":"Sandbox.Timedout","errorMessage":"2026-08-08T15:00:00Z Task timed out after 600.00 seconds"}`,
	})
	adapter := newAdapter(t, fake)

	res, err := adapter.Execute(t.Context(), operation())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	validateResult(t, res)

	if res.State != runners.StateTimedOut {
		t.Errorf("State = %q, want timed_out", res.State)
	}
	if res.Error == nil || res.Error.Kind != runners.ErrorTimeout {
		t.Errorf("Error = %+v, want a timeout", res.Error)
	}
	if res.Exit != nil {
		t.Errorf("exit = %+v; a timed-out invocation reported no exit", res.Exit)
	}
}

func TestFunctionThatReportsNoExitCodeIsAContractFailure(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	fake.setInvoke(invokeResponse{Payload: `{"message":"done!"}`})
	adapter := newAdapter(t, fake)

	res, err := adapter.Execute(t.Context(), operation())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	validateResult(t, res)

	if res.State != runners.StateFailed || res.Error == nil || res.Error.Kind != runners.ErrorContractFailure {
		t.Fatalf("State/Error = %q/%+v, want failed/contract_failure", res.State, res.Error)
	}
	if res.Exit != nil {
		t.Errorf("exit = %+v; the adapter must not substitute a zero for a value the function declined to state", res.Exit)
	}
}

func TestThrottleIsClassifiedRetryable(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	fake.setInvoke(invokeResponse{
		httpStatus: 429,
		errorType:  "TooManyRequestsException",
		errorMsg:   "Rate exceeded",
	})
	adapter := newAdapter(t, fake)

	_, err := adapter.Execute(t.Context(), operation())
	if err == nil {
		t.Fatal("a throttled invoke returned no error")
	}

	var dispatchErr *runners.DispatchError
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("error is not a *DispatchError: %v", err)
	}
	if dispatchErr.Kind != runners.ErrorRateLimited {
		t.Errorf("Kind = %q, want rate_limited", dispatchErr.Kind)
	}
	if !dispatchErr.Retryable() {
		t.Error("a throttle is retryable")
	}
}

func TestAccessDeniedIsNotRetryable(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	fake.setInvoke(invokeResponse{httpStatus: 403, errorType: "AccessDeniedException", errorMsg: "not authorized"})
	adapter := newAdapter(t, fake)

	_, err := adapter.Execute(t.Context(), operation())
	var dispatchErr *runners.DispatchError
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("error is not a *DispatchError: %v", err)
	}
	if dispatchErr.Kind != runners.ErrorAuthOrPolicy {
		t.Errorf("Kind = %q, want auth_or_policy", dispatchErr.Kind)
	}
	if dispatchErr.Retryable() {
		t.Error("an IAM denial is not fixed by trying again")
	}
	if !strings.Contains(dispatchErr.Detail, "ADR 0003") {
		t.Errorf("the denial does not point at the policy that governs it: %q", dispatchErr.Detail)
	}

	// "the platform said no" and "this process declined to ask" are different
	// facts. Conflating their sentinels would hide which of the two
	// boundaries actually held, which is the whole point of having both.
	if !errors.Is(err, runners.ErrAccessDenied) {
		t.Errorf("an IAM denial does not match ErrAccessDenied: %v", err)
	}
	if errors.Is(err, runners.ErrUnregisteredFunction) {
		t.Error("an IAM denial matched ErrUnregisteredFunction; the registry admitted this identity, AWS refused it")
	}
}

func TestServiceFaultIsRetryable(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	fake.setInvoke(invokeResponse{httpStatus: 500, errorType: "ServiceException", errorMsg: "internal"})
	adapter := newAdapter(t, fake)

	_, err := adapter.Execute(t.Context(), operation())
	var dispatchErr *runners.DispatchError
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("error is not a *DispatchError: %v", err)
	}
	if !dispatchErr.Retryable() {
		t.Errorf("a service fault should be retryable, got %+v", dispatchErr)
	}
}

// --- refusals --------------------------------------------------------------

// TestOversizePayloadIsRefusedWithARemediation: the 6 MB synchronous limit is
// a validation-time rule, and the refusal must tell the caller what to do
// instead rather than merely that it failed.
func TestOversizePayloadIsRefusedWithARemediation(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	adapter := newAdapter(t, fake)
	_, invokeBefore := fake.counts()

	// environment_refs is the only unbounded string list in the document.
	huge := make([]string, 0, 4096)
	for len(strings.Join(huge, "")) < runnerlambda.MaxSyncPayloadBytes {
		huge = append(huge, strings.Repeat("x", 4096))
	}

	_, err := adapter.Execute(t.Context(), operation(func(op *runners.Operation) {
		op.Command.EnvironmentRefs = huge
	}))
	if !errors.Is(err, runners.ErrOversizePayload) {
		t.Fatalf("error = %v, want ErrOversizePayload", err)
	}
	var dispatchErr *runners.DispatchError
	if errors.As(err, &dispatchErr) && !strings.Contains(dispatchErr.Detail, "S3 artifact refs") {
		t.Errorf("the refusal gives no remedy: %q", dispatchErr.Detail)
	}
	if _, invokeAfter := fake.counts(); invokeAfter != invokeBefore {
		t.Error("an oversize payload was sent anyway")
	}
}

// TestTimeoutDefenceInDepth: the compiler already rejects an over-cap
// timeout, and the adapter rejects it again. A limit checked in only one
// place is a limit one refactor away from being unchecked.
func TestTimeoutDefenceInDepth(t *testing.T) {
	fake := newFakeLambda(t)
	shortFunction := healthyFunction()
	shortFunction.TimeoutSeconds = 300
	fake.describe(testARN, shortFunction)
	adapter := newAdapter(t, fake)

	for _, tc := range []struct {
		name    string
		seconds int
		wantIn  string
	}{
		{"above the platform cap", 901, "Lambda's maximum duration is 900s"},
		{"above the function's configured timeout", 600, "configured to time out at 300s"},
		{"absent", 0, "declares no timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, invokeBefore := fake.counts()
			_, err := adapter.Execute(t.Context(), operation(func(op *runners.Operation) {
				op.Policy.TimeoutSeconds = tc.seconds
			}))
			if !errors.Is(err, runners.ErrTimeoutNotEnforceable) {
				t.Fatalf("error = %v, want ErrTimeoutNotEnforceable", err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("refusal %q does not name the limit (%q)", err, tc.wantIn)
			}
			if _, invokeAfter := fake.counts(); invokeAfter != invokeBefore {
				t.Error("an unenforceable timeout reached the network")
			}
		})
	}
}

// TestUnenforceablePolicyFieldsAreRefused: the operation schema says a field
// a runner cannot enforce must be rejected rather than silently ignored.
func TestUnenforceablePolicyFieldsAreRefused(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	adapter := newAdapter(t, fake)

	cpu := 2.0
	pids := 256
	tightMemory := 512
	shell := true

	for _, tc := range []struct {
		name   string
		mutate func(*runners.Operation)
		wantIn string
	}{
		{"cpu limit", func(op *runners.Operation) { op.Policy.CPU = &cpu }, "derives CPU from configured memory"},
		{"pid limit", func(op *runners.Operation) { op.Policy.PIDs = &pids }, "no per-invocation process limit"},
		{"memory tighter than the deployment", func(op *runners.Operation) { op.Policy.MemoryMiB = &tightMemory }, "redeploy the function"},
		{"output paths", func(op *runners.Operation) { op.Policy.AllowedOutputPaths = []string{"/workspace"} }, "no controlled workspace"},
		{"shell", func(op *runners.Operation) { op.Command.RequiresShell = &shell }, "policy rejects it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, invokeBefore := fake.counts()
			_, err := adapter.Execute(t.Context(), operation(tc.mutate))
			if err == nil {
				t.Fatal("the operation was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("refusal %q does not explain the limit (%q)", err, tc.wantIn)
			}
			if _, invokeAfter := fake.counts(); invokeAfter != invokeBefore {
				t.Error("an unenforceable policy reached the network")
			}
		})
	}
}

// TestScopedNetworkRequiresVPCAttachment records the honest half-measure: the
// adapter verifies attachment, refuses without it, and never claims to have
// observed what the subnets route to.
func TestScopedNetworkRequiresVPCAttachment(t *testing.T) {
	fake := newFakeLambda(t)
	open := healthyFunction()
	open.SubnetIDs = nil
	fake.describe(testARN, open)
	adapter := newAdapter(t, fake)

	_, err := adapter.Execute(t.Context(), operation())
	if err == nil || !strings.Contains(err.Error(), "not VPC-attached") {
		t.Fatalf("a network:none policy on an unattached function was accepted: %v", err)
	}

	// network:full is honest about asking for nothing, so it proceeds.
	fake.setInvoke(invokeResponse{Payload: `{"exit_code":0}`})
	if _, err := adapter.Execute(t.Context(), operation(func(op *runners.Operation) {
		op.Policy.Network = runners.NetworkFull
	})); err != nil {
		t.Fatalf("network:full should dispatch: %v", err)
	}
}

func TestWrongRunnerAndKindAreRefused(t *testing.T) {
	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())
	adapter := newAdapter(t, fake)

	for _, tc := range []struct {
		name   string
		mutate func(*runners.Operation)
		wantIn string
	}{
		{"another runner", func(op *runners.Operation) { op.Runner = "headspace" }, "this adapter is \"lambda\""},
		{"container kind", func(op *runners.Operation) { op.Execution.Kind = runners.ExecutionContainer }, "isolating container runner"},
		{"stale runner revision", func(op *runners.Operation) {
			op.RunnerRevision = contracts.Digest([]byte("an older adapter"))
		}, "revision pin that is not checked is not a pin"},
		{"no argv", func(op *runners.Operation) { op.Command.Argv = nil }, "no command argv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := adapter.Execute(t.Context(), operation(tc.mutate)); err == nil ||
				!strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantIn)
			}
		})
	}
}

// assertUnmeasured is the honesty assertion this package's tests lean on
// hardest: an observation that is not measured must also not be complete, and
// must explain itself.
func assertUnmeasured(t *testing.T, res runners.Result, name string) {
	t.Helper()
	obs, ok := res.Observations.Get(name)
	if !ok {
		t.Fatalf("observation %q is missing entirely; an absent declaration is not an honest one", name)
	}
	if obs.Measured || obs.Complete {
		t.Errorf("observation %q = {measured:%t, complete:%t}, want both false", name, obs.Measured, obs.Complete)
	}
	if obs.Note == "" {
		t.Errorf("observation %q is unmeasured with no explanation", name)
	}
}
