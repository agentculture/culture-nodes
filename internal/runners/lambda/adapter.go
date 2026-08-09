package lambda

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awslambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/smithy-go"

	"github.com/agentculture/culture-nodes/internal/awsauth"
	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/runners"
)

// RunnerName is the logical runner name operations must declare to reach this
// adapter.
const RunnerName = "lambda"

// Platform limits. Both are validation-time rules, not runtime surprises: the
// compiler already rejects an over-cap timeout, and Execute checks again
// because an adapter that cannot enforce a policy limit must refuse the
// operation rather than run it under a different one.
const (
	// MaxTimeoutSeconds is Lambda's hard 15-minute maximum duration.
	MaxTimeoutSeconds = 900
	// MaxSyncPayloadBytes is Lambda's 6 MB synchronous invocation payload
	// limit, applied to the request and to the response.
	MaxSyncPayloadBytes = 6 * 1024 * 1024
	// payloadHeadroomBytes is subtracted from MaxSyncPayloadBytes when
	// deciding whether to refuse a request. The 6 MB limit applies to the
	// whole HTTP request, not to the JSON document alone, so a payload that
	// only just fits locally can still be rejected by the service. Refusing
	// early, with a remediation, beats an opaque RequestEntityTooLarge.
	payloadHeadroomBytes = 64 * 1024
)

// AdapterRevisionSeed identifies this adapter's *contract* revision. The
// runner revision every result carries is its digest.
//
// It is not a build digest and does not pretend to be one: it changes when
// the operation/result mapping changes, not when the binary is rebuilt. A
// deployment that wants build provenance in the replay manifest sets
// Config.RunnerRevision to a digest it can actually vouch for.
const AdapterRevisionSeed = "culture-nodes/internal/runners/lambda@v1alpha1"

// DefaultRunnerRevision is AdapterRevisionSeed's digest.
var DefaultRunnerRevision = contracts.Digest([]byte(AdapterRevisionSeed))

// Config configures an Adapter.
type Config struct {
	// Registry is the allowlist of function identities this adapter may
	// dispatch to. Required — and required to be non-empty before Load, so
	// a misconfigured worker fails at startup rather than at the first code
	// node.
	Registry *runners.FunctionRegistry

	// Region is the AWS region the functions live in. Empty defaults to
	// "us-east-1", which against real AWS is almost certainly wrong and
	// should be set explicitly.
	Region string

	// Endpoint overrides the SDK's endpoint resolution. This package's own
	// tests point it at an in-process fake; production leaves it empty.
	Endpoint string

	// RunnerRevision overrides DefaultRunnerRevision. It must be a
	// "sha256:<64 hex>" digest, because the result schema says so.
	RunnerRevision string

	// ActorID is the ledger actor id this runner writes evidence under.
	// Empty defaults to DefaultActorID.
	ActorID string

	// Credentials, when non-nil, overrides the SDK's default credential
	// chain for New. Production leaves this nil and either accepts the
	// SDK's own default chain or uses NewFromAuth instead of New, which
	// resolves credentials through internal/awsauth.LoadConfig (task t17's
	// shared IRSA-ready resolver); the tests set static credentials the
	// fake never validates.
	Credentials aws.CredentialsProvider

	// HTTPClient, when non-nil, overrides the SDK's HTTP client. The tests
	// use it to reach an httptest.Server without touching process-wide
	// transport state.
	HTTPClient *http.Client

	// MaxAttempts overrides the SDK's retry attempt count. Zero means the
	// SDK default. The throttle test sets 1 so a 429 is observed once
	// rather than retried transparently out from under it.
	MaxAttempts int

	// Clock overrides time.Now, so a test can assert exact timings. The
	// adapter's own clock is the only wall-clock measurement it makes, and
	// it is reported as a round-trip, never as Lambda's execution duration.
	Clock func() time.Time
}

