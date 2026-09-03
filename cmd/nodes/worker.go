package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/telemetry"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// `nodes worker`: the process that claims ready work and dispatches it
// (PRD §12.1's worker role).
//
// The flag surface is deliberately small. Everything that has a sensible
// default has one in internal/worker, and a process-lifecycle command whose
// help text is thirty flags long is a command nobody reads. What is here is
// what a deployment genuinely has to state: where the database is, which
// namespace this worker serves, and — for asynchronous actors — how the
// outside world reaches this installation's callback endpoint.

// Environment variables `nodes worker` reads. Configuration comes from the
// environment rather than from flags for the secret-bearing values (§16.3),
// because a secret on a command line is visible in every process listing.
const (
	envDatabaseURL      = "NODES_DATABASE_URL"
	envNamespace        = "NODES_NAMESPACE_ID"
	envNamespaceSlug    = "NODES_NAMESPACE_SLUG"
	envCallbackBaseURL  = "NODES_CALLBACK_BASE_URL"
	envCallbackSecret   = "NODES_CALLBACK_TOKEN_SECRET"
	envWorkerIdentifier = "NODES_WORKER_ID"
	// envCodeRunnerName is the logical runner name stamped on every code
	// operation this worker builds (worker.Options.CodeRunnerName). Any code
	// dispatch — in-process adapter or runner service alike — refuses without
	// one: an operation that names no runner is one no adapter will accept.
	envCodeRunnerName = "NODES_CODE_RUNNER_NAME"
	// envCodeRunnerRevision pins the runner build revision stamped on every
	// code operation (worker.Options.CodeRunnerRevision); the runner service
	// validates the operation document against it.
	envCodeRunnerRevision = "NODES_CODE_RUNNER_REVISION"
	// envCodeRunnerActorID names the REGISTERED actors-table row the runner's
	// observed evidence is attributed to (worker.Options.CodeRunnerActorID).
	// The actor-identity contract makes registration a deployment
	// prerequisite: attempts.actor_id is a foreign key.
	envCodeRunnerActorID = "NODES_CODE_RUNNER_ACTOR_ID"
	// envRemintProducerActorID names the registered engine identity used for
	// derived trigger re-mint records. The stable default is registered by the
	// production deploy before workers can encounter a due re-mint.
	envRemintProducerActorID = "NODES_REMINT_PRODUCER_ACTOR_ID"
)

