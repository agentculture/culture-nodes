package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	culturenodes "github.com/agentculture/culture-nodes"
	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/scheduler"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
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

	decisionAuthSecret, err := decisionAuthSecretFromEnv()
	if err != nil {
		return 0, err
	}
	opts = append(opts, api.WithDecisionAuthSecret(decisionAuthSecret))

	srv, err := api.NewServer(db, namespaceID, opts...)
	if err != nil {
		return 0, envError("building the API server", err, "this is an environment fault; file a bug if it persists")
	}

	httpServer := &http.Server{Addr: addr, Handler: srv.Handler()}
	serverErrs := make(chan error, 1)
	go func() {
		clifmt.EmitDiagnostic(fmt.Sprintf("nodes %s: API listening on %s", verb, addr))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrs <- err
		}
	}()

	var schedulerErrs chan error
	var workerErrs chan error
	if withScheduler {
		schedulerErrs = make(chan error, 1)
		sched := scheduler.New(db, scheduler.Options{})
		go func() {
			clifmt.EmitDiagnostic(fmt.Sprintf("nodes %s: scheduler running as %s", verb, sched.OwnerID()))
			if err := sched.Run(ctx); err != nil && ctx.Err() == nil {
				schedulerErrs <- err
			}
		}()

		wk, buildErr := buildWorker(db, namespaceID)
		if buildErr != nil {
			stop()
			return 0, buildErr
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

	clifmt.EmitDiagnostic(fmt.Sprintf("nodes %s: shut down cleanly", verb))
	return clifmt.ExitSuccess, nil
}

// decisionAuthSecretFromEnv reads the human-task decision bearer secret from
// NODES_HUMAN_DECISION_TOKEN_SECRET. An unset secret is not an error — it
// just means every decision is refused with 401 (see
// api.WithDecisionAuthSecret) — but a secret that is present and shorter
// than minDecisionAuthSecretBytes looks configured and is not, so that is.
func decisionAuthSecretFromEnv() (string, error) {
	secret := os.Getenv(envDecisionAuthSecret)
	if secret == "" {
		return "", nil
	}
	if len(secret) < minDecisionAuthSecretBytes {
		return "", &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("%s is set but only %d bytes long", envDecisionAuthSecret, len(secret)),
			Remediation: fmt.Sprintf("set %s to at least %d bytes of random data", envDecisionAuthSecret, minDecisionAuthSecretBytes),
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
