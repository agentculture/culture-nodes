package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	culturenodes "github.com/agentculture/culture-nodes"
	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/artifacts"
	artifactpg "github.com/agentculture/culture-nodes/internal/artifacts/postgres"
	artifacts3 "github.com/agentculture/culture-nodes/internal/artifacts/s3"
	"github.com/agentculture/culture-nodes/internal/auth"
	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/scheduler"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/telemetry"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// defaultListenAddr is NODES_LISTEN's default (PRD §12.1's `nodes serve`).
const defaultListenAddr = ":8080"

// defaultNamespaceSlug is the single namespace `nodes serve`/`nodes all`
// resolve at startup. Phase 1 exposes exactly one namespace over HTTP (see
// internal/api's package doc, "Single namespace"); PRD §14 still models
// namespace as a deployment boundary every row carries, so this is a
// startup convenience, not a multi-tenancy story this binary tells yet.
const defaultNamespaceSlug = "default"

// envDecisionAuthSecret is the bearer secret POST
// /v1alpha1/human-tasks/{id}/decision requires (api.WithDecisionAuthSecret).
// Unset is not an error at startup — the same "missing is not an error,
// present-and-weak is" rule cmd/nodes/worker.go's callbackSignerFromEnv
// follows for its own secret — it just means every human-task decision is
// refused with 401 until an operator sets it (see
// (*api.Server).requireDecisionAuth's doc comment for why this one endpoint
// departs from the rest of this authless-by-design API, PRD spec decision
// c45).
const envDecisionAuthSecret = "NODES_HUMAN_DECISION_TOKEN_SECRET"

// envActorRegistrationSecret is the bearer secret POST /v1alpha1/actors
// requires (api.WithActorRegistrationSecret) — task t13's registration
// lane. Deliberately its own secret, not envDecisionAuthSecret, so an
// operator can grant registration standing without also granting the power
// to decide human tasks. The same unset-is-not-an-error rule applies:
// registration is simply refused with 401 until an operator sets it.
const envActorRegistrationSecret = "NODES_ACTOR_REGISTRATION_TOKEN_SECRET"

// envEventTokenSecret is the bearer secret POST /v1alpha1/events requires
// (api.WithEventTokenSecret) — task t10's inbound signal delivery lane.
// Its own secret again, for the same separation-of-standing reason: an
// external system that may emit signal events (resuming until.signal
// waits) need not also hold decision or registration power. Unset is not
// an error: delivery is simply refused with 401 until an operator sets it.
const envEventTokenSecret = "NODES_EVENT_TOKEN_SECRET"

const (
	envJiraWebhookSecret = "NODES_JIRA_WEBHOOK_SECRET"
	envJiraWebhookToken  = "NODES_JIRA_WEBHOOK_TOKEN"
)

// envAdhocRunSecret is the bearer secret POST /v1alpha1/adhoc-runs requires
// (api.WithAdhocRunSecret) — task t19's ad-hoc lane, gated by the t15
// auth-hardening pass (spec c27). Its own secret for the same
// separation-of-standing reason as the three above: the power to start
// ad-hoc (often billable) work is granted independently. Unset is not an
// error: ad-hoc runs are simply refused with 401 until an operator sets it.
const envAdhocRunSecret = "NODES_ADHOC_RUN_TOKEN_SECRET"

// envInboundIssuanceSecret is the bearer secret POST
// /v1alpha1/inbound/credentials and .../credentials/revoke require
// (api.WithInboundIssuanceSecret) — issue #111's dial-in half, the lane that
// MINTS the credential a bridge presents when it dials out. Its own secret
// again: issuing a bridge's identity is a distinct standing from
// registering an actor or starting billable work. Unset is not an error:
// issuance and revocation are simply refused with 401 until an operator sets
// it, and a fleet that has not yet been issued credentials keeps dialling
// with whatever it already holds.
const envInboundIssuanceSecret = "NODES_INBOUND_ISSUANCE_TOKEN_SECRET"