func cmdWorker(args []string, jsonMode bool) (int, error) {
	fs := newFlagSet("worker")
	databaseURL := fs.String("database-url", "", "PostgreSQL connection URL (defaults to "+envDatabaseURL+")")
	namespaceID := fs.String("namespace", "", "namespace this worker serves, by id (defaults to "+envNamespace+")")
	namespaceSlug := fs.String("namespace-slug", "", "namespace this worker serves, by slug: resolved (and created if absent) the same way 'nodes serve' resolves its own namespace -- ignored when --namespace/"+envNamespace+" is set (defaults to "+envNamespaceSlug+")")
	batch := fs.Int("batch", worker.DefaultClaimBatch, "how many work items one claim pass takes")
	pollInterval := fs.Duration("poll-interval", worker.DefaultPollInterval, "how long an idle worker waits before claiming again")
	leaseDuration := fs.Duration("lease", worker.DefaultLeaseDuration, "how long a claim is held before it can be reclaimed")
	if err := fs.Parse(args); err != nil {
		return 0, parseError("worker", err)
	}

	url := firstNonEmpty(*databaseURL, os.Getenv(envDatabaseURL))
	if url == "" {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     "no database URL configured",
			Remediation: "set " + envDatabaseURL + " or pass --database-url postgres://...",
		}
	}
	// A worker's namespace can be named directly by id, or by slug -- the
	// latter resolved (and created if this is the first process to ask)
	// through the identical idempotent lookup cmd/nodes/serve.go's
	// EnsureNamespace call uses. This exists because a namespace id is a
	// database-generated ULID (internal/store/postgres.Store.CreateNamespace),
	// not something a Helm chart's values.yaml can pin ahead of time: a
	// worker Deployment names the same namespace slug the api Deployment
	// serves ("default" in deploy/helm's values), and whichever process
	// asks first creates the row; every process after that gets the same id
	// back. See EnsureNamespace's own doc comment for the idempotency
	// argument.
	namespace := firstNonEmpty(*namespaceID, os.Getenv(envNamespace))
	slug := firstNonEmpty(*namespaceSlug, os.Getenv(envNamespaceSlug))
	if namespace == "" && slug == "" {
		return 0, &clifmt.CliError{
			Code:    clifmt.ExitUserError,
			Message: "no namespace configured",
			Remediation: "set " + envNamespace + " (or --namespace) to a namespace id, or " +
				envNamespaceSlug + " (or --namespace-slug) to a slug to resolve/create",
		}
	}
	if cliErr := workerConfigPreflight(); cliErr != nil {
		return 0, cliErr
	}

	ctx, stop := shutdownContext()
	defer stop()

	// task t19: env-gated (OTEL_EXPORTER_OTLP_ENDPOINT); telemetry.New
	// returns a safe no-op Provider when it is unset, so a standalone
	// `nodes worker` process with no collector configured dispatches
	// exactly as it did before this instrumentation existed.
	telemetryProvider, err := telemetry.New(ctx)
	if err != nil {
		return 0, &clifmt.CliError{
			Code:    clifmt.ExitEnvError,
			Message: fmt.Sprintf("building telemetry: %v", err),
			Remediation: "verify OTEL_EXPORTER_OTLP_ENDPOINT and any other OTEL_EXPORTER_OTLP_* " +
				"variables, or unset them to disable export",
		}
	}
	defer func() {
		flushCtx, cancelFlush := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancelFlush()
		if err := telemetryProvider.Shutdown(flushCtx); err != nil {
			clifmt.EmitDiagnostic(fmt.Sprintf("nodes worker: telemetry shutdown: %v", err))
		}
	}()

	db, err := postgres.Connect(ctx, url)
	if err != nil {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("connecting to the database: %v", err),
			Remediation: "verify " + envDatabaseURL + " is reachable and the credentials are correct",
		}
	}
	defer db.Close()

	if namespace == "" {
		namespace, err = api.EnsureNamespace(ctx, db, slug, slug)
		if err != nil {
			return 0, &clifmt.CliError{
				Code:        clifmt.ExitEnvError,
				Message:     fmt.Sprintf("resolving namespace slug %q: %v", slug, err),
				Remediation: "run 'nodes migrate' and verify the database is reachable",
			}
		}
	}

	eng, err := postgres.NewEngine(db, namespace, engine.WithTelemetry(telemetryProvider))
	if err != nil {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("building the engine: %v", err),
			Remediation: "run 'nodes migrate' and verify the namespace exists",
		}
	}
	registry, err := worker.NewDBRegistry(db, namespace)
	if err != nil {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("building the actor registry: %v", err),
			Remediation: "verify the namespace exists and the actors table is populated",
		}
	}

	// The callback signer is optional, and a worker without one says so
	// rather than silently dispatching to asynchronous actors it could never
	// hear back from.
	signer, callbackBase, err := callbackConfig()
	if err != nil {
		return 0, err
	}
	runnerSvc, runnerReloader, cliErr := runnerServiceConfig()
	if cliErr != nil {
		return 0, cliErr
	}
	// Task t10: the declared dispatch rates. Absent configuration is no
	// pacing; malformed configuration refuses to start rather than silently
	// dispatching unpaced (see pacing.go).
	pacingOpts, cliErr := pacingConfig()
	if cliErr != nil {
		return 0, cliErr
	}
	// Task t16: the declared per-actor concurrency ceilings. Same failure
	// posture as pacing: absent configuration is no ceiling, malformed
	// configuration refuses to start (see concurrency.go).
	concurrencyOpts, cliErr := concurrencyConfig()
	if cliErr != nil {
		return 0, cliErr
	}

	// Task t10: measure a handed-over ref, when the deployment named a
	// remote to fetch from and an identity to attribute the observation to.
	// Nil — the default — records nothing at all.
	handoverObs, cliErr := handoverObserver(db, namespace)
	if cliErr != nil {
		return 0, cliErr
	}
	hostname, err := os.Hostname()
	if err != nil {
		return 0, &clifmt.CliError{Code: clifmt.ExitEnvError, Message: fmt.Sprintf("reading worker hostname: %v", err)}
	}

	wk, err := worker.New(db, eng, worker.Options{
		WorkerID:              os.Getenv(envWorkerIdentifier),
		Hostname:              hostname,
		Revision:              revision,
		ActorKeys:             configuredActorKeyNames(),
		Handover:              handoverObs,
		Pacing:                pacingOpts,
		Concurrency:           concurrencyOpts,
		NamespaceID:           namespace,
		ClaimBatch:            *batch,
		LeaseDuration:         *leaseDuration,
		PollInterval:          *pollInterval,
		Registry:              registry,
		Signer:                signer,
		CallbackBaseURL:       callbackBase,
		RunnerService:         runnerSvc,
		CodeRunnerName:        os.Getenv(envCodeRunnerName),
		CodeRunnerRevision:    os.Getenv(envCodeRunnerRevision),
		CodeRunnerActorID:     os.Getenv(envCodeRunnerActorID),
		RemintProducerActorID: remintProducerActorIDFromEnv(),
		Telemetry:             telemetryProvider,
		OnError: func(err error) {
			// Diagnostics go to stderr; the stdout stream stays clean for
			// results, per the CLI's output contract.
			clifmt.EmitDiagnostic(err.Error())
		},
	})
	if err != nil {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("building the worker: %v", err),
			Remediation: "check the namespace and database configuration",
		}
	}

	// task t19 (issue #8): a registered runner-service file change takes
	// effect on this worker's next reload check, no restart required. The
	// checker shares the same registry and secrets wk was just built with, so
	// there is nothing to hand back to wk once it starts -- reloading mutates
	// state wk already holds a reference to. It stops with the worker, on the
	// same ctx.
	if runnerReloader != nil {
		go runnerReloader.poll(ctx, *pollInterval, func(err error) {
			clifmt.EmitDiagnostic(fmt.Sprintf("nodes worker: runner-service reload: %v", err))
		})
	}

	startup := map[string]any{
		"mode":                     "worker",
		"worker_id":                wk.ID(),
		"namespace_id":             namespace,
		"batch":                    *batch,
		"poll_interval":            pollInterval.String(),
		"lease":                    leaseDuration.String(),
		"callback_base_url":        callbackBase,
		"async_capable":            signer != nil && callbackBase != "",
		"runner_services_reloaded": runnerReloader != nil,
	}
	if jsonMode {
		if err := clifmt.EmitResultJSON(startup); err != nil {
			return 0, err
		}
	} else {
		clifmt.EmitResult(fmt.Sprintf(
			"worker %s serving namespace %s (batch %d, poll %s, lease %s, async %v)\npress Ctrl-C or send SIGTERM to stop",
			wk.ID(), namespace, *batch, *pollInterval, *leaseDuration, startup["async_capable"]))
	}

	// Run returns only when the context ends, which for this process means a
	// signal. A clean shutdown is exit 0: the operator asked for it.
	if err := wk.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("worker stopped: %v", err),
			Remediation: "check the database is reachable and re-start the worker",
		}
	}
	return clifmt.ExitSuccess, nil
}