// DefaultActorID is the ledger actor id evidence from this adapter carries
// when Config.ActorID is empty.
const DefaultActorID = "runner_lambda"

// functionState is what Load learned about one registered function. It is
// read on every dispatch and refreshed only by another Load: a pin that
// changes under a running worker is a deployment event, not a per-invocation
// lookup.
type functionState struct {
	identity runners.FunctionIdentity
	// version is the function version GetFunction reported. The invoke
	// response's ExecutedVersion is compared against it; a mismatch means
	// the deployment moved and the cached image digest may no longer
	// describe what ran, which downgrades the image_digest observation
	// instead of silently misattributing it.
	version string
	// timeoutSeconds, memoryMiB, and ephemeralMiB are the function's own
	// configured limits — the values that actually bound an execution, which
	// is why a policy asking for something tighter than them is refused.
	timeoutSeconds int
	memoryMiB      int
	ephemeralMiB   int
	// vpcAttached records whether the function has VPC subnets. It is
	// necessary for a scoped-network policy and deliberately not treated as
	// sufficient: what a subnet routes to is outside this adapter's view.
	vpcAttached bool
}

// Adapter is the Lambda runners.Runner. It is safe for concurrent use.
type Adapter struct {
	client   invoker
	registry *runners.FunctionRegistry
	revision string
	actorID  string
	now      func() time.Time

	mu     sync.RWMutex
	loaded map[string]functionState
}

// invoker is the slice of the Lambda API this adapter uses. Narrowing it here
// keeps the AWS surface visible in one place — two calls, both read-only or
// idempotent-by-operation-id.
type invoker interface {
	Invoke(ctx context.Context, in *awslambda.InvokeInput, opts ...func(*awslambda.Options)) (*awslambda.InvokeOutput, error)
	GetFunction(ctx context.Context, in *awslambda.GetFunctionInput, opts ...func(*awslambda.Options)) (*awslambda.GetFunctionOutput, error)
}

var _ runners.Runner = (*Adapter)(nil)

// New builds an Adapter and loads every registered function's deployed
// configuration, verifying each pinned image digest against what the platform
// reports. A registry entry whose digest does not match the deployed image is
// a load failure: a pin nobody checked is not a pin.
//
// See NewFromAuth for an alternative constructor that resolves credentials
// through internal/awsauth.LoadConfig (task t17's shared IRSA-ready
// resolver) instead of this function's own inline
// awsconfig.LoadDefaultConfig option list. New is unchanged and remains the
// default path -- NewFromAuth is additive.
func New(ctx context.Context, cfg Config) (*Adapter, error) {
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if cfg.Credentials != nil {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(cfg.Credentials))
	}
	if cfg.HTTPClient != nil {
		loadOpts = append(loadOpts, awsconfig.WithHTTPClient(cfg.HTTPClient))
	}
	if cfg.MaxAttempts > 0 {
		loadOpts = append(loadOpts, awsconfig.WithRetryMaxAttempts(cfg.MaxAttempts))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("runners/lambda: load AWS config: %w", err)
	}

	return newAdapter(ctx, awsCfg, cfg)
}

// NewFromAuth builds an Adapter the same way New does, except AWS
// credentials and region are resolved through internal/awsauth.LoadConfig
// (task t17) rather than this package's own awsconfig.LoadDefaultConfig
// option list -- giving this adapter IRSA support and Source reporting (via
// authOpts.Logf) for free.
//
// cfg.Credentials is ignored on this path: authOpts is the single source of
// authentication configuration. cfg.Region, if authOpts.Region is empty, is
// used as authOpts.Region's fallback (and "us-east-1" if both are empty,
// matching New's own default) -- everything else on cfg (Registry,
// Endpoint, RunnerRevision, ActorID, HTTPClient, MaxAttempts, Clock) applies
// exactly as it does for New, including the same Load(ctx) call at
// construction.
func NewFromAuth(ctx context.Context, authOpts awsauth.Options, cfg Config) (*Adapter, error) {
	if authOpts.Region == "" {
		authOpts.Region = cfg.Region
	}
	if authOpts.Region == "" {
		authOpts.Region = "us-east-1"
	}

	awsCfg, _, err := awsauth.LoadConfig(ctx, authOpts)
	if err != nil {
		return nil, fmt.Errorf("runners/lambda: NewFromAuth: resolve AWS credentials: %w", err)
	}

	if cfg.HTTPClient != nil {
		awsCfg.HTTPClient = cfg.HTTPClient
	}
	if cfg.MaxAttempts > 0 {
		awsCfg.RetryMaxAttempts = cfg.MaxAttempts
	}

	return newAdapter(ctx, awsCfg, cfg)
}

