package headspace

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/artifacts"
	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/runners"
)

// RunnerName is the logical runner name an Operation must declare to reach
// this bridge (runners.Operation.Runner).
const RunnerName = "headspace"

// AdapterRevisionSeed identifies this bridge's *contract* revision -- the
// Operation/Result mapping this file implements, not a build digest. It
// changes when that mapping changes, not when the binary is rebuilt, mirroring
// internal/runners/lambda's AdapterRevisionSeed convention exactly.
const AdapterRevisionSeed = "culture-nodes/internal/runners/headspace@v1alpha1"

// DefaultRunnerRevision is AdapterRevisionSeed's digest.
var DefaultRunnerRevision = contracts.Digest([]byte(AdapterRevisionSeed))

// DefaultProfilePython312 names the one profile headspace-cli 0.11.0
// registers upstream (see headspace-cli's headspace/core/profiles.py
// REGISTRY). It is not a fallback this bridge applies on its own -- every
// image digest a caller wants to run still needs an explicit entry in
// BridgeConfig.Profile -- it exists so refusal messages can name a concrete,
// currently-valid profile rather than an abstract "check the docs".
const DefaultProfilePython312 = "python3.12"

// JobWorkingDirectory is the directory headspace-cli starts every job in.
//
// headspace-cli's `run` verb has no working-directory flag, so this bridge
// cannot honour an arbitrary one — but it does not have to refuse the one it
// already provides. The value was verified live against headspace-cli 0.11.0
// (`headspace run <ws> python3 -c "import os;print(os.getcwd())"` prints
// "/workspace"), and it is the same path internal/compiler stamps on every
// code operation as DefaultWorkingDirectory (PRD §13.7's safe defaults). An
// operation asking for exactly this directory is asking for what happens
// anyway; anything else is still refused, because "silently ran somewhere
// else" is the failure mode this whole boundary exists to prevent.
//
// It is declared here rather than imported from internal/compiler on
// purpose: this is a fact about headspace-cli, not about the compiler, and
// the two agreeing is a property worth being able to lose noisily.
const JobWorkingDirectory = "/workspace"

// defaultStopTimeout bounds how long `headspace stop --apply` (see
// requestStop) or a `destroy` cleanup call is allowed to run before this
// bridge gives up waiting on it.
const defaultStopTimeout = 30 * time.Second

// BridgeConfig configures a Bridge.
type BridgeConfig struct {
	// HeadspaceBin is the headspace-cli executable to run. Empty defaults to
	// "headspace", resolved via PATH.
	HeadspaceBin string

	// Profile maps an operation's pinned execution.image_digest to the
	// headspace-cli --profile name that runs it. Required and must be
	// non-empty: headspace-cli 0.11.0 has exactly one registered profile
	// (DefaultProfilePython312), and there is no honest default mapping from
	// an arbitrary digest to it -- an unmapped digest is refused rather than
	// silently run under a profile the operation never named.
	Profile map[string]string

	// HeadspaceHome sets $HEADSPACE_HOME for every subprocess this bridge
	// launches. Empty (the default) makes Execute create a fresh temporary
	// directory for that one call and remove it when Execute returns, so
	// concurrent Execute calls and tests are hermetic and never share
	// workspace-store state. A non-empty value is used verbatim and is never
	// created or removed by this bridge -- the caller owns its lifecycle,
	// which is the shape a long-lived deployment sharing one store root
	// wants.
	HeadspaceHome string

	// Provider selects headspace-cli's --provider flag. Empty defaults to
	// "docker". "fake" selects headspace-cli's own in-memory backend, which
	// needs no Docker engine -- useful for exercising this bridge against the
	// real CLI without a container runtime, distinct from and complementary
	// to this package's own fake-*binary* unit tests (see bridge_test.go).
	Provider string

	// RunnerRevision overrides DefaultRunnerRevision. Must be a
	// "sha256:<64 hex>" digest.
	RunnerRevision string

	// Clock overrides time.Now, for tests that assert exact timings. Used
	// only as a fallback when headspace-cli's own reported timestamps cannot
	// be parsed (see buildTiming) -- headspace-cli's own clock is preferred
	// whenever it is available, because it is the thing that actually
	// measured the job.
	Clock func() time.Time

	// StopTimeout bounds `headspace stop --apply` (cancellation) and the
	// `destroy` cleanup call. Zero defaults to defaultStopTimeout.
	StopTimeout time.Duration

	// ArtifactStore, when non-nil, makes Execute persist the run's
	// captured-output excerpt (the code process's stdout, as headspace-cli
	// captured it) as a durable artifact via Store.Put, tied to the
	// operation's attempt through ArtifactMeta.AttemptID, and record the
	// returned artifact:// ref on Result.Artifacts.StdoutRef (issue #189:
	// without this, a green run discards what the process printed). Nil --
	// the default -- keeps the pre-existing local-only behaviour; see
	// stdout.go and doc.go's "Local artifact staging" section.
	ArtifactStore artifacts.Store

	// ArtifactNamespace is the namespace id every artifact this bridge
	// stores is scoped to (ArtifactMeta.NamespaceID, which the Store
	// contract requires). Required when ArtifactStore is set; ignored
	// otherwise.
	ArtifactNamespace string
}

