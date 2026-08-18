package runners_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/runners"
)

const (
	testARN      = "arn:aws:lambda:us-east-1:123456789012:function:nodes-run-tests"
	testARNTwo   = "arn:aws:lambda:us-east-1:123456789012:function:nodes-build"
	testDigest   = "sha256:0604fdb7edd7a8eacbcbdebdf7aad00db03650efd7f927f4b16f6cd0e0c3747e"
	otherDiges   = "sha256:104c92af170b72888c5be5b64aa08aff686010ae232cd101b8c1c61f7eff0636"
	testEndpoint = "https://runner.thor.internal:8443"
	testSecret   = "runner/thor/execute-token"
)

func testRegistry(t *testing.T) *runners.FunctionRegistry {
	t.Helper()
	registry := runners.NewFunctionRegistry()
	if err := registry.RegisterFunction("deliver-change/run-tests", runners.FunctionIdentity{
		ARN:         testARN,
		ImageDigest: testDigest,
	}); err != nil {
		t.Fatalf("RegisterFunction: %v", err)
	}
	return registry
}

// TestResolveRefusesUnregisteredIdentity is spec claim c41's core property at
// the registry level: an identity nobody registered has no answer, and the
// refusal is typed so a caller cannot mistake it for a transient failure.
func TestResolveRefusesUnregisteredIdentity(t *testing.T) {
	registry := testRegistry(t)

	_, err := registry.Resolve("deliver-change/exfiltrate")
	if err == nil {
		t.Fatal("Resolve of an unregistered identity returned no error")
	}
	if !errors.Is(err, runners.ErrUnregisteredFunction) {
		t.Errorf("error does not match ErrUnregisteredFunction: %v", err)
	}

	var dispatchErr *runners.DispatchError
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("error is not a *DispatchError: %v", err)
	}
	if dispatchErr.Kind != runners.ErrorAuthOrPolicy {
		t.Errorf("Kind = %q, want %q", dispatchErr.Kind, runners.ErrorAuthOrPolicy)
	}
	if dispatchErr.Retryable() {
		t.Error("a refused identity is not retryable: asking the same forbidden question again cannot succeed")
	}
	if !strings.Contains(dispatchErr.Error(), "deliver-change/exfiltrate") {
		t.Errorf("refusal does not name the requested identity: %v", dispatchErr)
	}
}

// TestEmptyRegistryRefusesEverything states the safe default out loud: a
// worker that has not been told what it may invoke may invoke nothing.
func TestEmptyRegistryRefusesEverything(t *testing.T) {
	registry := runners.NewFunctionRegistry()
	if _, err := registry.Resolve("anything"); !errors.Is(err, runners.ErrUnregisteredFunction) {
		t.Errorf("an empty registry must refuse every name, got %v", err)
	}
}

func TestRegisterFunctionValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		key      string
		identity runners.FunctionIdentity
		wantIn   string
	}{
		{
			name:     "wildcard ARN",
			key:      "wild",
			identity: runners.FunctionIdentity{ARN: "arn:aws:lambda:us-east-1:123456789012:function:*", ImageDigest: testDigest},
			wantIn:   "wildcard",
		},
		{
			name:     "bare function name",
			key:      "bare",
			identity: runners.FunctionIdentity{ARN: "nodes-run-tests", ImageDigest: testDigest},
			wantIn:   "fully-qualified",
		},
		{
			name:     "unpinned image",
			key:      "unpinned",
			identity: runners.FunctionIdentity{ARN: testARN},
			wantIn:   "pinned image digest",
		},
		{
			name:     "digest that is not a digest",
			key:      "bad-digest",
			identity: runners.FunctionIdentity{ARN: testARN, ImageDigest: "latest"},
			wantIn:   "sha256",
		},
		{
			name:     "empty name",
			key:      "",
			identity: runners.FunctionIdentity{ARN: testARN, ImageDigest: testDigest},
			wantIn:   "not a valid identity name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := runners.NewFunctionRegistry()
			err := registry.RegisterFunction(tc.key, tc.identity)
			if err == nil {
				t.Fatalf("RegisterFunction accepted %+v", tc.identity)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not explain the problem (%q)", err, tc.wantIn)
			}
			if registry.Len() != 0 {
				t.Error("a refused registration must not land in the registry")
			}
		})
	}
}