// envStoreWriteSecret is the bearer secret the flow store's two write
// routes require — POST /v1alpha1/store/entries and POST
// /v1alpha1/store/entries/pull (api.WithStoreWriteSecret; task t7, issue
// #192). Its own secret for the same separation-of-standing reason as the
// five above: writing the mesh's flow catalog is a distinct standing from
// deciding, registering, emitting, or starting billable work. Store READS
// take no secret: the registry is an internal, mesh-private surface and
// everyone on the mesh reads. Unset is not an error: store writes are
// simply refused with 401 until an operator sets it.
const envStoreWriteSecret = "NODES_STORE_TOKEN_SECRET"

const (
	envAccessListen     = "NODES_ACCESS_LISTEN"
	envAccessTeamDomain = "NODES_ACCESS_TEAM_DOMAIN"
	envAccessAudience   = "NODES_ACCESS_AUD"
)

const (
	envArtifactS3Endpoint  = "NODES_ARTIFACT_S3_ENDPOINT"
	envArtifactS3AccessKey = "NODES_ARTIFACT_S3_ACCESS_KEY"
	envArtifactS3SecretKey = "NODES_ARTIFACT_S3_SECRET_KEY"
	envArtifactS3Bucket    = "NODES_ARTIFACT_S3_BUCKET"
	envArtifactS3UseTLS    = "NODES_ARTIFACT_S3_USE_TLS"
)

// minDecisionAuthSecretBytes mirrors actors.MinTokenSecretBytes: a secret
// short enough to guess is not meaningfully different from no secret at
// all, so it is refused at startup rather than accepted and quietly weak.
const minDecisionAuthSecretBytes = actors.MinTokenSecretBytes

// connectTimeout and shutdownTimeout bound the two edges of a serve
// process's lifecycle that must not hang forever: connecting to a database
// that never answers, and a graceful shutdown that never finishes because a
// client is still mid-request.
const (
	connectTimeout  = 30 * time.Second
	shutdownTimeout = 15 * time.Second
)

// cmdServe implements `nodes serve`: the API server alone.
func cmdServe(args []string, jsonMode bool) (int, error) {
	return runServeMode(args, "serve", false)
}

// cmdAll implements `nodes all`: serve plus the scheduler plus a worker in
// one process, for local development (PRD §12.1). Production still scales
// each role independently via its own mode.
func cmdAll(args []string, jsonMode bool) (int, error) {
	return runServeMode(args, "all", true)
}