// newAdapter builds and loads an Adapter from an already-resolved
// aws.Config, shared by New and NewFromAuth. Registry and revision
// validation happen here (not before awsCfg is resolved) so both
// constructors refuse the same misconfigurations the same way.
func newAdapter(ctx context.Context, awsCfg aws.Config, cfg Config) (*Adapter, error) {
	if cfg.Registry == nil {
		return nil, fmt.Errorf("runners/lambda: New requires a function registry")
	}
	if cfg.Registry.Len() == 0 {
		return nil, fmt.Errorf(
			"runners/lambda: New requires at least one registered function; " +
				"an adapter with an empty registry refuses every dispatch, which is a misconfiguration worth failing at startup")
	}

	revision := cfg.RunnerRevision
	if revision == "" {
		revision = DefaultRunnerRevision
	}
	if !strings.HasPrefix(revision, contracts.DigestPrefix) || len(revision) != len(contracts.DigestPrefix)+64 {
		return nil, fmt.Errorf("runners/lambda: runner revision %q is not a sha256 digest", revision)
	}

	client := awslambda.NewFromConfig(awsCfg, func(o *awslambda.Options) {
		if cfg.Endpoint != "" {
			endpoint := cfg.Endpoint
			o.BaseEndpoint = &endpoint
		}
	})

	actorID := cfg.ActorID
	if actorID == "" {
		actorID = DefaultActorID
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	adapter := &Adapter{
		client:   client,
		registry: cfg.Registry,
		revision: revision,
		actorID:  actorID,
		now:      clock,
		loaded:   make(map[string]functionState),
	}
	if err := adapter.Load(ctx); err != nil {
		return nil, err
	}
	return adapter, nil
}

// ActorID returns the ledger actor id this adapter's evidence carries.
func (a *Adapter) ActorID() string { return a.actorID }

// RunnerRevision returns the revision every result reports.
func (a *Adapter) RunnerRevision() string { return a.revision }

// Load reads each registered function's deployed configuration and verifies
// its pinned image digest. It is called by New and may be called again to
// pick up a redeploy.
//
// GetFunction is the only extra AWS call this adapter makes, and it is why
// the reference IAM policy grants lambda:GetFunction on exactly the same
// enumerated ARNs as lambda:InvokeFunction (ADR 0003): a pin the worker
// cannot read is a pin it cannot verify.
func (a *Adapter) Load(ctx context.Context) error {
	loaded := make(map[string]functionState, a.registry.Len())

	for _, name := range a.registry.Names() {
		identity, err := a.registry.Resolve(name)
		if err != nil {
			return err
		}

		out, err := a.client.GetFunction(ctx, &awslambda.GetFunctionInput{
			FunctionName: aws.String(identity.ARN),
		})
		if err != nil {
			return fmt.Errorf("runners/lambda: read function %s (registered as %q): %w", identity.ARN, name, classify("", name, err))
		}
		state, err := functionStateFrom(name, identity, out)
		if err != nil {
			return err
		}
		loaded[name] = state
	}

	a.mu.Lock()
	a.loaded = loaded
	a.mu.Unlock()
	return nil
}

// functionStateFrom validates a GetFunction response against a registered
// identity and projects the fields dispatch needs.
func functionStateFrom(name string, identity runners.FunctionIdentity, out *awslambda.GetFunctionOutput) (functionState, error) {
	if out == nil || out.Configuration == nil {
		return functionState{}, fmt.Errorf("runners/lambda: function %s returned no configuration", identity.ARN)
	}
	cfg := out.Configuration

	if cfg.PackageType != lambdatypes.PackageTypeImage {
		return functionState{}, fmt.Errorf(
			"runners/lambda: function %s (registered as %q) is package type %q; "+
				"the runner boundary dispatches to container-image functions pinned by digest, and a zip package has no image digest to pin",
			identity.ARN, name, cfg.PackageType)
	}

	deployed := resolvedImageDigest(out.Code)
	switch {
	case deployed == "":
		return functionState{}, fmt.Errorf(
			"runners/lambda: function %s (registered as %q) reported no resolved image digest, so its pin cannot be verified",
			identity.ARN, name)
	case deployed != identity.ImageDigest:
		return functionState{}, fmt.Errorf(
			"runners/lambda: function %s (registered as %q) is deployed from %s but the registry pins %s: %w",
			identity.ARN, name, deployed, identity.ImageDigest, runners.ErrDigestMismatch)
	}

	state := functionState{identity: identity, version: aws.ToString(cfg.Version)}
	state.timeoutSeconds = int(aws.ToInt32(cfg.Timeout))
	state.memoryMiB = int(aws.ToInt32(cfg.MemorySize))
	if cfg.EphemeralStorage != nil {
		state.ephemeralMiB = int(aws.ToInt32(cfg.EphemeralStorage.Size))
	}
	if cfg.VpcConfig != nil {
		state.vpcAttached = len(cfg.VpcConfig.SubnetIds) > 0
	}
	return state, nil
}

// resolvedImageDigest pulls the "@sha256:…" digest out of a resolved image
// URI. An image URI without a digest yields "", which the caller turns into a
// load failure rather than an unverified pin.
func resolvedImageDigest(code *lambdatypes.FunctionCodeLocation) string {
	if code == nil {
		return ""
	}
	for _, uri := range []string{aws.ToString(code.ResolvedImageUri), aws.ToString(code.ImageUri)} {
		if _, digest, ok := strings.Cut(uri, "@"); ok && strings.HasPrefix(digest, contracts.DigestPrefix) {
			return digest
		}
	}
	return ""
}

// Execute dispatches one operation and maps the invocation onto a
// schema-valid Result claiming only what Lambda observably provided.
//
// Order matters and is not an accident: every refusal that can be decided
// locally is decided before a request exists, so an unregistered identity, an
// unenforceable policy, and an oversize payload each cost zero AWS calls and
// zero IAM exposure.
func (a *Adapter) Execute(ctx context.Context, op runners.Operation) (runners.Result, error) {
	state, payload, err := a.prepare(op)
	if err != nil {
		return runners.Result{}, err
	}

	logType := lambdatypes.LogTypeNone
	if op.Evidence.CaptureLogs {
		logType = lambdatypes.LogTypeTail
	}

	started := a.now().UTC()
	out, invokeErr := a.client.Invoke(ctx, &awslambda.InvokeInput{
		FunctionName:   aws.String(state.identity.ARN),
		InvocationType: lambdatypes.InvocationTypeRequestResponse,
		LogType:        logType,
		Payload:        payload,
	})
	finished := a.now().UTC()

	if invokeErr != nil {
		return runners.Result{}, classify(op.OperationID, op.Execution.ImageRef, invokeErr)
	}

	return a.buildResult(op, state, out, started, finished)
}

// prepare performs every local refusal and returns the resolved function
// state plus the request payload.
func (a *Adapter) prepare(op runners.Operation) (functionState, []byte, error) {
	reject := func(kind runners.ErrorKind, sentinel error, detail string) error {
		return &runners.DispatchError{
			Kind:        kind,
			OperationID: op.OperationID,
			Identity:    op.Execution.ImageRef,
			Detail:      detail,
			Err:         sentinel,
		}
	}

	if op.Runner != RunnerName {
		return functionState{}, nil, reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			fmt.Sprintf("operation names runner %q; this adapter is %q", op.Runner, RunnerName))
	}
	if op.RunnerRevision != a.revision {
		return functionState{}, nil, reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			fmt.Sprintf("operation pins runner revision %s; this adapter is %s — "+
				"recompile the workflow against the deployed adapter, because a revision pin that is not checked is not a pin",
				op.RunnerRevision, a.revision))
	}
	if op.Execution.Kind != runners.ExecutionFunction {
		return functionState{}, nil, reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			fmt.Sprintf("execution kind %q is not %q; a container operation belongs to an isolating container runner",
				op.Execution.Kind, runners.ExecutionFunction))
	}

	// Registry first: dispatch to an unregistered identity is refused before
	// anything else happens (spec claim c41).
	identity, err := a.registry.Resolve(op.Execution.ImageRef)
	if err != nil {
		var dispatchErr *runners.DispatchError
		if errors.As(err, &dispatchErr) {
			dispatchErr.OperationID = op.OperationID
		}
		return functionState{}, nil, err
	}
	if op.Execution.ImageDigest != identity.ImageDigest {
		return functionState{}, nil, reject(runners.ErrorAuthOrPolicy, runners.ErrDigestMismatch,
			fmt.Sprintf("operation pins image %s; %q is registered at %s",
				op.Execution.ImageDigest, op.Execution.ImageRef, identity.ImageDigest))
	}

	a.mu.RLock()
	state, loaded := a.loaded[op.Execution.ImageRef]
	a.mu.RUnlock()
	if !loaded {
		return functionState{}, nil, reject(runners.ErrorRunnerUnavailable, runners.ErrRunnerUnavailable,
			"the identity is registered but its deployed configuration has not been loaded; call Load before dispatching")
	}

	if err := checkPolicy(op, state, reject); err != nil {
		return functionState{}, nil, err
	}

	payload, err := json.Marshal(op)
	if err != nil {
		return functionState{}, nil, reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			fmt.Sprintf("encode operation: %v", err))
	}
	if limit := MaxSyncPayloadBytes - payloadHeadroomBytes; len(payload) > limit {
		return functionState{}, nil, reject(runners.ErrorRejectedInput, runners.ErrOversizePayload,
			fmt.Sprintf("operation payload is %d bytes and the synchronous invoke limit is %d (refused at %d to leave room for request framing); "+
				"pass the workspace and inputs as S3 artifact refs instead of inline content",
				len(payload), MaxSyncPayloadBytes, limit))
	}
	return state, payload, nil
}