// Bridge is the headspace-cli runners.Runner: it drives the real CLI by
// subprocess (os/exec, never a shell), one process per verb, and maps its
// nine-section result package onto a schema-valid runners.Result. See doc.go
// for the verb flow and the exit-band-to-outcome table.
type Bridge struct {
	bin               string
	profile           map[string]string
	home              string
	provider          string
	revision          string
	now               func() time.Time
	stopTimeout       time.Duration
	artifactStore     artifacts.Store
	artifactNamespace string
}

var _ runners.Runner = (*Bridge)(nil)

// New validates cfg and builds a Bridge.
func New(cfg BridgeConfig) (*Bridge, error) {
	bin := cfg.HeadspaceBin
	if bin == "" {
		bin = "headspace"
	}
	provider := cfg.Provider
	if provider == "" {
		provider = "docker"
	}
	revision := cfg.RunnerRevision
	if revision == "" {
		revision = DefaultRunnerRevision
	}
	if !strings.HasPrefix(revision, contracts.DigestPrefix) || len(revision) != len(contracts.DigestPrefix)+64 {
		return nil, fmt.Errorf("runners/headspace: runner revision %q is not a sha256 digest", revision)
	}
	if len(cfg.Profile) == 0 {
		return nil, fmt.Errorf(
			"runners/headspace: New requires at least one image-digest-to-profile mapping in BridgeConfig.Profile; "+
				"headspace-cli 0.11.0 registers only %q upstream, and a bridge with no mapping refuses every operation, "+
				"which is a misconfiguration worth failing at startup", DefaultProfilePython312)
	}
	if cfg.ArtifactStore != nil && cfg.ArtifactNamespace == "" {
		return nil, fmt.Errorf(
			"runners/headspace: New requires ArtifactNamespace when ArtifactStore is set; " +
				"the artifacts Store contract requires every Put to carry a namespace id, and a bridge that " +
				"cannot store what it captured is a misconfiguration worth failing at startup")
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	stopTimeout := cfg.StopTimeout
	if stopTimeout <= 0 {
		stopTimeout = defaultStopTimeout
	}

	profile := make(map[string]string, len(cfg.Profile))
	for k, v := range cfg.Profile {
		profile[k] = v
	}

	return &Bridge{
		bin:               bin,
		profile:           profile,
		home:              cfg.HeadspaceHome,
		provider:          provider,
		revision:          revision,
		now:               now,
		stopTimeout:       stopTimeout,
		artifactStore:     cfg.ArtifactStore,
		artifactNamespace: cfg.ArtifactNamespace,
	}, nil
}

// RunnerRevision returns the revision every result reports.
func (b *Bridge) RunnerRevision() string { return b.revision }

// Execute maps op onto a create -> put -> run -> export -> destroy verb
// sequence and returns a schema-valid Result, or a *runners.DispatchError
// when no execution happened that this bridge can honestly describe.
//
// Order, and why: every refusal this bridge can decide locally (op.Runner,
// op.RunnerRevision, execution kind, profile registration, policy fields
// headspace-cli cannot enforce, unresolved environment refs) is decided
// before any subprocess runs, so a malformed operation costs zero process
// launches. create, put, run, export, and the final destroy are then each one
// subprocess, classified by classifyVerbExit against the frozen exit band.
func (b *Bridge) Execute(ctx context.Context, op runners.Operation) (result runners.Result, err error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return runners.Result{}, &runners.DispatchError{
			Kind:        runners.ErrorCancellation,
			OperationID: op.OperationID,
			Detail:      "runners/headspace: context already done before dispatch: " + ctxErr.Error(),
		}
	}

	profile, envValues, verr := b.validate(op)
	if verr != nil {
		return runners.Result{}, verr
	}

	home, ownHome, herr := b.resolveHome()
	if herr != nil {
		return runners.Result{}, herr
	}
	if ownHome {
		defer os.RemoveAll(home)
	}

	var ws string
	defer func() {
		if ws == "" {
			return // create never produced a workspace; nothing to clean up.
		}
		note := b.cleanupWorkspace(ctx, home, ws)
		if err == nil {
			attachCleanupObservation(&result, note)
		}
	}()

	ws, err = b.create(ctx, home, op, profile)
	if err != nil {
		return runners.Result{}, err
	}

	if op.Workspace != nil {
		if perr := b.put(ctx, home, ws, op.Workspace.SourceRef); perr != nil {
			return runners.Result{}, perr
		}
	}

	result, err = b.runAndBuildResult(ctx, home, ws, op, envValues)
	if err != nil {
		return runners.Result{}, err
	}

	b.exportDeclared(ctx, home, ws, op, &result)
	return result, nil
}