// TestRepointingANameIsRefused keeps the allowlist from widening in place.
// Re-registering the identical identity stays a no-op so idempotent startup
// wiring is not punished.
func TestRepointingANameIsRefused(t *testing.T) {
	registry := testRegistry(t)

	if err := registry.RegisterFunction("deliver-change/run-tests", runners.FunctionIdentity{
		ARN: testARN, ImageDigest: testDigest,
	}); err != nil {
		t.Errorf("re-registering an identical identity should be a no-op, got %v", err)
	}

	err := registry.RegisterFunction("deliver-change/run-tests", runners.FunctionIdentity{
		ARN: testARNTwo, ImageDigest: otherDiges,
	})
	if err == nil {
		t.Fatal("repointing a registered name was accepted")
	}
	identity, resolveErr := registry.Resolve("deliver-change/run-tests")
	if resolveErr != nil {
		t.Fatalf("Resolve: %v", resolveErr)
	}
	if identity.ARN != testARN {
		t.Errorf("the original pin was overwritten: %s", identity.ARN)
	}
}

func TestNodeKeyNamespacesByWorkflow(t *testing.T) {
	if got := runners.NodeKey("deliver-change", "run-tests"); got != "deliver-change/run-tests" {
		t.Errorf("NodeKey = %q", got)
	}
	registry := runners.NewFunctionRegistry()
	if err := registry.RegisterFunction(runners.NodeKey("deliver-change", "run-tests"), runners.FunctionIdentity{
		ARN: testARN, ImageDigest: testDigest,
	}); err != nil {
		t.Fatalf("a NodeKey must be a registrable name: %v", err)
	}
}

func TestARNsAreDistinctAndSorted(t *testing.T) {
	registry := testRegistry(t)
	for name, arn := range map[string]string{
		"deliver-change/build": testARNTwo,
		"shared/run-tests":     testARN, // same ARN under a second logical name
	} {
		if err := registry.RegisterFunction(name, runners.FunctionIdentity{ARN: arn, ImageDigest: testDigest}); err != nil {
			t.Fatalf("RegisterFunction(%s): %v", name, err)
		}
	}

	arns := registry.ARNs()
	if len(arns) != 2 {
		t.Fatalf("ARNs() = %v, want the two distinct ARNs", arns)
	}
	if arns[0] > arns[1] {
		t.Errorf("ARNs() is not sorted: %v", arns)
	}
}

// --- runner-service identities (api/runner-protocol) ---------------------
//
// The second identity form is what makes the registry runner-neutral: a
// runner reached over the wire protocol has an endpoint, a pinned digest and
// a named credential, and no ARN anywhere. The tests below hold the line that
// the ARN form and the service form coexist without either weakening the
// other's validation.

// TestRegisterServiceAcceptsTheProtocolIdentityForm is acceptance criterion 2
// stated as a test: ARN-only validation no longer blocks a non-Lambda runner.
func TestRegisterServiceAcceptsTheProtocolIdentityForm(t *testing.T) {
	for _, tc := range []struct {
		name     string
		identity runners.ServiceIdentity
	}{
		{
			name:     "https endpoint on another machine",
			identity: runners.ServiceIdentity{Endpoint: testEndpoint, ImageDigest: testDigest, SecretRef: testSecret},
		},
		{
			name:     "loopback http needs no insecure opt-in",
			identity: runners.ServiceIdentity{Endpoint: "http://127.0.0.1:8080", ImageDigest: testDigest, SecretRef: testSecret},
		},
		{
			name:     "localhost by name is loopback too",
			identity: runners.ServiceIdentity{Endpoint: "http://localhost:8080/runner", ImageDigest: testDigest, SecretRef: testSecret},
		},
		{
			name: "plaintext to another host only with the explicit opt-in",
			identity: runners.ServiceIdentity{
				Endpoint: "http://thor.lan:8080", ImageDigest: testDigest, SecretRef: testSecret,
				AllowInsecureTransport: true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := runners.NewFunctionRegistry()
			if err := registry.RegisterService("deliver-change/run-tests", tc.identity); err != nil {
				t.Fatalf("RegisterService(%+v): %v", tc.identity, err)
			}
			resolved, err := registry.ResolveService("deliver-change/run-tests")
			if err != nil {
				t.Fatalf("ResolveService: %v", err)
			}
			if resolved != tc.identity {
				t.Errorf("ResolveService = %+v, want %+v", resolved, tc.identity)
			}
			if kind, ok := registry.Kind("deliver-change/run-tests"); !ok || kind != runners.IdentityService {
				t.Errorf("Kind = %q/%v, want %q", kind, ok, runners.IdentityService)
			}
		})
	}
}

