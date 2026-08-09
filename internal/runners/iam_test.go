package runners_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// placeholderARNs are the two ARNs deploy/aws/worker-iam-policy.json is
// rendered from. They are syntactically real and semantically fake (account
// 000000000000), so an operator who applies the template unchanged grants
// access to nothing rather than to something.
var placeholderARNs = []string{
	"arn:aws:lambda:us-east-1:000000000000:function:culture-nodes-runner-default",
	"arn:aws:lambda:us-east-1:000000000000:function:culture-nodes-runner-tests",
}

const (
	placeholderBucket = "culture-nodes-artifacts-000000000000"
	placeholderPrefix = "artifacts"
)

// templatePath locates deploy/aws/worker-iam-policy.json from this test's own
// file, so the test does not depend on the working directory.
func templatePath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repository root")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	return filepath.Join(root, "deploy", "aws", "worker-iam-policy.json")
}

func placeholderRegistry(t *testing.T) *runners.FunctionRegistry {
	t.Helper()
	registry := runners.NewFunctionRegistry()
	for i, arn := range placeholderARNs {
		digest := testDigest
		if i == 1 {
			digest = otherDiges
		}
		name := []string{"default", "tests"}[i]
		if err := registry.RegisterFunction(name, runners.FunctionIdentity{ARN: arn, ImageDigest: digest}); err != nil {
			t.Fatalf("RegisterFunction(%s): %v", name, err)
		}
	}
	return registry
}

// TestRenderedPolicyEnumeratesRegisteredARNsOnly is honesty condition h36's
// second half: the worker IAM policy enumerates registered function ARNs, no
// wildcard invoke.
func TestRenderedPolicyEnumeratesRegisteredARNsOnly(t *testing.T) {
	registry := placeholderRegistry(t)
	policy, err := runners.RenderWorkerIAMPolicy(registry, runners.IAMOptions{
		ArtifactBucket: placeholderBucket,
		ArtifactPrefix: placeholderPrefix,
	})
	if err != nil {
		t.Fatalf("RenderWorkerIAMPolicy: %v", err)
	}

	assertNoLambdaWildcard(t, policy)

	invoke := statement(t, policy, runners.SidInvokeFunctions)
	if got, want := invoke.Resource, registry.ARNs(); !equalStrings(got, want) {
		t.Errorf("invoke Resource = %v, want exactly the registered ARNs %v", got, want)
	}
	if !equalStrings(invoke.Action, []string{"lambda:InvokeFunction"}) {
		t.Errorf("invoke Action = %v, want only lambda:InvokeFunction", invoke.Action)
	}

	read := statement(t, policy, runners.SidReadFunctionPins)
	if got, want := read.Resource, registry.ARNs(); !equalStrings(got, want) {
		t.Errorf("GetFunction Resource = %v, want exactly the registered ARNs %v", got, want)
	}

	deny := statement(t, policy, runners.SidDenyOtherInvokes)
	if deny.Effect != "Deny" {
		t.Errorf("the belt-and-braces statement is %q, not a Deny", deny.Effect)
	}
	if len(deny.Resource) != 0 {
		t.Errorf("the Deny must use NotResource, not Resource: %v", deny.Resource)
	}
	if got, want := deny.NotResource, registry.ARNs(); !equalStrings(got, want) {
		t.Errorf("Deny NotResource = %v, want the registered ARNs %v", got, want)
	}
}