// workerConfigPreflight validates deployment-level environment as one unit
// before telemetry or PostgreSQL is touched. Code runner identity is a tuple:
// accepting an absent or partial tuple lets a worker start but guarantees that
// its first code dispatch cannot produce correctly attributed evidence.
func workerConfigPreflight() *clifmt.CliError {
	var problems []string
	// All three or none, which is the same rule the callback pair below
	// follows and for the same reason. A PARTIAL tuple is the dangerous
	// state: the worker starts and its first code dispatch produces evidence
	// attributed to an identity nobody fully declared. A tuple that is absent
	// ENTIRELY says something different and legitimate — this deployment runs
	// no code nodes at all, which is exactly the Helm chart's smoke
	// configuration and every deployment that only dispatches agents.
	//
	// Demanding all three unconditionally broke that: the chart sets none of
	// them, so every worker pod CrashLoopBackOff'd on a check meant to catch
	// misattribution. Same shape as #124 — a fail-closed validation added
	// without walking the deployment paths it would newly refuse.
	var runnerSet, runnerMissing []string
	for _, name := range []string{envCodeRunnerName, envCodeRunnerRevision, envCodeRunnerActorID} {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			runnerMissing = append(runnerMissing, name)
		} else {
			runnerSet = append(runnerSet, name)
		}
	}
	if len(runnerSet) > 0 && len(runnerMissing) > 0 {
		for _, name := range runnerMissing {
			problems = append(problems, name+" is missing while "+runnerSet[0]+" is set")
		}
	}
	callbackURL := strings.TrimSpace(os.Getenv(envCallbackBaseURL))
	callbackSecret := strings.TrimSpace(os.Getenv(envCallbackSecret))
	if callbackURL == "" && callbackSecret != "" {
		problems = append(problems, envCallbackBaseURL+" is missing while "+envCallbackSecret+" is set")
	}
	if callbackURL != "" && callbackSecret == "" {
		problems = append(problems, envCallbackSecret+" is missing while "+envCallbackBaseURL+" is set")
	}
	if len(problems) == 0 {
		return nil
	}
	return &clifmt.CliError{
		Code:        clifmt.ExitEnvError,
		Message:     "worker deployment configuration is invalid: " + strings.Join(problems, "; "),
		Remediation: "set the complete NODES_CODE_RUNNER_{NAME,REVISION,ACTOR_ID} tuple and either both or neither NODES_CALLBACK_{BASE_URL,TOKEN_SECRET}",
	}
}