func TestRegisterServiceValidation(t *testing.T) {
	valid := runners.ServiceIdentity{Endpoint: testEndpoint, ImageDigest: testDigest, SecretRef: testSecret}
	with := func(mutate func(*runners.ServiceIdentity)) runners.ServiceIdentity {
		identity := valid
		mutate(&identity)
		return identity
	}

	for _, tc := range []struct {
		name     string
		key      string
		identity runners.ServiceIdentity
		wantIn   string
	}{
		{
			name:     "no endpoint",
			key:      "no-endpoint",
			identity: with(func(s *runners.ServiceIdentity) { s.Endpoint = "" }),
			wantIn:   "endpoint",
		},
		{
			name:     "wildcard endpoint",
			key:      "wild",
			identity: with(func(s *runners.ServiceIdentity) { s.Endpoint = "https://*.thor.internal" }),
			wantIn:   "wildcard",
		},
		{
			name:     "not an absolute URL",
			key:      "relative",
			identity: with(func(s *runners.ServiceIdentity) { s.Endpoint = "runner.thor.internal:8443" }),
			wantIn:   "absolute",
		},
		{
			name:     "unsupported scheme",
			key:      "unix",
			identity: with(func(s *runners.ServiceIdentity) { s.Endpoint = "unix:///var/run/runner.sock" }),
			wantIn:   "http or https",
		},
		{
			name:     "plaintext to a remote host without the opt-in",
			key:      "plaintext",
			identity: with(func(s *runners.ServiceIdentity) { s.Endpoint = "http://thor.lan:8080" }),
			wantIn:   "AllowInsecureTransport",
		},
		{
			name:     "credentials embedded in the URL",
			key:      "userinfo",
			identity: with(func(s *runners.ServiceIdentity) { s.Endpoint = "https://user:pass@runner.thor.internal" }),
			wantIn:   "credential",
		},
		{
			name:     "query string in the endpoint",
			key:      "query",
			identity: with(func(s *runners.ServiceIdentity) { s.Endpoint = testEndpoint + "/?token=abc" }),
			wantIn:   "query",
		},
		{
			name:     "unpinned image",
			key:      "unpinned",
			identity: with(func(s *runners.ServiceIdentity) { s.ImageDigest = "" }),
			wantIn:   "pinned image digest",
		},
		{
			name:     "digest that is not a digest",
			key:      "bad-digest",
			identity: with(func(s *runners.ServiceIdentity) { s.ImageDigest = "latest" }),
			wantIn:   "sha256",
		},
		{
			name:     "no secret reference",
			key:      "authless",
			identity: with(func(s *runners.ServiceIdentity) { s.SecretRef = "" }),
			wantIn:   "authentication is mandatory",
		},
		{
			name:     "the secret itself instead of a reference",
			key:      "inline-secret",
			identity: with(func(s *runners.ServiceIdentity) { s.SecretRef = "Bearer s3cr3t-material" }),
			wantIn:   "reference name",
		},
		{
			name:     "empty name",
			key:      "",
			identity: valid,
			wantIn:   "not a valid identity name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := runners.NewFunctionRegistry()
			err := registry.RegisterService(tc.key, tc.identity)
			if err == nil {
				t.Fatalf("RegisterService accepted %+v", tc.identity)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not explain the problem (%q)", err, tc.wantIn)
			}
			if registry.Len() != 0 {
				t.Error("a refused registration must not land in the registry")
			}
		})
	}
}