// checkPolicy refuses every policy field this adapter cannot enforce, and
// verifies the ones it can against the function's deployed configuration.
//
// The schema is explicit that "fields a given runner cannot enforce must be
// rejected by that adapter rather than silently ignored", and that is the
// whole content of this function. Lambda enforces limits through the
// function's *configuration*, not per invocation, so "enforced" here means
// the deployed configuration is already at least as tight as the policy asks.
func checkPolicy(op runners.Operation, state functionState, reject func(runners.ErrorKind, error, string) error) error {
	policy := op.Policy

	switch {
	case policy.TimeoutSeconds <= 0:
		return reject(runners.ErrorRejectedInput, runners.ErrTimeoutNotEnforceable,
			"policy declares no timeout")
	case policy.TimeoutSeconds > MaxTimeoutSeconds:
		return reject(runners.ErrorRejectedInput, runners.ErrTimeoutNotEnforceable,
			fmt.Sprintf("policy timeout is %ds and Lambda's maximum duration is %ds; "+
				"a workload that needs longer needs a different runner adapter, not a longer wait",
				policy.TimeoutSeconds, MaxTimeoutSeconds))
	case state.timeoutSeconds > 0 && policy.TimeoutSeconds > state.timeoutSeconds:
		return reject(runners.ErrorRejectedInput, runners.ErrTimeoutNotEnforceable,
			fmt.Sprintf("policy timeout is %ds but function %s is configured to time out at %ds; "+
				"the platform would stop the work before the policy's own bound, so the policy is not the limit that applies",
				policy.TimeoutSeconds, state.identity.ARN, state.timeoutSeconds))
	}

	if policy.CPU != nil {
		return reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			"policy sets a cpu limit; Lambda derives CPU from configured memory and cannot accept one — set memory_mib instead")
	}
	if policy.PIDs != nil {
		return reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			"policy sets a pid limit; Lambda exposes no per-invocation process limit and this adapter will not pretend to enforce one")
	}
	if policy.MemoryMiB != nil && state.memoryMiB > 0 && state.memoryMiB > *policy.MemoryMiB {
		return reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			fmt.Sprintf("policy allows %d MiB but function %s is configured with %d MiB; "+
				"function memory is a deploy-time setting, so redeploy the function rather than loosening the policy at dispatch",
				*policy.MemoryMiB, state.identity.ARN, state.memoryMiB))
	}
	if policy.DiskMiB != nil && state.ephemeralMiB > 0 && state.ephemeralMiB > *policy.DiskMiB {
		return reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			fmt.Sprintf("policy allows %d MiB of scratch disk but function %s is configured with %d MiB of ephemeral storage",
				*policy.DiskMiB, state.identity.ARN, state.ephemeralMiB))
	}

	if policy.Network != runners.NetworkFull && !state.vpcAttached {
		return reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			fmt.Sprintf("policy asks for network %q but function %s is not VPC-attached, so its egress is the platform default; "+
				"attach the function to subnets whose routing enforces the posture — this adapter verifies attachment and never claims to have observed what those subnets route to",
				policy.Network, state.identity.ARN))
	}

	if len(policy.AllowedOutputPaths) > 0 {
		return reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			fmt.Sprintf("policy allows output paths %v; Lambda has no controlled workspace whose paths this adapter could scope, "+
				"so declare allowed_output_paths as [] and return outputs as artifact refs", policy.AllowedOutputPaths))
	}

	if op.Command.RequiresShell != nil && *op.Command.RequiresShell {
		return reject(runners.ErrorAuthOrPolicy, runners.ErrUnsupportedOperation,
			"operation declares requires_shell; policy rejects it — the command is an argument array the function image executes directly")
	}
	if len(op.Command.Argv) == 0 {
		return reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			"operation declares no command argv")
	}
	return nil
}