// callbackConfig builds the attempt-scoped token signer from the environment.
//
// A missing secret is not an error: a deployment that only runs synchronous
// actors has no callbacks to authenticate, and demanding a secret it will
// never use would be configuration for its own sake. What IS an error is a
// secret that is present but too weak, because that looks configured and is
// not.
func callbackConfig() (*actors.TokenSigner, string, error) {
	base := os.Getenv(envCallbackBaseURL)
	signer, err := callbackSignerFromEnv()
	if err != nil {
		return nil, "", err
	}
	if signer == nil {
		return nil, base, nil
	}
	if base == "" {
		return nil, "", &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     "a callback token secret is set but no callback base URL is",
			Remediation: "set " + envCallbackBaseURL + " to this installation's externally reachable base URL",
		}
	}
	return signer, base, nil
}

// callbackSignerFromEnv builds the attempt-scoped token signer from
// NODES_CALLBACK_TOKEN_SECRET alone -- the half of callbackConfig that does
// not depend on a callback base URL, and therefore the part cmd/nodes/serve.go
// shares: the API process verifies callback tokens but never mints a
// callback.url itself, so it has no use for envCallbackBaseURL. Returns a
// nil signer, nil error when the secret is unset -- not an error, for the
// same reason callbackConfig's doc comment gives.
func callbackSignerFromEnv() (*actors.TokenSigner, error) {
	raw := os.Getenv(envCallbackSecret)
	if raw == "" {
		return nil, nil
	}

	secret := []byte(raw)
	// A base64 secret is accepted so an operator can inject random bytes
	// through an environment variable without worrying about encoding.
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil && len(decoded) >= actors.MinTokenSecretBytes {
		secret = decoded
	}

	signer, err := actors.NewTokenSigner(secret)
	if err != nil {
		return nil, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("callback token secret is unusable: %v", err),
			Remediation: fmt.Sprintf("set %s to at least %d bytes of random data", envCallbackSecret, actors.MinTokenSecretBytes),
		}
	}
	return signer, nil
}

// shutdownContext returns a context cancelled on SIGINT or SIGTERM, so a
// container stop drains rather than kills. Draining matters here: a worker
// killed mid-dispatch leaves a leased work item that nothing can complete
// until its lease expires (§20.4), which is recoverable but slow.
func shutdownContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func remintProducerActorIDFromEnv() string {
	return firstNonEmpty(os.Getenv(envRemintProducerActorID), postgres.RemintSchedulerActorID)
}

// Compile-time proof the flag defaults stay in the same units the worker
// expects. A duration flag silently redefined as an int would otherwise only
// show up at runtime.
var (
	_ time.Duration = worker.DefaultPollInterval
	_ time.Duration = worker.DefaultLeaseDuration
)

const explainWorker = `# nodes worker

Runs the worker process: claim ready work, resolve each node's input
bindings, dispatch it (an actor over the PRD §13 HTTP protocol, or a
decision evaluated in-process), and report the result through the engine's
§12.5 completion transaction.

An actor that answers 202 releases the claim: the work item is parked in
the ` + "`waiting`" + ` state and the completion arrives later through the
callback endpoint (§12.6), so the worker holds no lease and no goroutine
for a long-running agent.

The worker does not serve that callback endpoint — the API process does.
NODES_CALLBACK_BASE_URL is the address actors should reach it on, and a
deployment that runs workers without an API reachable from its actors can
only use actors that answer synchronously.

## Configuration

    NODES_DATABASE_URL           PostgreSQL connection URL (required)
    NODES_NAMESPACE_ID           namespace this worker serves, by id
    NODES_NAMESPACE_SLUG         namespace this worker serves, by slug
    NODES_CALLBACK_BASE_URL      externally reachable base URL for callbacks
    NODES_CALLBACK_TOKEN_SECRET  HMAC secret for attempt-scoped tokens
    NODES_WORKER_ID              lease owner identity (defaults to a ULID)
    OTEL_EXPORTER_OTLP_ENDPOINT  OTLP collector endpoint; unset disables all
                                  tracing/metrics export (no exporter, no
                                  goroutine, no dial -- see internal/telemetry)

Exactly one of NODES_NAMESPACE_ID or NODES_NAMESPACE_SLUG is required. A
slug is resolved to an id (creating the namespace if this is the first
process to ask) through the same idempotent lookup 'nodes serve' uses for
its own namespace -- a namespace id is a database-generated ULID nothing
outside the database can predict ahead of time, so a deployment that starts
its api and worker processes independently (a Helm chart's separate
Deployments, for instance) names them by the same slug, not the same id.

Without a callback secret and base URL the worker still runs, but it can
only dispatch to actors that answer synchronously.

## Usage

    nodes worker --namespace-slug default
    nodes worker --namespace ns_01J --batch 8 --poll-interval 500ms
    nodes worker --json

Stops cleanly on SIGINT or SIGTERM.
`