// TestResolveServiceRefusesUnregisteredIdentity mirrors the function form's
// core refusal: a runner service nobody registered has no endpoint to reach,
// and the refusal is typed so a caller cannot mistake it for a hiccup.
func TestResolveServiceRefusesUnregisteredIdentity(t *testing.T) {
	registry := runners.NewFunctionRegistry()
	if err := registry.RegisterService("deliver-change/run-tests", runners.ServiceIdentity{
		Endpoint: testEndpoint, ImageDigest: testDigest, SecretRef: testSecret,
	}); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}

	_, err := registry.ResolveService("deliver-change/exfiltrate")
	if !errors.Is(err, runners.ErrUnregisteredFunction) {
		t.Fatalf("error does not match ErrUnregisteredFunction: %v", err)
	}
	var dispatchErr *runners.DispatchError
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("error is not a *DispatchError: %v", err)
	}
	if dispatchErr.Kind != runners.ErrorAuthOrPolicy {
		t.Errorf("Kind = %q, want %q", dispatchErr.Kind, runners.ErrorAuthOrPolicy)
	}
	if dispatchErr.Retryable() {
		t.Error("a refused identity is not retryable")
	}
}

// TestIdentityFormsShareOneNamespace keeps the two forms from talking past
// each other. A name means one thing, and asking for it in the wrong form is
// answered with what it actually is rather than with "never heard of it" —
// which is the difference between a diagnosable misconfiguration and a hunt.
func TestIdentityFormsShareOneNamespace(t *testing.T) {
	registry := testRegistry(t) // "deliver-change/run-tests" is an ARN identity
	service := runners.ServiceIdentity{Endpoint: testEndpoint, ImageDigest: testDigest, SecretRef: testSecret}

	if err := registry.RegisterService("deliver-change/run-tests", service); err == nil {
		t.Fatal("a registered function name was silently repointed at a runner service")
	}
	if err := registry.RegisterService("deliver-change/build", service); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}
	if err := registry.RegisterFunction("deliver-change/build", runners.FunctionIdentity{
		ARN: testARNTwo, ImageDigest: testDigest,
	}); err == nil {
		t.Fatal("a registered service name was silently repointed at a Lambda function")
	}

	_, err := registry.Resolve("deliver-change/build")
	if !errors.Is(err, runners.ErrUnsupportedOperation) {
		t.Errorf("Resolve of a service identity: want ErrUnsupportedOperation, got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "runner-service") {
		t.Errorf("refusal does not say which form the name actually holds: %v", err)
	}

	_, err = registry.ResolveService("deliver-change/run-tests")
	if !errors.Is(err, runners.ErrUnsupportedOperation) {
		t.Errorf("ResolveService of a function identity: want ErrUnsupportedOperation, got %v", err)
	}

	if names := registry.Names(); len(names) != 2 || names[0] != "deliver-change/build" || names[1] != "deliver-change/run-tests" {
		t.Errorf("Names() = %v, want both forms in one sorted namespace", names)
	}
	if registry.Len() != 2 {
		t.Errorf("Len() = %d, want 2", registry.Len())
	}
}

// TestRepointingAServiceNameIsRefused holds the same line for services that
// TestRepointingANameIsRefused holds for functions.
func TestRepointingAServiceNameIsRefused(t *testing.T) {
	registry := runners.NewFunctionRegistry()
	identity := runners.ServiceIdentity{Endpoint: testEndpoint, ImageDigest: testDigest, SecretRef: testSecret}
	if err := registry.RegisterService("deliver-change/run-tests", identity); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}
	if err := registry.RegisterService("deliver-change/run-tests", identity); err != nil {
		t.Errorf("re-registering an identical identity should be a no-op, got %v", err)
	}

	moved := identity
	moved.Endpoint = "https://runner.orin.internal:8443"
	if err := registry.RegisterService("deliver-change/run-tests", moved); err == nil {
		t.Fatal("repointing a registered service endpoint was accepted")
	}
	resolved, err := registry.ResolveService("deliver-change/run-tests")
	if err != nil {
		t.Fatalf("ResolveService: %v", err)
	}
	if resolved.Endpoint != testEndpoint {
		t.Errorf("the original pin was overwritten: %s", resolved.Endpoint)
	}
}

