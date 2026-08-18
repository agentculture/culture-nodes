package runners

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// IdentityKind is which of the two registered identity forms a name holds.
//
// The two forms exist because "where does this code run" has two answers the
// registry must express without the workflow knowing which: a managed
// function reached through a cloud API, and a runner service reached over the
// wire protocol (api/runner-protocol). A workflow definition names neither —
// it names a node, and the registry says how that node's code is reached. So
// moving a code node from this machine to another is a registry change, not a
// workflow change.
type IdentityKind string

// The registered identity forms.
const (
	// IdentityFunction is the ARN-shaped, managed-function form the Lambda
	// adapter dispatches to.
	IdentityFunction IdentityKind = "function"
	// IdentityService is the runner-protocol form: an endpoint, a pinned
	// digest, and the name of the credential that authenticates the caller.
	IdentityService IdentityKind = "service"
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

// secretRefPattern matches a credential *reference* — a name a deployment
// resolves to secret material. It deliberately admits no whitespace, so the
// most likely operator slip (pasting the token itself, or a whole
// "Bearer …" header value) is refused at registration rather than written to
// a config file and forgotten there.
var secretRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@/+-]{0,127}$`)

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

// ServiceIdentity is one registered runner service a code node may be
// dispatched to over api/runner-protocol.
//
// It is the runner-neutral counterpart of FunctionIdentity: no ARN, no cloud,
// no assumption that the runner is on this machine or any particular one. All
// three fields are load-bearing and all three are checked.
//
//   - Endpoint is *where*, and it is the only thing that changes when a code
//     node moves from a laptop to a machine on the network. That is the
//     placement-unaware property in one field.
//   - ImageDigest is what makes "pinned" true: an adapter compares the
//     operation's declared execution.image_digest against this one and
//     refuses a mismatch, exactly as the function form does.
//   - SecretRef is what makes the caller authenticable. It is mandatory
//     because a runner service accepting operations over the network is a
//     remote-code-execution surface, and an unauthenticated one executes work
//     for anyone who can reach it.
type ServiceIdentity struct {
	// Endpoint is the runner service's base URL, e.g.
	// "https://runner.thor.internal:8443". The protocol paths are appended to
	// it (see ExecuteURL). Wildcards, query strings, fragments and embedded
	// credentials are refused at registration.
	Endpoint string
	// ImageDigest is the pinned execution-environment digest, "sha256:" + 64
	// hex — the digest the operation's execution.image_digest must match.
	ImageDigest string
	// SecretRef names the credential the deployment resolves to the bearer
	// secret presented on every execute and status request. It is a
	// reference, never the secret material: this struct is built from
	// configuration that gets logged, diffed, and committed.
	SecretRef string
	// AllowInsecureTransport permits a plaintext http endpoint to a host that
	// is not loopback. It exists as an explicit, greppable opt-in rather than
	// a silent default because presenting a bearer secret over plaintext to
	// another machine is the secret leaking — an operator may accept that on
	// a trusted LAN, but never by accident.
	AllowInsecureTransport bool
	// Description is optional, for operator-facing listings.
	Description string
}

// Validate reports whether a service identity is well-formed enough to
// register. It is as strict as the function form's Validate, for the same
// reason: the registry is the enforcement point, and a check skipped here is
// a check that happens nowhere.
func (s ServiceIdentity) Validate() error {
	if err := s.validateEndpoint(); err != nil {
		return err
	}
	switch {
	case s.ImageDigest == "":
		return fmt.Errorf("runners: runner service %s requires a pinned image digest; "+
			"an unpinned runner is not an execution environment", s.Endpoint)
	case !digestPattern.MatchString(s.ImageDigest):
		return fmt.Errorf("runners: runner service %s image digest %q is not sha256:<64 hex>", s.Endpoint, s.ImageDigest)
	case s.SecretRef == "":
		return fmt.Errorf("runners: runner service %s requires a secret reference; "+
			"caller authentication is mandatory on the runner protocol and an unauthenticated "+
			"runner service executes code for anyone who can reach it", s.Endpoint)
	case !secretRefPattern.MatchString(s.SecretRef):
		return fmt.Errorf("runners: runner service %s secret %q is not a valid secret reference name; "+
			"name the credential the deployment resolves, never the secret material itself", s.Endpoint, s.SecretRef)
	}
	return nil
}

// validateEndpoint holds the endpoint to the same standard the ARN pattern
// holds a function to: exact, reachable, and carrying no credential of its
// own.
func (s ServiceIdentity) validateEndpoint() error {
	raw := strings.TrimSpace(s.Endpoint)
	if raw == "" {
		return fmt.Errorf("runners: runner-service identity requires an endpoint URL")
	}
	if strings.Contains(raw, "*") {
		return fmt.Errorf("runners: runner service endpoint %q contains a wildcard; "+
			"the registry is the list of runners this worker may reach, and a wildcard is not a list", raw)
	}

	parsed, err := url.Parse(raw)
	scheme := ""
	if parsed != nil {
		scheme = strings.ToLower(parsed.Scheme)
	}
	if err != nil || (scheme != "http" && scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("runners: runner service endpoint %q must be an absolute http or https URL "+
			"(e.g. https://runner.thor.internal:8443)", raw)
	}
	if parsed.User != nil {
		return fmt.Errorf("runners: runner service endpoint %q embeds a credential; "+
			"register a secret reference instead — a URL is configuration and configuration gets logged", raw)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return fmt.Errorf("runners: runner service endpoint %q carries a query string or fragment; "+
			"the endpoint is a base URL the protocol paths are appended to", raw)
	}
	if scheme == "http" && !isLoopbackURL(parsed) && !s.AllowInsecureTransport {
		return fmt.Errorf("runners: runner service endpoint %q is plaintext http to a non-loopback host; "+
			"the caller's bearer secret would cross the network in the clear. "+
			"Use https, or set AllowInsecureTransport to accept that risk explicitly", raw)
	}
	return nil
}

// isLoopbackURL reports whether a URL's host is this machine. A loopback
// endpoint needs no transport opt-in: nothing leaves the host.
func isLoopbackURL(u *url.URL) bool {
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// NodeKey builds the registry key for a function registered against one
// workflow node, so a node's code identity is namespaced by the workflow that
// declares it rather than sharing a global name.
func NodeKey(workflow, nodeID string) string {
	return workflow + "/" + nodeID
}

// FunctionRegistry is the allowlist of execution identities a worker may
// dispatch to. It is the single enforcement point for spec claim c41:
// Resolve refuses an unregistered name, and it refuses it *before* an adapter
// has built a request, let alone sent one.
//
// One registry holds both identity forms in one namespace. That is
// deliberate: a name means one thing, and a node whose code moved from a
// managed function to a runner service must not end up resolvable as both.
// The type name is historical — it predates the second form — and is kept
// because renaming it would churn every caller for no behavioural gain.
//
// It is safe for concurrent use. Registration is expected to happen at
// startup from configuration; resolution happens on every dispatch.
type FunctionRegistry struct {
	mu    sync.RWMutex
	byKey map[string]registryEntry
}

// registryEntry is one registered name: which form it holds, and the identity
// of that form. Only the field matching kind is populated.
type registryEntry struct {
	kind     IdentityKind
	function FunctionIdentity
	service  ServiceIdentity
}

// NewFunctionRegistry returns an empty registry. Empty is the safe default:
// a registry with nothing in it refuses every dispatch, which is the correct
// behaviour for a worker that has not been told what it may invoke.
func NewFunctionRegistry() *FunctionRegistry {
	return &FunctionRegistry{byKey: make(map[string]registryEntry)}
}

// RegisterFunction binds a logical name (a NodeKey, or a shared name several
// nodes reference) to a digest-pinned identity.
//
// Re-registering the same name with the same identity is a no-op. Changing an
// existing name's identity is refused: a running worker's allowlist is not
// something a later call gets to widen silently, and the operator-visible way
// to change a pin is to rebuild the registry.
func (r *FunctionRegistry) RegisterFunction(name string, identity FunctionIdentity) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	return r.register(name, registryEntry{kind: IdentityFunction, function: identity})
}

// RegisterService binds a logical name to a digest-pinned runner service
// reached over api/runner-protocol.
//
// It follows RegisterFunction's rules exactly — identical re-registration is
// a no-op, repointing a name is refused — and shares its namespace, so a name
// can be a function or a service but never both.
func (r *FunctionRegistry) RegisterService(name string, identity ServiceIdentity) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	return r.register(name, registryEntry{kind: IdentityService, service: identity})
}

// register is the shared registration path for both identity forms.
func (r *FunctionRegistry) register(name string, entry registryEntry) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("runners: registry name %q is not a valid identity name", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byKey[name]; ok {
		if existing == entry {
			return nil
		}
		return fmt.Errorf("runners: %q is already registered as %s; "+
			"rebuild the registry rather than repointing a name in place",
			name, existing.describe())
	}
	r.byKey[name] = entry
	return nil
}

// describe renders an entry for an operator-facing message: what a name is
// already bound to, so a refused re-registration says which pin is in the way.
// It prints the endpoint and the digest, and no credential — a ServiceIdentity
// holds only a reference name, and even that has no business in an error.
func (e registryEntry) describe() string {
	if e.kind == IdentityService {
		return fmt.Sprintf("the runner service %s @ %s", e.service.Endpoint, e.service.ImageDigest)
	}
	return fmt.Sprintf("the function %s @ %s", e.function.ARN, e.function.ImageDigest)
}

// Resolve returns the managed-function identity registered under name.
//
// An unregistered name yields a *DispatchError wrapping
// ErrUnregisteredFunction. Callers must treat that as terminal: it is not a
// transport hiccup, and retrying it would only ask the same forbidden
// question again.
//
// A name registered in the *other* form is refused too, and says so: "this
// name is a runner service" is a diagnosable misconfiguration, while
// "unregistered" would send an operator hunting for a registration that is
// right there.
func (r *FunctionRegistry) Resolve(name string) (FunctionIdentity, error) {
	entry, err := r.lookup(name, IdentityFunction)
	if err != nil {
		return FunctionIdentity{}, err
	}
	return entry.function, nil
}

// ResolveService returns the runner-service identity registered under name,
// with the same refusals Resolve makes for the function form.
func (r *FunctionRegistry) ResolveService(name string) (ServiceIdentity, error) {
	entry, err := r.lookup(name, IdentityService)
	if err != nil {
		return ServiceIdentity{}, err
	}
	return entry.service, nil
}

// lookup is the shared resolution path: registered, and registered in the
// form the caller can actually dispatch to.
func (r *FunctionRegistry) lookup(name string, want IdentityKind) (registryEntry, error) {
	r.mu.RLock()
	entry, ok := r.byKey[name]
	registered := len(r.byKey)
	r.mu.RUnlock()

	if !ok {
		detail := fmt.Sprintf("no such identity in the runner registry (%d registered); "+
			"register the function's ARN, or the runner service's endpoint and secret reference, "+
			"with a pinned image digest before a node may dispatch to it", registered)
		if name == "" {
			detail = "the operation named no function identity in execution.image_ref"
		}
		return registryEntry{}, refuse(ErrorAuthOrPolicy, ErrUnregisteredFunction, "", name, detail)
	}
	if entry.kind != want {
		return registryEntry{}, refuse(ErrorRejectedInput, ErrUnsupportedOperation, "", name, fmt.Sprintf(
			"the identity is registered as %s, and this dispatch path handles %s identities; "+
				"a %s identity is dispatched to over %s",
			formName(entry.kind), formName(want), formName(entry.kind), formPath(entry.kind)))
	}
	return entry, nil
}

// formName renders an identity kind for a message.
func formName(kind IdentityKind) string {
	if kind == IdentityService {
		return "a runner-service endpoint"
	}
	return "a managed-function (ARN)"
}

// formPath names where each form is dispatched, so a refusal points at the
// contract the operator should be reading.
func formPath(kind IdentityKind) string {
	if kind == IdentityService {
		return "api/runner-protocol"
	}
	return "the platform's function-invoke API"
}

// Kind reports which identity form a name holds, and whether it is registered
// at all. It is how a dispatcher routes a work item to the right path without
// resolving into a type it may not be able to use.
func (r *FunctionRegistry) Kind(name string) (IdentityKind, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.byKey[name]
	if !ok {
		return "", false
	}
	return entry.kind, true
}

// Names returns the registered names of both forms, sorted.
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
// the worker's IAM policy enumerates — and it is why the two forms are
// separate records rather than one struct with optional fields: a runner
// service is reached with a registered credential over the protocol, so it
// must never be able to widen an AWS grant.
func (r *FunctionRegistry) ARNs() []string {
	return r.distinct(IdentityFunction, func(e registryEntry) string { return e.function.ARN })
}

// Endpoints returns the distinct registered runner-service endpoints, sorted.
// It is the protocol-side counterpart of ARNs: the list of hosts this worker
// may submit operations to, which is what an egress policy or an operator
// review needs to see. It carries no credential, only the reference names
// resolved elsewhere.
func (r *FunctionRegistry) Endpoints() []string {
	return r.distinct(IdentityService, func(e registryEntry) string { return e.service.Endpoint })
}

// distinct collects one field of every entry of a given kind, deduplicated
// and sorted.
func (r *FunctionRegistry) distinct(kind IdentityKind, field func(registryEntry) string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]bool, len(r.byKey))
	for _, entry := range r.byKey {
		if entry.kind == kind {
			seen[field(entry)] = true
		}
	}
	values := make([]string, 0, len(seen))
	for value := range seen {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

// Len returns how many names are registered.
func (r *FunctionRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byKey)
}

// ReloadServices atomically replaces every SERVICE identity in the registry
// with services, in one swap: every entry is validated before anything is
// changed, so a single malformed identity refuses the whole reload and
// leaves the registry exactly as it was — a worker mid-dispatch must never
// observe half of a rebuild (task t19, issue #8's "runner services load at
// worker start only" gap).
//
// This is the "rebuild the registry" RegisterService's own doc comment
// points an operator at when a name's identity genuinely needs to change —
// endpoint, digest, or secret reference — without a process restart.
// RegisterService refuses that same change as a repoint; ReloadServices is
// the sanctioned way to do it, because it is explicit about replacing the
// whole service set rather than quietly repointing one name underneath a
// caller that thinks it is still talking to what it registered.
//
// It is for a registry whose SERVICE entries all come from one reloadable
// source (cmd/nodes/runnerservices.go's NODES_RUNNER_SERVICES_FILE), not a
// mix of independently-managed RegisterService callers: a name registered
// through RegisterService directly and not present in a later
// ReloadServices call is removed, because the reload's services map is
// treated as the complete, authoritative set. Any FUNCTION identity already
// registered is left untouched, unless an incoming service claims the same
// name — the registry's one-name-one-kind invariant holds across a reload
// exactly as it holds across two RegisterX calls, so that collision is
// refused rather than silently resolved either way.
func (r *FunctionRegistry) ReloadServices(services map[string]ServiceIdentity) error {
	next := make(map[string]registryEntry, len(services))
	for name, identity := range services {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("runners: registry name %q is not a valid identity name", name)
		}
		if err := identity.Validate(); err != nil {
			return fmt.Errorf("runners: reloading service %q: %w", name, err)
		}
		next[name] = registryEntry{kind: IdentityService, service: identity}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for name, entry := range r.byKey {
		if entry.kind != IdentityFunction {
			continue
		}
		if _, clash := next[name]; clash {
			return fmt.Errorf("runners: reload cannot register service %q: "+
				"the name is already bound to %s", name, entry.describe())
		}
		next[name] = entry
	}
	r.byKey = next
	return nil
}
