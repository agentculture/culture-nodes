package runners

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The IAM policy this file renders is the machine-readable half of ADR 0003
// (docs/adr/0003-lambda-runner-iam.md). deploy/aws/worker-iam-policy.json is
// the checked-in template of the same document with placeholder ARNs, and
// iam_test.go holds the two together.
//
// The rule the renderer exists to make structural: the worker role may
// invoke only the function ARNs the registry holds, enumerated one by one.
// There is no wildcard invoke, no "arn:aws:lambda:*:*:function:*", and no
// account-wide Action. A function that is not in the registry is not in the
// policy — which means an operator cannot widen dispatch by editing code
// alone, and cannot widen IAM without the registry entry that code refuses
// to dispatch without (spec claim c41 / honesty condition h36).

// IAM policy constants. The statement ids are stable: an operator diffing a
// rendered policy against the deployed one should see ARNs move, never Sids.
const (
	iamPolicyVersion = "2012-10-17"

	SidInvokeFunctions  = "InvokeRegisteredCodeRunnerFunctions"
	SidReadFunctionPins = "ReadRegisteredFunctionPinning"
	SidArtifactObjects  = "ArtifactBucketObjects"
	SidArtifactListing  = "ArtifactBucketListing"
	SidDenyOtherInvokes = "DenyInvokeOutsideRegisteredFunctions"
)

// IAMPolicy is an IAM policy document, shaped for JSON round-tripping rather
// than for expressiveness: only the fields this policy actually uses exist,
// so a field cannot be silently dropped on re-render.
type IAMPolicy struct {
	Version   string         `json:"Version"`
	Statement []IAMStatement `json:"Statement"`
}

// IAMStatement is one statement of an IAMPolicy.
type IAMStatement struct {
	Sid    string   `json:"Sid"`
	Effect string   `json:"Effect"`
	Action []string `json:"Action"`
	// Resource and NotResource are mutually exclusive in IAM and both carry
	// omitempty: a statement rendered with "Resource": null is not a valid
	// policy document, and emitting one would fail at apply time rather than
	// at review time.
	Resource    []string                       `json:"Resource,omitempty"`
	NotResource []string                       `json:"NotResource,omitempty"`
	Condition   map[string]map[string][]string `json:"Condition,omitempty"`
}

// IAMOptions carries the deployment-specific values the registry cannot
// know: which bucket holds artifacts and under which key prefix.
type IAMOptions struct {
	// ArtifactBucket is the S3 bucket name (not an ARN) holding workspace
	// and log artifacts.
	ArtifactBucket string
	// ArtifactPrefix is the key prefix within that bucket the worker may
	// read and write. Required: an empty prefix would grant the whole
	// bucket, and "the whole bucket" is not a scope.
	ArtifactPrefix string
}

// RenderWorkerIAMPolicy renders the worker role's policy from a registry.
//
// Every registered ARN is enumerated in the invoke statement and in the
// GetFunction statement — GetFunction because the adapter reads the deployed
// image digest at load time to verify the pin, and a pin it cannot read is a
// pin it cannot verify.
//
// An empty registry is refused rather than rendered: a policy with an empty
// Resource list is not valid IAM, and emitting one would tempt whoever
// applies it to "fix" it with a wildcard.
func RenderWorkerIAMPolicy(registry *FunctionRegistry, opts IAMOptions) (IAMPolicy, error) {
	if registry == nil {
		return IAMPolicy{}, fmt.Errorf("runners: RenderWorkerIAMPolicy requires a registry")
	}
	arns := registry.ARNs()
	if len(arns) == 0 {
		return IAMPolicy{}, fmt.Errorf(
			"runners: cannot render a worker IAM policy from an empty registry; " +
				"register the code-runner functions first — an empty Resource list is not a policy and a wildcard is not a fix")
	}
	if opts.ArtifactBucket == "" {
		return IAMPolicy{}, fmt.Errorf("runners: RenderWorkerIAMPolicy requires an artifact bucket")
	}
	prefix := strings.Trim(opts.ArtifactPrefix, "/")
	if prefix == "" {
		return IAMPolicy{}, fmt.Errorf(
			"runners: RenderWorkerIAMPolicy requires an artifact key prefix; " +
				"granting the whole bucket is not a scoped grant")
	}

	bucketARN := "arn:aws:s3:::" + opts.ArtifactBucket
	objectARN := bucketARN + "/" + prefix + "/*"

	return IAMPolicy{
		Version: iamPolicyVersion,
		Statement: []IAMStatement{
			{
				Sid:      SidInvokeFunctions,
				Effect:   "Allow",
				Action:   []string{"lambda:InvokeFunction"},
				Resource: arns,
			},
			{
				Sid:      SidReadFunctionPins,
				Effect:   "Allow",
				Action:   []string{"lambda:GetFunction"},
				Resource: arns,
			},
			{
				// The deny is belt-and-braces, not decoration: an Allow from
				// some *other* attached policy (a broad platform role, a
				// future managed policy) cannot re-open invoke, because an
				// explicit Deny always wins in IAM evaluation.
				Sid:         SidDenyOtherInvokes,
				Effect:      "Deny",
				Action:      []string{"lambda:InvokeFunction", "lambda:InvokeFunctionUrl"},
				NotResource: arns,
			},
			{
				Sid:      SidArtifactObjects,
				Effect:   "Allow",
				Action:   []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject"},
				Resource: []string{objectARN},
			},
			{
				// ListBucket is a bucket-level action, so its Resource is the
				// bucket ARN itself with no wildcard; the prefix condition is
				// what keeps the listing scoped.
				Sid:      SidArtifactListing,
				Effect:   "Allow",
				Action:   []string{"s3:ListBucket"},
				Resource: []string{bucketARN},
				Condition: map[string]map[string][]string{
					"StringLike": {"s3:prefix": {prefix + "/*"}},
				},
			},
		},
	}, nil
}

// MarshalIndent renders the policy as the indented JSON an operator commits.
func (p IAMPolicy) MarshalIndent() ([]byte, error) {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("runners: render IAM policy: %w", err)
	}
	return append(data, '\n'), nil
}

// LambdaResources returns every Resource (and NotResource) named by a
// statement whose actions are all Lambda actions. Tests use it to assert the
// property that matters: no wildcard ever reaches a Lambda invoke.
func (p IAMPolicy) LambdaResources() []string {
	var out []string
	for _, stmt := range p.Statement {
		lambdaOnly := len(stmt.Action) > 0
		for _, action := range stmt.Action {
			if !strings.HasPrefix(action, "lambda:") {
				lambdaOnly = false
				break
			}
		}
		if !lambdaOnly {
			continue
		}
		out = append(out, stmt.Resource...)
		out = append(out, stmt.NotResource...)
	}
	return out
}