// TestServiceIdentitiesStayOutOfTheIAMPolicy is the containment property of
// having two forms: a runner service is reached over the protocol with a
// registered credential, so it must never widen the AWS grant the ARN form
// renders. Registering ten of them adds nothing to the worker's policy.
func TestServiceIdentitiesStayOutOfTheIAMPolicy(t *testing.T) {
	registry := testRegistry(t)
	if err := registry.RegisterService("deliver-change/build", runners.ServiceIdentity{
		Endpoint: testEndpoint, ImageDigest: testDigest, SecretRef: testSecret,
	}); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}

	arns := registry.ARNs()
	if len(arns) != 1 || arns[0] != testARN {
		t.Errorf("ARNs() = %v, want only the registered Lambda ARN", arns)
	}

	endpoints := registry.Endpoints()
	if len(endpoints) != 1 || endpoints[0] != testEndpoint {
		t.Errorf("Endpoints() = %v, want only the registered service endpoint", endpoints)
	}
}

// TestServiceOnlyRegistryRendersNoIAMPolicy states the same fact from the
// other side, and requires the refusal to say *why* rather than claim the
// registry is empty when it holds runner services.
func TestServiceOnlyRegistryRendersNoIAMPolicy(t *testing.T) {
	registry := runners.NewFunctionRegistry()
	if err := registry.RegisterService("deliver-change/run-tests", runners.ServiceIdentity{
		Endpoint: testEndpoint, ImageDigest: testDigest, SecretRef: testSecret,
	}); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}

	_, err := runners.RenderWorkerIAMPolicy(registry, runners.IAMOptions{
		ArtifactBucket: "bucket", ArtifactPrefix: "artifacts",
	})
	if err == nil {
		t.Fatal("a registry of runner services rendered a Lambda IAM policy")
	}
	if !strings.Contains(err.Error(), "runner-service") {
		t.Errorf("refusal does not explain that the registry holds service identities: %v", err)
	}
}

// TestEndpointsAreDistinctAndSorted mirrors TestARNsAreDistinctAndSorted:
// Endpoints() is the operator-facing list of hosts this worker may reach, the
// way ARNs() is the list of functions it may invoke.
func TestEndpointsAreDistinctAndSorted(t *testing.T) {
	registry := runners.NewFunctionRegistry()
	for name, endpoint := range map[string]string{
		"deliver-change/run-tests": testEndpoint,
		"deliver-change/build":     "https://runner.orin.internal:8443",
		"shared/run-tests":         testEndpoint, // same endpoint under a second logical name
	} {
		if err := registry.RegisterService(name, runners.ServiceIdentity{
			Endpoint: endpoint, ImageDigest: testDigest, SecretRef: testSecret,
		}); err != nil {
			t.Fatalf("RegisterService(%s): %v", name, err)
		}
	}

	endpoints := registry.Endpoints()
	if len(endpoints) != 2 {
		t.Fatalf("Endpoints() = %v, want the two distinct endpoints", endpoints)
	}
	if endpoints[0] > endpoints[1] {
		t.Errorf("Endpoints() is not sorted: %v", endpoints)
	}
}

// TestKindOfAnUnregisteredNameIsNotAGuess keeps Kind from inventing a default
// for a name nobody registered.
func TestKindOfAnUnregisteredNameIsNotAGuess(t *testing.T) {
	registry := runners.NewFunctionRegistry()
	if kind, ok := registry.Kind("nothing"); ok || kind != "" {
		t.Errorf("Kind of an unregistered name = %q/%v, want \"\"/false", kind, ok)
	}
}

