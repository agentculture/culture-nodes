//go:build awslive

// This file is excluded from every default build. It exists so the adapter
// can be exercised against real Lambda by hand today, without pretending a CI
// lane for it exists (plan risk r1: LocalStack's fidelity for digest-pinned
// container-image functions and IAM is unverified, and no dedicated AWS test
// account has been provisioned).
//
//	NODES_TEST_LAMBDA_ARN=arn:aws:lambda:us-east-1:123456789012:function:nodes-runner \
//	NODES_TEST_LAMBDA_IMAGE_DIGEST=sha256:… \
//	go test -tags awslive ./internal/runners/lambda/ -run TestLive -v
//
// It costs real invocations against real credentials from the ambient chain.

package lambda_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/runners"
	runnerlambda "github.com/agentculture/culture-nodes/internal/runners/lambda"
)

// liveConfig reads the environment, skipping rather than failing when the
// lane is not configured — an unconfigured environment is not a test failure.
func liveConfig(t *testing.T) (arn, digest, region string) {
	t.Helper()
	arn = os.Getenv("NODES_TEST_LAMBDA_ARN")
	digest = os.Getenv("NODES_TEST_LAMBDA_IMAGE_DIGEST")
	region = os.Getenv("AWS_REGION")
	if arn == "" || digest == "" {
		t.Skip("set NODES_TEST_LAMBDA_ARN and NODES_TEST_LAMBDA_IMAGE_DIGEST to run the live Lambda test")
	}
	if region == "" {
		region = "us-east-1"
	}
	return arn, digest, region
}

func liveAdapter(t *testing.T) (*runnerlambda.Adapter, string) {
	t.Helper()
	arn, digest, region := liveConfig(t)

	registry := runners.NewFunctionRegistry()
	const key = "live/run-tests"
	if err := registry.RegisterFunction(key, runners.FunctionIdentity{ARN: arn, ImageDigest: digest}); err != nil {
		t.Fatalf("RegisterFunction: %v", err)
	}

	adapter, err := runnerlambda.New(t.Context(), runnerlambda.Config{
		Registry: registry,
		Region:   region,
	})
	if err != nil {
		t.Fatalf("lambda.New against real AWS: %v", err)
	}
	return adapter, key
}

// TestLiveInvokeProducesASchemaValidResult is the end-to-end check the fake
// cannot make: that a real Invoke response carries the request id, executed
// version, and REPORT line this adapter reads, and that the result it builds
// still validates.
func TestLiveInvokeProducesASchemaValidResult(t *testing.T) {
	adapter, key := liveAdapter(t)
	_, digest, _ := liveConfig(t)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	res, err := adapter.Execute(ctx, runners.Operation{
		OperationID:    "op_live_" + time.Now().UTC().Format("20060102T150405Z"),
		Runner:         runnerlambda.RunnerName,
		RunnerRevision: adapter.RunnerRevision(),
		Execution: runners.Execution{
			Kind:        runners.ExecutionFunction,
			ImageRef:    key,
			ImageDigest: digest,
		},
		Command: runners.Command{Argv: []string{"true"}, EnvironmentRefs: []string{}},
		Policy: runners.Policy{
			TimeoutSeconds:     60,
			Network:            runners.NetworkFull,
			AllowedOutputPaths: []string{},
		},
		Evidence: runners.EvidenceRequest{CaptureExit: true, CaptureLogs: true},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatalf("contracts.NewValidator: %v", err)
	}
	if err := validator.ValidateJSON(contracts.SchemaRunnerResult, encoded); err != nil {
		t.Fatalf("live result does not validate: %v\n%s", err, encoded)
	}

	if res.Environment.PlatformRequestID == "" {
		t.Error("real Lambda returned no request id")
	}
	if obs, _ := res.Observations.Get(runnerlambda.ObsImageDigest); !obs.Measured {
		t.Errorf("the image digest could not be attributed to the live invocation: %+v", obs)
	}
	if res.Changes.Complete {
		t.Error("changes.complete is true against real Lambda; it can never be")
	}
}

// TestLiveUnregisteredIdentityIsRefused proves the c41 refusal is local by
// pointing it at a name no registry holds while real credentials are
// available: if the refusal were IAM's rather than this process's, the error
// would come back as AccessDenied instead.
func TestLiveUnregisteredIdentityIsRefused(t *testing.T) {
	adapter, _ := liveAdapter(t)
	_, digest, _ := liveConfig(t)

	_, err := adapter.Execute(t.Context(), runners.Operation{
		OperationID:    "op_live_refusal",
		Runner:         runnerlambda.RunnerName,
		RunnerRevision: adapter.RunnerRevision(),
		Execution: runners.Execution{
			Kind:        runners.ExecutionFunction,
			ImageRef:    "live/never-registered",
			ImageDigest: digest,
		},
		Command: runners.Command{Argv: []string{"true"}},
		Policy: runners.Policy{
			TimeoutSeconds:     60,
			Network:            runners.NetworkFull,
			AllowedOutputPaths: []string{},
		},
		Evidence: runners.EvidenceRequest{CaptureExit: true},
	})
	if !errors.Is(err, runners.ErrUnregisteredFunction) {
		t.Fatalf("error = %v, want ErrUnregisteredFunction", err)
	}
}