// functionReport is the partial result the function returns. Everything in it
// is process-reported: it is what the code inside the container says about
// itself, and no field of it becomes an observation.
type functionReport struct {
	ExitCode  *int              `json:"exit_code"`
	Signal    *string           `json:"signal"`
	Artifacts map[string]string `json:"artifacts,omitempty"`
	Message   string            `json:"message,omitempty"`
}

// lambdaErrorPayload is the object Lambda returns when the handler raised.
type lambdaErrorPayload struct {
	ErrorType    string `json:"errorType"`
	ErrorMessage string `json:"errorMessage"`
}

// buildResult maps the invoke response onto a schema-valid Result.
func (a *Adapter) buildResult(
	op runners.Operation,
	state functionState,
	out *awslambda.InvokeOutput,
	started, finished time.Time,
) (runners.Result, error) {
	requestID, _ := awsmiddleware.GetRequestIDMetadata(out.ResultMetadata)
	executedVersion := aws.ToString(out.ExecutedVersion)
	functionError := aws.ToString(out.FunctionError)

	logTail := decodeLogTail(aws.ToString(out.LogResult))
	rep := parseReport(logTail)

	policyDigest, err := contracts.DigestValue(op.Policy)
	if err != nil {
		return runners.Result{}, fmt.Errorf("runners/lambda: digest policy: %w", err)
	}

	result := runners.Result{
		OperationID: op.OperationID,
		Timing:      buildTiming(started, finished, rep),
		Environment: runners.Environment{
			RunnerRevision:    a.revision,
			ImageDigest:       state.identity.ImageDigest,
			PolicyDigest:      policyDigest,
			PlatformRequestID: requestID,
		},
		// c25: Lambda cannot observe a workspace. The workspace arrives and
		// leaves as S3 artifact refs and the platform never compares them, so
		// changes is complete:false with no paths — every time, on every
		// result, including a successful one. Fabricating "no changes" here
		// would read as an observation that nothing changed.
		Changes: runners.Changes{Complete: false},
	}
	if op.Workspace != nil {
		digest := op.Workspace.SourceDigest
		result.Environment.InputDigest = &digest
	}
	if state.memoryMiB > 0 {
		memory := state.memoryMiB
		result.Environment.MemoryMiB = &memory
	}

	reported, exit, executionState, resultErr := interpretPayload(functionError, out.Payload)
	result.State = executionState
	result.Exit = exit
	result.Error = resultErr
	if reported != nil && len(reported.Artifacts) > 0 {
		artifacts := remapArtifacts(reported.Artifacts)
		result.Artifacts = &artifacts
	}

	if rep.MaxMemoryMB != nil {
		maxMemory := *rep.MaxMemoryMB
		result.ResourceUsage = &runners.ResourceUsage{MaxMemoryMiB: &maxMemory}
	}

	result.Observations = a.observations(op, state, rep, logTail, executedVersion, requestID)
	return result, nil
}