// TestReloadServicesReplacesTheWholeServiceSet is task t19's core registry
// property (issue #8's "runner services load at worker start only" gap): a
// name repointed through ReloadServices is accepted -- unlike RegisterService,
// which refuses exactly that -- and a name dropped from the reload's map is
// no longer resolvable, because the reload's set is the new complete truth.
func TestReloadServicesReplacesTheWholeServiceSet(t *testing.T) {
	registry := runners.NewFunctionRegistry()
	first := runners.ServiceIdentity{Endpoint: testEndpoint, ImageDigest: testDigest, SecretRef: testSecret}
	if err := registry.RegisterService("delivery-loop/keep", first); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}
	if err := registry.RegisterService("delivery-loop/drop", first); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}

	moved := first
	moved.Endpoint = "https://runner.orin.internal:8443"
	if err := registry.ReloadServices(map[string]runners.ServiceIdentity{
		"delivery-loop/keep": moved, // repointed -- refused by RegisterService, accepted here
		"delivery-loop/new":  first, // newly added
		// "delivery-loop/drop" is absent: it must stop resolving.
	}); err != nil {
		t.Fatalf("ReloadServices: %v", err)
	}

	kept, err := registry.ResolveService("delivery-loop/keep")
	if err != nil {
		t.Fatalf("ResolveService(keep): %v", err)
	}
	if kept.Endpoint != moved.Endpoint {
		t.Errorf("reload did not repoint delivery-loop/keep: got %s", kept.Endpoint)
	}
	if _, err := registry.ResolveService("delivery-loop/new"); err != nil {
		t.Errorf("ResolveService(new): %v", err)
	}
	if _, err := registry.ResolveService("delivery-loop/drop"); err == nil {
		t.Error("delivery-loop/drop should no longer resolve after a reload that omitted it")
	}
}

// TestReloadServicesRefusesAnInvalidEntryWithoutChangingAnything is the
// all-or-nothing property: one malformed identity in the reload set must
// leave every already-registered name exactly as it was, not half-replaced.
func TestReloadServicesRefusesAnInvalidEntryWithoutChangingAnything(t *testing.T) {
	registry := runners.NewFunctionRegistry()
	good := runners.ServiceIdentity{Endpoint: testEndpoint, ImageDigest: testDigest, SecretRef: testSecret}
	if err := registry.RegisterService("delivery-loop/keep", good); err != nil {
		t.Fatalf("RegisterService: %v", err)
	}

	err := registry.ReloadServices(map[string]runners.ServiceIdentity{
		"delivery-loop/keep": good,
		"delivery-loop/bad":  {Endpoint: testEndpoint}, // no digest, no secret ref: invalid
	})
	if err == nil {
		t.Fatal("ReloadServices with an invalid entry must be refused")
	}

	kept, resolveErr := registry.ResolveService("delivery-loop/keep")
	if resolveErr != nil {
		t.Fatalf("a refused reload must not disturb an already-registered name: %v", resolveErr)
	}
	if kept.Endpoint != testEndpoint {
		t.Errorf("delivery-loop/keep changed despite the refused reload: %s", kept.Endpoint)
	}
	if _, err := registry.ResolveService("delivery-loop/bad"); err == nil {
		t.Error("the invalid entry must not have been registered either")
	}
}

// TestReloadServicesRefusesCollidingWithAFunctionIdentity keeps the
// registry's one-name-one-kind invariant across a reload the same way it
// holds across two direct RegisterX calls.
func TestReloadServicesRefusesCollidingWithAFunctionIdentity(t *testing.T) {
	registry := testRegistry(t) // registers "deliver-change/run-tests" as a function
	err := registry.ReloadServices(map[string]runners.ServiceIdentity{
		"deliver-change/run-tests": {Endpoint: testEndpoint, ImageDigest: testDigest, SecretRef: testSecret},
	})
	if err == nil {
		t.Fatal("a reload that collides with a registered function identity must be refused")
	}
	if kind, ok := registry.Kind("deliver-change/run-tests"); !ok || kind != runners.IdentityFunction {
		t.Errorf("the function identity must survive the refused reload, got kind=%q ok=%v", kind, ok)
	}
}
