package runners

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// FunctionIdentity is one registered, digest-pinned function a code node may
// be dispatched to (spec claim c41).
//
// Both fields are load-bearing and both are checked. The ARN is what the
// worker's IAM policy enumerates, so an identity that is not in the registry
// is also not in the policy — refusal in code and refusal in IAM are two
// renderings of one list (see RenderWorkerIAMPolicy). The image digest is
// what makes "pinned" true: an adapter compares the operation's declared
// digest against this one and, at load time, against the digest the platform
// reports for the deployed function.
type FunctionIdentity struct {
	// ARN is the fully-qualified function ARN. Wildcards are refused at
	// registration: a wildcard here would become a wildcard in the IAM
	// policy, which is exactly the thing c41 forbids.
	ARN string
	// ImageDigest is the pinned container-image digest, "sha256:" + 64 hex.
	ImageDigest string
	// Description is optional, for operator-facing listings.
	Description string
}

// arnPattern matches a fully-qualified Lambda function ARN, optionally with a
// version or alias qualifier. It is intentionally strict: partial ARNs and
// bare function names are legal inputs to the AWS Invoke API, but they are
// not legal *policy* subjects, and this registry is the policy's source.
var arnPattern = regexp.MustCompile(
	`^arn:aws[a-z-]*:lambda:[a-z0-9-]+:\d{12}:function:[A-Za-z0-9_-]{1,140}(:[A-Za-z0-9$_-]{1,128})?$`)

// digestPattern matches the schema's digest form.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// namePattern matches a registry key. It is the operation's
// execution.image_ref value, so it stays within the schema's identifier
// alphabet and is safe to print in an error.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@/+-]{0,127}$`)

// Validate reports whether an identity is well-formed enough to register.
func (f FunctionIdentity) Validate() error {
	switch {
	case f.ARN == "":
		return fmt.Errorf("runners: function identity requires an ARN")
	case strings.Contains(f.ARN, "*") || strings.Contains(f.ARN, "?"):
		return fmt.Errorf("runners: function ARN %q contains a wildcard; "+
			"the registry is the source of the worker's IAM policy and that policy enumerates ARNs", f.ARN)
	case !arnPattern.MatchString(f.ARN):
		return fmt.Errorf("runners: function ARN %q is not a fully-qualified Lambda function ARN "+
			"(arn:aws:lambda:<region>:<account>:function:<name>[:<qualifier>])", f.ARN)
	case f.ImageDigest == "":
		return fmt.Errorf("runners: function %s requires a pinned image digest; an unpinned function is not an execution environment", f.ARN)
	case !digestPattern.MatchString(f.ImageDigest):
		return fmt.Errorf("runners: function %s image digest %q is not sha256:<64 hex>", f.ARN, f.ImageDigest)
	}
	return nil
}

// NodeKey builds the registry key for a function registered against one
// workflow node, so a node's code identity is namespaced by the workflow that
// declares it rather than sharing a global name.
func NodeKey(workflow, nodeID string) string {
	return workflow + "/" + nodeID
}

// FunctionRegistry is the allowlist of function identities a runner may
// dispatch to. It is the single enforcement point for spec claim c41:
// Resolve refuses an unregistered name, and it refuses it *before* an adapter
// has built a request, let alone sent one.
//
// It is safe for concurrent use. Registration is expected to happen at
// startup from configuration; Resolve happens on every dispatch.
type FunctionRegistry struct {
	mu    sync.RWMutex
	byKey map[string]FunctionIdentity
}

// NewFunctionRegistry returns an empty registry. Empty is the safe default:
// a registry with nothing in it refuses every dispatch, which is the correct
// behaviour for a worker that has not been told what it may invoke.
func NewFunctionRegistry() *FunctionRegistry {
	return &FunctionRegistry{byKey: make(map[string]FunctionIdentity)}
}

// RegisterFunction binds a logical name (a NodeKey, or a shared name several
// nodes reference) to a digest-pinned identity.
//
// Re-registering the same name with the same identity is a no-op. Changing an
// existing name's identity is refused: a running worker's allowlist is not
// something a later call gets to widen silently, and the operator-visible way
// to change a pin is to rebuild the registry.
func (r *FunctionRegistry) RegisterFunction(name string, identity FunctionIdentity) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("runners: registry name %q is not a valid identity name", name)
	}
	if err := identity.Validate(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byKey[name]; ok {
		if existing.ARN == identity.ARN && existing.ImageDigest == identity.ImageDigest {
			return nil
		}
		return fmt.Errorf("runners: %q is already registered as %s @ %s; "+
			"rebuild the registry rather than repointing a name in place",
			name, existing.ARN, existing.ImageDigest)
	}
	r.byKey[name] = identity
	return nil
}

// Resolve returns the identity registered under name.
//
// An unregistered name yields a *DispatchError wrapping
// ErrUnregisteredFunction. Callers must treat that as terminal: it is not a
// transport hiccup, and retrying it would only ask the same forbidden
// question again.
func (r *FunctionRegistry) Resolve(name string) (FunctionIdentity, error) {
	r.mu.RLock()
	identity, ok := r.byKey[name]
	registered := len(r.byKey)
	r.mu.RUnlock()

	if !ok {
		detail := fmt.Sprintf("no such identity in the runner registry (%d registered); "+
			"register the function's ARN and pinned image digest before a node may dispatch to it", registered)
		if name == "" {
			detail = "the operation named no function identity in execution.image_ref"
		}
		return FunctionIdentity{}, refuse(ErrorAuthOrPolicy, ErrUnregisteredFunction, "", name, detail)
	}
	return identity, nil
}

// Names returns the registered names, sorted.
func (r *FunctionRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.byKey))
	for name := range r.byKey {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ARNs returns the distinct registered function ARNs, sorted. This is what
// the worker's IAM policy enumerates.
func (r *FunctionRegistry) ARNs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]bool, len(r.byKey))
	for _, identity := range r.byKey {
		seen[identity.ARN] = true
	}
	arns := make([]string, 0, len(seen))
	for arn := range seen {
		arns = append(arns, arn)
	}
	sort.Strings(arns)
	return arns
}

// Len returns how many names are registered.
func (r *FunctionRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byKey)
}