// validate performs every refusal this bridge can decide without launching a
// subprocess, and resolves the two things a valid operation still needs: the
// headspace-cli profile to run under and the secret values its
// environment_refs name.
func (b *Bridge) validate(op runners.Operation) (profile string, envValues map[string]string, err error) {
	reject := func(kind runners.ErrorKind, sentinel error, detail string) error {
		return &runners.DispatchError{
			Kind:        kind,
			OperationID: op.OperationID,
			Identity:    op.Execution.ImageDigest,
			Detail:      detail,
			Err:         sentinel,
		}
	}

	if op.Runner != RunnerName {
		return "", nil, reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			fmt.Sprintf("operation names runner %q; this bridge is %q", op.Runner, RunnerName))
	}
	if op.RunnerRevision != b.revision {
		return "", nil, reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			fmt.Sprintf("operation pins runner revision %s; this bridge is %s -- "+
				"recompile the workflow against the deployed bridge, because a revision pin that is not checked is not a pin",
				op.RunnerRevision, b.revision))
	}
	if op.Execution.Kind != runners.ExecutionContainer {
		return "", nil, reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			fmt.Sprintf("execution kind %q is not %q; headspace-cli runs OCI containers, not managed functions",
				op.Execution.Kind, runners.ExecutionContainer))
	}

	profile, ok := b.profile[op.Execution.ImageDigest]
	if !ok {
		return "", nil, reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			fmt.Sprintf("image digest %s has no registered headspace-cli profile; register it in BridgeConfig.Profile "+
				"(only %q is registered upstream in headspace-cli 0.11.0)", op.Execution.ImageDigest, DefaultProfilePython312))
	}

	if op.Command.RequiresShell != nil && *op.Command.RequiresShell {
		return "", nil, reject(runners.ErrorAuthOrPolicy, runners.ErrUnsupportedOperation,
			"operation declares requires_shell; policy rejects it -- argv is executed by headspace-cli's run verb directly, never through a shell")
	}
	if len(op.Command.Argv) == 0 {
		return "", nil, reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			"operation declares no command argv")
	}
	if op.Command.WorkingDirectory != "" && op.Command.WorkingDirectory != JobWorkingDirectory {
		return "", nil, reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			fmt.Sprintf("operation sets working_directory %q; headspace-cli's run verb has no flag to set one, "+
				"and every job it runs starts in %s",
				op.Command.WorkingDirectory, JobWorkingDirectory))
	}

	if perr := checkPolicy(op.Policy, reject); perr != nil {
		return "", nil, perr
	}

	envValues, missing := resolveEnv(op.Command.EnvironmentRefs)
	if len(missing) > 0 {
		return "", nil, reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			fmt.Sprintf("environment_refs names %s, not set in this worker process's own environment; "+
				"a value must be granted to the worker before an operation can request it by name (see doc.go)",
				strings.Join(missing, ", ")))
	}

	// The operation's own run identity, forwarded LAST so it wins over any
	// same-named grant (task t16, runners.ContextEnvironment). A code node
	// that writes derived records about its run has to be able to name it,
	// and the name must come from the operation the control plane composed —
	// never from a value the executed process or a stray export could choose.
	if context := runners.ContextEnvironment(op); len(context) > 0 {
		if envValues == nil {
			envValues = make(map[string]string, len(context))
		}
		for name, value := range context {
			envValues[name] = value
		}
	}

	return profile, envValues, nil
}