// runServeMode is the shared body of cmdServe and cmdAll: parse flags,
// connect to PostgreSQL, resolve the default namespace, build the API
// server (and, for `all`, the scheduler), serve until SIGINT/SIGTERM, then
// shut down gracefully. It never calls os.Exit -- like every handlerFunc,
// its result flows back through run()'s dispatch.
func runServeMode(args []string, verb string, withScheduler bool) (int, error) {
	fs := newFlagSet(verb)
	listen := fs.String("listen", "", "listen address (defaults to NODES_LISTEN, then "+defaultListenAddr+")")
	databaseURL := fs.String("database-url", "", "PostgreSQL connection URL (defaults to NODES_DATABASE_URL)")
	if err := fs.Parse(args); err != nil {
		return 0, parseError(verb, err)
	}

	addr := *listen
	if addr == "" {
		addr = os.Getenv("NODES_LISTEN")
	}
	if addr == "" {
		addr = defaultListenAddr
	}

	url := *databaseURL
	if url == "" {
		url = os.Getenv("NODES_DATABASE_URL")
	}
	if url == "" {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     "no database URL configured",
			Remediation: "set NODES_DATABASE_URL or pass --database-url postgres://...",
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// task t19: env-gated (OTEL_EXPORTER_OTLP_ENDPOINT), so a deployment
	// that has not configured a collector gets telemetry.NoOp() here --
	// no exporter, no goroutine, no dial. Built once and threaded through
	// the API server (engine completion + callback ingest) and, for `nodes
	// all`, the in-process worker below, so every seam a request touches
	// shares one Provider and one graceful shutdown.
	telemetryProvider, err := telemetry.New(ctx)
	if err != nil {
		return 0, envError("building telemetry", err,
			"verify OTEL_EXPORTER_OTLP_ENDPOINT and any other OTEL_EXPORTER_OTLP_* variables, or unset them to disable export")
	}
	defer func() {
		flushCtx, cancelFlush := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancelFlush()
		if err := telemetryProvider.Shutdown(flushCtx); err != nil {
			clifmt.EmitDiagnostic(fmt.Sprintf("nodes %s: telemetry shutdown: %v", verb, err))
		}
	}()

	connectCtx, cancelConnect := context.WithTimeout(ctx, connectTimeout)
	db, err := postgres.Connect(connectCtx, url)
	cancelConnect()
	if err != nil {
		return 0, envError("connecting to database", err, "verify NODES_DATABASE_URL is reachable and credentials are correct")
	}
	defer db.Close()

	// `nodes serve`/`nodes all` deliberately do not migrate the schema
	// themselves -- docs/adr/0002-migration-policy.md's `nodes migrate` is
	// the one path that applies schema changes (e.g. a k8s
	// pre-install/pre-upgrade Job), so a server process never races a
	// migration against its own queries.
	namespaceID, err := api.EnsureNamespace(ctx, db, defaultNamespaceSlug, "Default Namespace")
	if err != nil {
		return 0, envError("resolving the default namespace", err, "verify the schema is migrated: run 'nodes migrate' first")
	}

	var opts []api.Option
	if assets, ok := culturenodes.WebAssets(); ok {
		opts = append(opts, api.WithWebAssets(assets))
	}
	opts = append(opts, api.WithTelemetry(telemetryProvider))
	accessAddr := os.Getenv(envAccessListen)
	accessDomain := os.Getenv(envAccessTeamDomain)
	accessAudience := os.Getenv(envAccessAudience)
	configuredAccess := 0
	for _, value := range []string{accessAddr, accessDomain, accessAudience} {
		if value != "" {
			configuredAccess++
		}
	}
	if configuredAccess != 0 && configuredAccess != 3 {
		return 0, envError("configuring the Access listener", errors.New("incomplete Access configuration"),
			"set NODES_ACCESS_LISTEN, NODES_ACCESS_TEAM_DOMAIN, and NODES_ACCESS_AUD together, or unset all three")
	}
	if configuredAccess == 3 {
		opts = append(opts, api.WithPrincipalVerifier(auth.New(accessDomain, accessAudience)))
	}

	// A callback token secret is optional here for the same reason it is
	// optional on the worker side (cmd/nodes/worker.go's callbackConfig): a
	// deployment that only dispatches to synchronous actors never sees a
	// callback, so nothing needs verifying. When it IS set, it must be the
	// same secret the worker signs with -- see WithCallbackSigner's doc.
	callbackSigner, err := callbackSignerFromEnv()
	if err != nil {
		return 0, err
	}
	opts = append(opts, api.WithCallbackSigner(callbackSigner))

	// Task t10: a `completed` callback that reports a handed-over ref gets the
	// ref fetched and measured. It is configured identically to the worker's
	// own observer (cmd/nodes/handover.go) so an API process and a worker
	// process reading the same environment measure from the same remote under
	// the same identity — nil, and silent, unless the deployment set both
	// variables.
	handoverObs, cliErr := handoverObserver(db, namespaceID)
	if cliErr != nil {
		return 0, cliErr
	}
	opts = append(opts, api.WithHandoverObserver(handoverObs))
	artifactRouter, err := artifactRouterFromEnv(ctx, db)
	if err != nil {
		return 0, err
	}
	opts = append(opts, api.WithArtifactRouter(artifactRouter))

	decisionAuthSecret, err := authSecretFromEnv(envDecisionAuthSecret)
	if err != nil {
		return 0, err
	}
	opts = append(opts, api.WithDecisionAuthSecret(decisionAuthSecret))

	actorRegistrationSecret, err := authSecretFromEnv(envActorRegistrationSecret)
	if err != nil {
		return 0, err
	}
	opts = append(opts, api.WithActorRegistrationSecret(actorRegistrationSecret))

	eventTokenSecret, err := authSecretFromEnv(envEventTokenSecret)
	if err != nil {
		return 0, err
	}
	opts = append(opts, api.WithEventTokenSecret(eventTokenSecret))
	opts = append(opts, api.WithJiraWebhook(
		os.Getenv(envJiraWebhookSecret), os.Getenv(envJiraWebhookToken),
		os.Getenv("JIRA_API_BASE"), os.Getenv("JIRA_SITE"), os.Getenv("JIRA_PROJECT"),
		os.Getenv("JIRA_ACCOUNT_EMAIL"), os.Getenv("JIRA_API_TOKEN"), os.Getenv("JIRA_BOT_ACCOUNT_ID"),
	))

	adhocRunSecret, err := authSecretFromEnv(envAdhocRunSecret)
	if err != nil {
		return 0, err
	}
	opts = append(opts, api.WithAdhocRunSecret(adhocRunSecret))

	inboundIssuanceSecret, err := authSecretFromEnv(envInboundIssuanceSecret)
	if err != nil {
		return 0, err
	}
	opts = append(opts, api.WithInboundIssuanceSecret(inboundIssuanceSecret))

	storeWriteSecret, err := authSecretFromEnv(envStoreWriteSecret)
	if err != nil {
		return 0, err
	}
	opts = append(opts, api.WithStoreWriteSecret(storeWriteSecret))
	// What this binary was built as, so a live test can assert which code it
	// is testing rather than assume it (task t32, issue #104).
	opts = append(opts, api.WithBuildInfo(version, revision))

	srv, err := api.NewServer(db, namespaceID, opts...)
	if err != nil {
		return 0, envError("building the API server", err, "this is an environment fault; file a bug if it persists")
	}

	httpServer := &http.Server{Addr: addr, Handler: srv.Handler()}
	serverErrs := make(chan error, 2)
	go func() {
		clifmt.EmitDiagnostic(fmt.Sprintf("nodes %s: API listening on %s", verb, addr))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrs <- err
		}
	}()
	var accessServer *http.Server
	if configuredAccess == 3 {
		accessServer = &http.Server{Addr: accessAddr, Handler: srv.AccessHandler()}
		go func() {
			clifmt.EmitDiagnostic(fmt.Sprintf("nodes %s: Access API listening on %s", verb, accessAddr))
			if err := accessServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErrs <- err
			}
		}()
	}

	var schedulerErrs chan error
	var workerErrs chan error
	if withScheduler {
		schedulerErrs = make(chan error, 1)
		sched := scheduler.New(db, scheduler.Options{Telemetry: telemetryProvider})
		go func() {
			clifmt.EmitDiagnostic(fmt.Sprintf("nodes %s: scheduler running as %s", verb, sched.OwnerID()))
			if err := sched.Run(ctx); err != nil && ctx.Err() == nil {
				schedulerErrs <- err
			}
		}()

		wk, runnerReloader, buildErr := buildWorker(db, namespaceID, telemetryProvider)
		if buildErr != nil {
			stop()
			return 0, buildErr
		}
		// task t19 (issue #8): same live-reload wk.opts.RunnerService gets
		// under `nodes worker` -- a runner-service file change takes effect
		// on the next check, no restart of this in-process worker required.
		if runnerReloader != nil {
			go runnerReloader.poll(ctx, worker.DefaultPollInterval, func(err error) {
				clifmt.EmitDiagnostic(fmt.Sprintf("nodes %s: runner-service reload: %v", verb, err))
			})
		}
		workerErrs = make(chan error, 1)
		go func() {
			clifmt.EmitDiagnostic(fmt.Sprintf("nodes %s: worker running", verb))
			if err := wk.Run(ctx); err != nil && ctx.Err() == nil {
				workerErrs <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		clifmt.EmitDiagnostic(fmt.Sprintf("nodes %s: signal received, shutting down", verb))
	case err := <-serverErrs:
		stop()
		return 0, envError("running the API server", err, "inspect the listen address and try again")
	case err := <-schedulerErrs:
		stop()
		return 0, envError("running the scheduler", err, "inspect the database connection and try again")
	case err := <-workerErrs:
		stop()
		return 0, envError("running the worker", err, "inspect the database connection and try again")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return 0, envError("shutting down the API server", err, "a client may still be mid-request; the process will now exit anyway")
	}
	if accessServer != nil {
		if err := accessServer.Shutdown(shutdownCtx); err != nil {
			return 0, envError("shutting down the Access API server", err, "a client may still be mid-request; the process will now exit anyway")
		}
	}

	clifmt.EmitDiagnostic(fmt.Sprintf("nodes %s: shut down cleanly", verb))
	return clifmt.ExitSuccess, nil
}

func artifactRouterFromEnv(ctx context.Context, db *postgres.Store) (*artifacts.Router, error) {
	small := artifactpg.New(db, artifactpg.DefaultCapBytes)
	endpoint := os.Getenv(envArtifactS3Endpoint)
	if endpoint == "" {
		// A Postgres-only installation still gets the authenticated production
		// route for small artifacts. Routing a larger body back to the capped
		// driver fails closed with ErrTooLarge rather than touching a filesystem.
		return artifacts.NewRouter(small, small, artifactpg.DefaultCapBytes), nil
	}

	accessKey := os.Getenv(envArtifactS3AccessKey)
	secretKey := os.Getenv(envArtifactS3SecretKey)
	bucket := os.Getenv(envArtifactS3Bucket)
	if accessKey == "" || secretKey == "" || bucket == "" {
		return nil, envError("configuring artifact storage", errors.New("incomplete NODES_ARTIFACT_S3_* configuration"),
			"set endpoint, access key, secret key, and bucket together, or unset the endpoint for Postgres-only artifacts")
	}
	useTLS := false
	if raw := os.Getenv(envArtifactS3UseTLS); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, envError("configuring artifact storage", err, "set NODES_ARTIFACT_S3_USE_TLS to true or false")
		}
		useTLS = parsed
	}
	object, err := artifacts3.New(ctx, artifacts3.Config{
		Endpoint: endpoint, AccessKey: accessKey, SecretKey: secretKey, Bucket: bucket, UseTLS: useTLS,
	}, db)
	if err != nil {
		return nil, envError("connecting artifact object storage", err, "verify NODES_ARTIFACT_S3_* and MinIO/S3 reachability")
	}
	return artifacts.NewRouter(small, object, artifactpg.DefaultCapBytes), nil
}