// buildWorker wires a Worker with default options for `nodes all`'s
// in-process dev worker, mirroring cmdWorker's own wiring (engine, DB
// registry, optional callback signer) without its flag surface. telemetry
// may be nil (a safe no-op, see internal/telemetry.Provider's doc); when
// set, it instruments both this worker's dispatch seam and the engine
// instance it drives, so a node completed by this in-process worker emits
// the same engine-transition telemetry the API server's own engine would.
func buildWorker(db *postgres.Store, namespace string, telemetryProvider *telemetry.Provider) (*worker.Worker, *runnerServiceReloader, *clifmt.CliError) {
	eng, err := postgres.NewEngine(db, namespace, engine.WithTelemetry(telemetryProvider))
	if err != nil {
		return nil, nil, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("building the engine: %v", err),
			Remediation: "run 'nodes migrate' and verify the namespace exists",
		}
	}
	registry, err := worker.NewDBRegistry(db, namespace)
	if err != nil {
		return nil, nil, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("building the actor registry: %v", err),
			Remediation: "verify the namespace exists and the actors table is populated",
		}
	}
	signer, callbackBase, err := callbackConfig()
	if err != nil {
		var ce *clifmt.CliError
		if !errors.As(err, &ce) {
			ce = &clifmt.CliError{
				Code:        clifmt.ExitEnvError,
				Message:     err.Error(),
				Remediation: "check the NODES_CALLBACK_* environment variables",
			}
		}
		return nil, nil, ce
	}
	runnerSvc, runnerReloader, cliErr := runnerServiceConfig()
	if cliErr != nil {
		return nil, nil, cliErr
	}
	pacingOpts, cliErr := pacingConfig()
	if cliErr != nil {
		return nil, nil, cliErr
	}
	concurrencyOpts, cliErr := concurrencyConfig()
	if cliErr != nil {
		return nil, nil, cliErr
	}
	handoverObs, cliErr := handoverObserver(db, namespace)
	if cliErr != nil {
		return nil, nil, cliErr
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, nil, &clifmt.CliError{Code: clifmt.ExitEnvError, Message: fmt.Sprintf("reading worker hostname: %v", err)}
	}

	wk, err := worker.New(db, eng, worker.Options{
		NamespaceID:        namespace,
		Hostname:           hostname,
		Revision:           revision,
		ActorKeys:          configuredActorKeyNames(),
		Handover:           handoverObs,
		Pacing:             pacingOpts,
		Concurrency:        concurrencyOpts,
		Registry:           registry,
		Signer:             signer,
		CallbackBaseURL:    callbackBase,
		RunnerService:      runnerSvc,
		CodeRunnerName:     os.Getenv(envCodeRunnerName),
		CodeRunnerRevision: os.Getenv(envCodeRunnerRevision),
		CodeRunnerActorID:  os.Getenv(envCodeRunnerActorID),
		Telemetry:          telemetryProvider,
		OnError: func(err error) {
			clifmt.EmitDiagnostic(err.Error())
		},
	})
	if err != nil {
		return nil, nil, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("building the worker: %v", err),
			Remediation: "this is an environment fault; file a bug if it persists",
		}
	}
	return wk, runnerReloader, nil
}

// configuredActorKeyNames reports only configuration names. Values are never
// copied into worker presence, even transiently through this return value.
func configuredActorKeyNames() []string {
	var names []string
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if ok && value != "" && strings.HasPrefix(name, "NODES_ACTOR_") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