// buildTiming reports the platform's duration when the REPORT line carried
// one and the adapter's own round-trip otherwise. The distinction is not
// cosmetic — the round-trip includes network and SDK time the function never
// spent — so it is declared in the `duration` observation's method.
func buildTiming(started, finished time.Time, rep report) runners.Timing {
	timing := runners.Timing{
		StartedAt:  started,
		FinishedAt: finished,
		DurationMs: int(finished.Sub(started).Milliseconds()),
	}
	if rep.DurationMs != nil {
		timing.DurationMs = int(*rep.DurationMs)
	}
	if rep.BilledMs != nil {
		billed := int(*rep.BilledMs)
		timing.BilledDurationMs = &billed
	}
	return timing
}

// interpretPayload turns the invoke response's FunctionError and payload into
// a state, an exit, and an error.
//
// The exit it produces is the function's own report about a process it ran.
// Whether anything measured that exit is a separate question, answered by the
// exit_status observation — which on this platform always answers "no".
func interpretPayload(functionError string, payload []byte) (*functionReport, *runners.Exit, runners.State, *runners.ResultError) {
	if functionError != "" {
		var raised lambdaErrorPayload
		_ = json.Unmarshal(payload, &raised)

		message := strings.TrimSpace(raised.ErrorType + ": " + raised.ErrorMessage)
		if strings.Contains(raised.ErrorMessage, "Task timed out") {
			return nil, nil, runners.StateTimedOut, &runners.ResultError{
				Kind:      runners.ErrorTimeout,
				Retryable: false,
				Message:   raised.ErrorMessage,
			}
		}
		return nil, nil, runners.StateFailed, &runners.ResultError{
			Kind:      runners.ErrorExecutionFailure,
			Retryable: false,
			Message:   "the function handler raised (" + functionError + "): " + message,
		}
	}

	var report functionReport
	if err := json.Unmarshal(payload, &report); err != nil {
		return nil, nil, runners.StateFailed, &runners.ResultError{
			Kind:      runners.ErrorContractFailure,
			Retryable: false,
			Message: "the function returned a payload this adapter cannot read as a runner report: " + err.Error() +
				" — the contract is a JSON object carrying exit_code and optional artifact refs",
		}
	}
	if report.ExitCode == nil {
		return &report, nil, runners.StateFailed, &runners.ResultError{
			Kind:      runners.ErrorContractFailure,
			Retryable: false,
			Message: "the function returned no exit_code; the runner contract requires one, and this adapter will not " +
				"substitute a zero for a value the function declined to state",
		}
	}
	return &report, &runners.Exit{Code: report.ExitCode, Signal: report.Signal}, runners.StateCompleted, nil
}