// TestPolicyGrowsOnlyWithTheRegistry proves the two lists are one list: a
// function that is not registered cannot appear in the policy, and one that
// is must.
func TestPolicyGrowsOnlyWithTheRegistry(t *testing.T) {
	registry := placeholderRegistry(t)
	before, err := runners.RenderWorkerIAMPolicy(registry, runners.IAMOptions{
		ArtifactBucket: placeholderBucket, ArtifactPrefix: placeholderPrefix,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := len(statement(t, before, runners.SidInvokeFunctions).Resource); got != 2 {
		t.Fatalf("expected two ARNs before, got %d", got)
	}

	newARN := "arn:aws:lambda:eu-west-1:000000000000:function:culture-nodes-runner-lint"
	if err := registry.RegisterFunction("lint", runners.FunctionIdentity{ARN: newARN, ImageDigest: testDigest}); err != nil {
		t.Fatalf("RegisterFunction: %v", err)
	}
	after, err := runners.RenderWorkerIAMPolicy(registry, runners.IAMOptions{
		ArtifactBucket: placeholderBucket, ArtifactPrefix: placeholderPrefix,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	resources := statement(t, after, runners.SidInvokeFunctions).Resource
	if len(resources) != 3 || !contains(resources, newARN) {
		t.Errorf("invoke Resource = %v, want the newly registered ARN included", resources)
	}
	assertNoLambdaWildcard(t, after)
}

// TestEmptyRegistryRendersNoPolicy refuses the shape that invites a wildcard
// fix: an IAM statement with an empty Resource list.
func TestEmptyRegistryRendersNoPolicy(t *testing.T) {
	_, err := runners.RenderWorkerIAMPolicy(runners.NewFunctionRegistry(), runners.IAMOptions{
		ArtifactBucket: placeholderBucket, ArtifactPrefix: placeholderPrefix,
	})
	if err == nil {
		t.Fatal("an empty registry rendered a policy")
	}
	if !strings.Contains(err.Error(), "wildcard is not a fix") {
		t.Errorf("refusal does not warn against the obvious wrong fix: %v", err)
	}
}

// TestArtifactScopeIsRequired refuses a whole-bucket grant.
func TestArtifactScopeIsRequired(t *testing.T) {
	registry := placeholderRegistry(t)
	for _, opts := range []runners.IAMOptions{
		{ArtifactBucket: "", ArtifactPrefix: placeholderPrefix},
		{ArtifactBucket: placeholderBucket, ArtifactPrefix: ""},
		{ArtifactBucket: placeholderBucket, ArtifactPrefix: "/"},
	} {
		if _, err := runners.RenderWorkerIAMPolicy(registry, opts); err == nil {
			t.Errorf("RenderWorkerIAMPolicy accepted %+v", opts)
		}
	}
}

// TestCheckedInTemplateMatchesTheRenderer holds deploy/aws/worker-iam-policy.json
// and RenderWorkerIAMPolicy together. The template is what an operator reads
// and adapts; the renderer is what a deployment tool will generate. If they
// can drift, the reference policy stops being a reference.
func TestCheckedInTemplateMatchesTheRenderer(t *testing.T) {
	want, err := runners.RenderWorkerIAMPolicy(placeholderRegistry(t), runners.IAMOptions{
		ArtifactBucket: placeholderBucket,
		ArtifactPrefix: placeholderPrefix,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	rendered, err := want.MarshalIndent()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	onDisk, err := os.ReadFile(templatePath(t))
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	if string(onDisk) != string(rendered) {
		t.Errorf("deploy/aws/worker-iam-policy.json has drifted from RenderWorkerIAMPolicy.\n--- on disk ---\n%s\n--- rendered ---\n%s",
			onDisk, rendered)
	}
}

// TestCheckedInTemplateHasNoWildcardResource reads the committed file on its
// own terms — parsing the JSON rather than trusting the renderer — because
// the file is what actually gets applied to an AWS account.
func TestCheckedInTemplateHasNoWildcardResource(t *testing.T) {
	data, err := os.ReadFile(templatePath(t))
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	var policy runners.IAMPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		t.Fatalf("parse template: %v", err)
	}

	assertNoLambdaWildcard(t, policy)

	for _, stmt := range policy.Statement {
		for _, resource := range append(append([]string{}, stmt.Resource...), stmt.NotResource...) {
			if resource == "*" {
				t.Errorf("statement %q grants Resource \"*\"", stmt.Sid)
			}
		}
	}

	// The one wildcard the policy is allowed to contain is an S3 object-key
	// suffix under a named bucket and prefix. Anything else — a wildcard
	// bucket, account, or region — is a scope nobody chose.
	objects := statement(t, policy, runners.SidArtifactObjects)
	wantObject := "arn:aws:s3:::" + placeholderBucket + "/" + placeholderPrefix + "/*"
	if !equalStrings(objects.Resource, []string{wantObject}) {
		t.Errorf("artifact object Resource = %v, want %q", objects.Resource, wantObject)
	}

	listing := statement(t, policy, runners.SidArtifactListing)
	if !equalStrings(listing.Resource, []string{"arn:aws:s3:::" + placeholderBucket}) {
		t.Errorf("bucket listing Resource = %v, want the bare bucket ARN", listing.Resource)
	}
	if listing.Condition["StringLike"]["s3:prefix"] == nil {
		t.Error("bucket listing has no prefix condition, so it lists the whole bucket")
	}
}

// assertNoLambdaWildcard is the property honesty condition h36 names: no
// wildcard ever reaches a Lambda action's Resource.
func assertNoLambdaWildcard(t *testing.T, policy runners.IAMPolicy) {
	t.Helper()
	resources := policy.LambdaResources()
	if len(resources) == 0 {
		t.Fatal("the policy names no Lambda resources at all")
	}
	for _, resource := range resources {
		if strings.ContainsAny(resource, "*?") {
			t.Errorf("Lambda resource %q contains a wildcard; the worker role may invoke only enumerated function ARNs", resource)
		}
	}
}

func statement(t *testing.T, policy runners.IAMPolicy, sid string) runners.IAMStatement {
	t.Helper()
	for _, stmt := range policy.Statement {
		if stmt.Sid == sid {
			return stmt
		}
	}
	t.Fatalf("policy has no statement %q", sid)
	return runners.IAMStatement{}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