// checkPolicy refuses every policy field headspace-cli 0.11.0 cannot enforce,
// mirroring internal/runners/lambda's identical discipline: a field this
// bridge cannot honour is refused, never silently ignored.
func checkPolicy(p runners.Policy, reject func(runners.ErrorKind, error, string) error) error {
	if p.TimeoutSeconds <= 0 {
		return reject(runners.ErrorRejectedInput, runners.ErrTimeoutNotEnforceable,
			"policy declares no timeout; headspace-cli's create verb requires --wall-clock-seconds to be positive")
	}
	switch p.Network {
	case runners.NetworkNone, runners.NetworkFull:
		// disabled / enabled: both are flags headspace-cli's create verb has.
	default:
		return reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			fmt.Sprintf("policy requests network %q; headspace-cli 0.11.0 supports only a disabled/enabled posture, not an egress allowlist",
				p.Network))
	}
	if len(p.EgressAllowlist) > 0 {
		return reject(runners.ErrorRejectedInput, runners.ErrUnsupportedOperation,
			"policy sets an egress_allowlist; headspace-cli has no per-workspace network allowlist to enforce it with")
	}
	return nil
}

// resolveEnv reads each named environment_ref from this bridge process's own
// environment. This is the bridge's secret-values side channel: the
// runners.Operation document (task t18's replay manifest) carries only names,
// never values, and there is no secrets-provider abstraction wired into
// runners.Runner today, so this bridge treats its own process environment as
// the value source -- exactly mirroring how a real deployment injects
// secrets into a worker process (a Kubernetes Secret mounted as env vars) and
// this boundary forwards only the named ones onward. See doc.go's "Secrets"
// section for the full reasoning and the deviation this records.
func resolveEnv(refs []string) (values map[string]string, missing []string) {
	if len(refs) == 0 {
		return nil, nil
	}
	values = make(map[string]string, len(refs))
	for _, name := range refs {
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			continue
		}
		values[name] = v
	}
	return values, missing
}

// resolveHome returns the $HEADSPACE_HOME this Execute call uses, and
// whether this bridge owns (and must remove) it.
func (b *Bridge) resolveHome() (home string, owned bool, err error) {
	if b.home != "" {
		return b.home, false, nil
	}
	dir, mkErr := os.MkdirTemp("", "nodes-headspace-*")
	if mkErr != nil {
		return "", false, &runners.DispatchError{
			Kind:   runners.ErrorRunnerUnavailable,
			Err:    runners.ErrRunnerUnavailable,
			Detail: "runners/headspace: create a per-Execute HEADSPACE_HOME: " + mkErr.Error(),
		}
	}
	return dir, true, nil
}

// attachCleanupObservation records the destroy step's outcome on an
// otherwise-already-built Result. Cleanup is best-effort and never turns a
// genuine execution result into a dispatch failure (see Execute's deferred
// cleanup and cleanupWorkspace's doc comment for why).
func attachCleanupObservation(result *runners.Result, note string) {
	if result.Observations.Additional == nil {
		result.Observations.Additional = map[string]runners.Observation{}
	}
	result.Observations.Additional["workspace_cleanup"] = runners.Observation{
		Measured: true,
		Complete: true,
		Method:   "headspace_destroy",
		Note:     note,
	}
}