// authSecretFromEnv reads an optional bearer secret from envName (the
// human-task decision secret and the actor-registration secret share this
// one rule). An unset secret is not an error — it just means the endpoint
// it gates refuses every request with 401 (see api.WithDecisionAuthSecret,
// api.WithActorRegistrationSecret) — but a secret that is present and
// shorter than minDecisionAuthSecretBytes looks configured and is not, so
// that is.
func authSecretFromEnv(envName string) (string, error) {
	secret := os.Getenv(envName)
	if secret == "" {
		return "", nil
	}
	if len(secret) < minDecisionAuthSecretBytes {
		return "", &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("%s is set but only %d bytes long", envName, len(secret)),
			Remediation: fmt.Sprintf("set %s to at least %d bytes of random data", envName, minDecisionAuthSecretBytes),
		}
	}
	return secret, nil
}

// envError builds a CliError in the environment-error bucket for a
// serve-mode setup/runtime failure, matching runMigrate's own
// error:/hint: shape (cmd/nodes/migrate.go) for consistency across this
// binary's long-running commands.
func envError(doing string, cause error, remediation string) *clifmt.CliError {
	return &clifmt.CliError{
		Code:        clifmt.ExitEnvError,
		Message:     fmt.Sprintf("%s: %v", doing, cause),
		Remediation: remediation,
	}
}