// remapArtifacts copies function-reported refs onto the result's artifact
// block, keeping the schema's named keys named and carrying the rest through
// rather than dropping refs a caller may need to fetch.
//
// These are references the function asserted it wrote. Nothing here is an
// observation: the artifact store verifies content against the digest it
// recorded at Put time, and that is where the trust in an artifact ref
// actually comes from.
func remapArtifacts(src map[string]string) runners.Artifacts {
	var dst runners.Artifacts
	for key, value := range src {
		if value == "" {
			continue
		}
		switch key {
		case "stdout_ref":
			dst.StdoutRef = value
		case "stderr_ref":
			dst.StderrRef = value
		case "output_workspace_ref":
			dst.OutputWorkspaceRef = value
		case "result_payload_ref":
			dst.ResultPayloadRef = value
		default:
			if dst.Additional == nil {
				dst.Additional = map[string]string{}
			}
			dst.Additional[key] = value
		}
	}
	return dst
}

// decodeLogTail base64-decodes the log tail, returning "" for anything it
// cannot decode. An unreadable tail is the same as an absent one: it produces
// {measured:false}, never a guess.
func decodeLogTail(encoded string) string {
	if encoded == "" {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return ""
	}
	return string(decoded)
}

// classify maps an AWS SDK error onto a typed DispatchError. Every mapping
// answers one question — may the runtime retry this by itself? — and the
// default answer is no, because "we do not know what went wrong" is not a
// reason to do it again.
func classify(operationID, identity string, err error) error {
	build := func(kind runners.ErrorKind, detail string) error {
		return &runners.DispatchError{
			Kind:        kind,
			OperationID: operationID,
			Identity:    identity,
			Detail:      detail,
			Err:         runners.SentinelFor(kind),
		}
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		detail := code + ": " + apiErr.ErrorMessage()
		switch code {
		case "TooManyRequestsException":
			return build(runners.ErrorRateLimited, detail+" — Lambda throttled the invoke; this is retryable")
		case "ServiceException", "EC2ThrottledException", "ResourceNotReadyException",
			"EC2AccessDeniedException", "ENILimitReachedException":
			return build(runners.ErrorRunnerUnavailable, detail)
		case "AccessDeniedException", "KMSAccessDeniedException", "KMSDisabledException":
			return build(runners.ErrorAuthOrPolicy, detail+
				" — the worker role may invoke only the enumerated registered function ARNs (ADR 0003)")
		case "ResourceNotFoundException":
			return build(runners.ErrorAuthOrPolicy, detail+
				" — the registry names a function the account does not have; registry and deployment disagree")
		case "RequestTooLargeException":
			return &runners.DispatchError{
				Kind:        runners.ErrorRejectedInput,
				OperationID: operationID,
				Identity:    identity,
				Detail:      detail + " — pass inputs as S3 artifact refs",
				Err:         runners.ErrOversizePayload,
			}
		case "InvalidParameterValueException", "InvalidRequestContentException",
			"UnsupportedMediaTypeException", "ResourceConflictException":
			return build(runners.ErrorRejectedInput, detail)
		}
		if apiErr.ErrorFault() == smithy.FaultServer {
			return build(runners.ErrorRetryableTransport, detail)
		}
		return build(runners.ErrorExecutionFailure, detail)
	}

	return build(runners.ErrorRetryableTransport, "invoke transport failure: "+err.Error())
}
