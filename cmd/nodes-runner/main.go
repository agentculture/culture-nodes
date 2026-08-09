// Command nodes-runner is the reference api/runner-protocol runner service:
// the headspace-cli execution bridge behind the protocol's HTTP surface, with
// mandatory bearer authentication.
//
// It is a separate binary from `nodes` on purpose. The control plane never
// executes code — PRD §13.7's whole boundary — so the process that does is a
// different process, with a different deployment, a different credential, and
// no database connection at all. Running it in the same binary would make
// "the control plane cannot execute code" a claim about discipline rather
// than about topology.
//
// Configuration is environment-first (§16.3): the bearer secret has no flag,
// because a secret on a command line is visible in every process listing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/runners/headspace"
	"github.com/agentculture/culture-nodes/internal/runners/runnerservice"
)

// version is this binary's own version. Like cmd/nodes, the Go side has no
// CI-enforced versioning scheme yet (only pyproject.toml's is), so this is a
// placeholder rather than a promise.
const version = "0.1.0-dev"

// Environment variables nodes-runner reads. Every one has a flag except the
// secret, which deliberately does not.
const (
	envListen        = "NODES_RUNNER_LISTEN"
	envSecret        = "NODES_RUNNER_SECRET"
	envSecretFile    = "NODES_RUNNER_SECRET_FILE"
	envStateDir      = "NODES_RUNNER_STATE_DIR"
	envEphemeral     = "NODES_RUNNER_EPHEMERAL_STATE"
	envConcurrency   = "NODES_RUNNER_CONCURRENCY"
	envQueueDepth    = "NODES_RUNNER_QUEUE_DEPTH"
	envRetention     = "NODES_RUNNER_STATUS_RETENTION"
	envPollAfter     = "NODES_RUNNER_POLL_AFTER"
	envHeadspaceBin  = "NODES_RUNNER_HEADSPACE_BIN"
	envHeadspaceHome = "NODES_RUNNER_HEADSPACE_HOME"
	envProvider      = "NODES_RUNNER_HEADSPACE_PROVIDER"
	envProfiles      = "NODES_RUNNER_HEADSPACE_PROFILES"
	envStopTimeout   = "NODES_RUNNER_HEADSPACE_STOP_TIMEOUT"
)

const defaultListenAddr = ":8090"

// shutdownTimeout bounds the HTTP server's graceful shutdown. The runner
// service's own Close() drains the worker pool separately.
const shutdownTimeout = 15 * time.Second

var usageText = `nodes-runner - Culture Nodes reference runner service

Runs headspace-cli behind api/runner-protocol: POST /v1/operations to
dispatch, GET /v1/operations/{id} to learn the outcome. Every request must
carry Authorization: Bearer <secret>; there is no loopback exemption.

Usage:
  nodes-runner [flags]

Flags:
  --listen ADDR          listen address (default ` + defaultListenAddr + `)
  --state-dir DIR        directory holding per-operation status durably
  --ephemeral-state      keep status in memory only, accepting that a
                         restart forgets every operation (explicit opt-in)
  --concurrency N        operations executing at once
  --queue-depth N        accepted operations awaiting a worker
  --retention DURATION   how long a terminal status stays readable
                         (minimum ` + runners.MinStatusRetention.String() + `)
  --poll-after DURATION  sampling interval acceptances ask for
  --headspace-bin PATH   headspace-cli executable
  --headspace-provider P headspace-cli provider (docker, fake)
  --profile D=P          image digest to headspace profile, repeatable
  --stop-timeout D       bound on headspace stop/destroy
  --json                 emit machine-readable output
  --version              print the version and exit

Environment (flags win where both exist):
  ` + envSecret + `          bearer secret every request must present
  ` + envSecretFile + `     file holding that secret
  ` + envListen + `          listen address
  ` + envStateDir + `       status state directory
  ` + envEphemeral + ` set to 1 to accept in-memory status
  ` + envConcurrency + `     operations executing at once
  ` + envQueueDepth + `      accepted operations awaiting a worker
  ` + envRetention + ` terminal-status retention
  ` + envPollAfter + `      requested sampling interval
  ` + envHeadspaceBin + `   headspace-cli executable
  ` + envHeadspaceHome + `  $HEADSPACE_HOME for every subprocess
  ` + envProvider + ` headspace-cli provider
  ` + envProfiles + ` comma-separated digest=profile pairs
  ` + envStopTimeout + ` bound on headspace stop/destroy

The secret has no flag on purpose: a secret on a command line is visible in
every process listing on the host.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

// run parses, resolves, and serves. It never calls os.Exit, so it stays
// unit-testable, and it routes every failure through clifmt's CliError so the
// error:/hint: contract holds here exactly as it does in cmd/nodes.
func run(argv []string) int {
	rest, jsonMode := clifmt.StripJSONFlag(argv)
	if len(rest) > 0 && (rest[0] == "-h" || rest[0] == "--help") {
		clifmt.EmitResult(usageText)
		return clifmt.ExitSuccess
	}
	if len(rest) > 0 && (rest[0] == "-version" || rest[0] == "--version") {
		clifmt.EmitResult(version)
		return clifmt.ExitSuccess
	}

	if cliErr := clifmt.Guard(func() error { return serve(rest, jsonMode) }); cliErr != nil {
		_ = clifmt.EmitError(cliErr, jsonMode)
		return cliErr.Code
	}
	return clifmt.ExitSuccess
}

// profileFlag collects repeated --profile digest=profile pairs.
type profileFlag map[string]string

func (p profileFlag) String() string { return "" }

func (p profileFlag) Set(value string) error {
	digest, profile, ok := strings.Cut(value, "=")
	if !ok || strings.TrimSpace(digest) == "" || strings.TrimSpace(profile) == "" {
		return fmt.Errorf("expected DIGEST=PROFILE (e.g. sha256:...=%s), got %q",
			headspace.DefaultProfilePython312, value)
	}
	p[strings.TrimSpace(digest)] = strings.TrimSpace(profile)
	return nil
}

// settings is the resolved configuration, separated from serve so it can be
// tested without opening a socket.
type settings struct {
	listen          string
	secret          string
	stateDir        string
	ephemeral       bool
	concurrency     int
	queueDepth      int
	retention       time.Duration
	pollAfter       time.Duration
	headspaceBin    string
	headspaceHome   string
	provider        string
	profiles        profileFlag
	stopTimeout     time.Duration
	durabilityLimit string
}

//nolint:gocyclo // flag/env resolution is a flat list of independent fields
func resolve(args []string) (settings, *clifmt.CliError) {
	fs := flag.NewFlagSet("nodes-runner", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	listen := fs.String("listen", "", "listen address (defaults to "+envListen+", then "+defaultListenAddr+")")
	stateDir := fs.String("state-dir", "", "directory holding per-operation status durably (defaults to "+envStateDir+")")
	ephemeral := fs.Bool("ephemeral-state", false, "keep status in memory only, accepting that a restart forgets it")
	concurrency := fs.Int("concurrency", 0, "operations executing at once (defaults to "+envConcurrency+")")
	queueDepth := fs.Int("queue-depth", 0, "accepted operations awaiting a worker (defaults to "+envQueueDepth+")")
	retention := fs.Duration("retention", 0, "terminal-status retention (defaults to "+envRetention+")")
	pollAfter := fs.Duration("poll-after", 0, "requested sampling interval (defaults to "+envPollAfter+")")
	headspaceBin := fs.String("headspace-bin", "", "headspace-cli executable (defaults to "+envHeadspaceBin+")")
	provider := fs.String("headspace-provider", "", "headspace-cli provider (defaults to "+envProvider+")")
	stopTimeout := fs.Duration("stop-timeout", 0, "bound on headspace stop/destroy (defaults to "+envStopTimeout+")")
	profiles := profileFlag{}
	fs.Var(profiles, "profile", "image digest to headspace profile, repeatable (DIGEST=PROFILE)")

	if err := fs.Parse(args); err != nil {
		return settings{}, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     err.Error(),
			Remediation: "run 'nodes-runner --help' to see usage",
		}
	}

	resolved := settings{
		listen:        firstNonEmpty(*listen, os.Getenv(envListen), defaultListenAddr),
		stateDir:      firstNonEmpty(*stateDir, os.Getenv(envStateDir)),
		ephemeral:     *ephemeral || truthy(os.Getenv(envEphemeral)),
		headspaceBin:  firstNonEmpty(*headspaceBin, os.Getenv(envHeadspaceBin)),
		headspaceHome: os.Getenv(envHeadspaceHome),
		provider:      firstNonEmpty(*provider, os.Getenv(envProvider)),
		profiles:      profiles,
	}

	secret, cliErr := resolveSecret()
	if cliErr != nil {
		return settings{}, cliErr
	}
	resolved.secret = secret

	if cliErr := resolveDurability(&resolved); cliErr != nil {
		return settings{}, cliErr
	}
	if cliErr := resolveNumbers(&resolved, *concurrency, *queueDepth); cliErr != nil {
		return settings{}, cliErr
	}
	if cliErr := resolveDurations(&resolved, *retention, *pollAfter, *stopTimeout); cliErr != nil {
		return settings{}, cliErr
	}
	if cliErr := resolveProfiles(&resolved); cliErr != nil {
		return settings{}, cliErr
	}
	return resolved, nil
}

// resolveSecret reads the bearer secret from the environment, preferring a
// file so a Kubernetes Secret can be mounted rather than injected.
func resolveSecret() (string, *clifmt.CliError) {
	if path := os.Getenv(envSecretFile); path != "" {
		raw, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own configuration
		if err != nil {
			return "", &clifmt.CliError{
				Code:        clifmt.ExitEnvError,
				Message:     fmt.Sprintf("reading %s: %v", envSecretFile, err),
				Remediation: "point " + envSecretFile + " at a readable file holding the bearer secret",
			}
		}
		if secret := strings.TrimSpace(string(raw)); secret != "" {
			return secret, nil
		}
	}
	if secret := os.Getenv(envSecret); secret != "" {
		return secret, nil
	}
	return "", &clifmt.CliError{
		Code: clifmt.ExitUserError,
		Message: "no bearer secret configured; a runner service accepting operations over the network is a " +
			"remote-code-execution surface, and an unauthenticated one executes code for anyone who can reach it",
		Remediation: "set " + envSecret + " (or " + envSecretFile + ") to the secret callers will present",
	}
}

// resolveDurability turns the state-dir / --ephemeral-state pair into one
// explicit posture.
//
// A missing state directory is refused rather than defaulted, and the escape
// hatch is a named flag. That mirrors ServiceIdentity.AllowInsecureTransport:
// accepting a weaker guarantee stays possible, but only as a deliberate,
// greppable act — never as what happens when nobody said anything.
func resolveDurability(resolved *settings) *clifmt.CliError {
	switch {
	case resolved.stateDir != "" && resolved.ephemeral:
		return &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     "both a state directory and --ephemeral-state were given; they are opposite postures",
			Remediation: "drop --ephemeral-state to keep status durably, or unset " + envStateDir,
		}
	case resolved.stateDir != "":
		return nil
	case resolved.ephemeral:
		resolved.durabilityLimit = "status is held in memory only: a restart forgets every operation, and the " +
			"retention this service declares in each acceptance cannot be kept across one"
		return nil
	default:
		return &clifmt.CliError{
			Code: clifmt.ExitUserError,
			Message: "no status state directory configured; the protocol requires that an operation's status " +
				"never disappear before its declared retention elapses, and a process restart is how it disappears",
			Remediation: "set " + envStateDir + " (or --state-dir) to a durable directory, or pass " +
				"--ephemeral-state to accept in-memory status and its restart limit explicitly",
		}
	}
}

func resolveNumbers(resolved *settings, concurrency, queueDepth int) *clifmt.CliError {
	value, cliErr := intOrEnv(concurrency, envConcurrency)
	if cliErr != nil {
		return cliErr
	}
	resolved.concurrency = value

	value, cliErr = intOrEnv(queueDepth, envQueueDepth)
	if cliErr != nil {
		return cliErr
	}
	resolved.queueDepth = value
	return nil
}

func resolveDurations(resolved *settings, retention, pollAfter, stopTimeout time.Duration) *clifmt.CliError {
	value, cliErr := durationOrEnv(retention, envRetention)
	if cliErr != nil {
		return cliErr
	}
	resolved.retention = value

	value, cliErr = durationOrEnv(pollAfter, envPollAfter)
	if cliErr != nil {
		return cliErr
	}
	resolved.pollAfter = value

	value, cliErr = durationOrEnv(stopTimeout, envStopTimeout)
	if cliErr != nil {
		return cliErr
	}
	resolved.stopTimeout = value
	return nil
}

// resolveProfiles merges the repeatable --profile flag with the
// comma-separated environment form and refuses an empty result: a bridge with
// no digest-to-profile mapping refuses every operation, which is a
// misconfiguration worth failing at startup rather than at first dispatch.
func resolveProfiles(resolved *settings) *clifmt.CliError {
	if raw := os.Getenv(envProfiles); raw != "" {
		for _, pair := range strings.Split(raw, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			if _, exists := resolved.profiles[strings.TrimSpace(strings.SplitN(pair, "=", 2)[0])]; exists {
				continue // an explicit --profile wins over the environment
			}
			if err := resolved.profiles.Set(pair); err != nil {
				return &clifmt.CliError{
					Code:        clifmt.ExitUserError,
					Message:     fmt.Sprintf("%s: %v", envProfiles, err),
					Remediation: "use comma-separated DIGEST=PROFILE pairs",
				}
			}
		}
	}
	if len(resolved.profiles) == 0 {
		return &clifmt.CliError{
			Code: clifmt.ExitUserError,
			Message: "no image-digest-to-headspace-profile mapping configured; an unmapped digest is refused " +
				"rather than silently run under a profile the operation never named",
			Remediation: "pass --profile sha256:<digest>=" + headspace.DefaultProfilePython312 + " (repeatable), or set " +
				envProfiles,
		}
	}
	return nil
}

// serve resolves configuration, builds the bridge and the service, and serves
// until SIGINT or SIGTERM.
func serve(args []string, jsonMode bool) error {
	resolved, cliErr := resolve(args)
	if cliErr != nil {
		return cliErr
	}

	bridge, err := headspace.New(headspace.BridgeConfig{
		HeadspaceBin:  resolved.headspaceBin,
		Profile:       resolved.profiles,
		HeadspaceHome: resolved.headspaceHome,
		Provider:      resolved.provider,
		StopTimeout:   resolved.stopTimeout,
	})
	if err != nil {
		return &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("building the headspace bridge: %v", err),
			Remediation: "check --profile, --headspace-bin and --headspace-provider",
		}
	}

	store, err := buildStore(resolved)
	if err != nil {
		return &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("opening the status store: %v", err),
			Remediation: "verify " + envStateDir + " is a writable directory holding only this service's records",
		}
	}

	svc, err := runnerservice.New(runnerservice.Config{
		Runner:          bridge,
		Store:           store,
		Secret:          resolved.secret,
		Concurrency:     resolved.concurrency,
		QueueDepth:      resolved.queueDepth,
		StatusRetention: resolved.retention,
		PollAfter:       resolved.pollAfter,
		OnError:         func(err error) { clifmt.EmitDiagnostic(err.Error()) },
	})
	if err != nil {
		return &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("building the runner service: %v", err),
			Remediation: "check the secret, the retention and the state directory",
		}
	}
	defer svc.Close()

	emitStartup(resolved, svc, bridge, jsonMode)
	return listen(resolved.listen, svc)
}

func buildStore(resolved settings) (runnerservice.Store, error) {
	if resolved.ephemeral {
		return runnerservice.NewMemoryStore(), nil
	}
	return runnerservice.NewFileStore(resolved.stateDir)
}

func emitStartup(resolved settings, svc *runnerservice.Service, bridge *headspace.Bridge, jsonMode bool) {
	startup := map[string]any{
		"mode":             "runner",
		"listen":           resolved.listen,
		"runner":           headspace.RunnerName,
		"runner_revision":  bridge.RunnerRevision(),
		"durable_status":   svc.Durable(),
		"status_retention": svc.StatusRetention().String(),
		"profiles":         len(resolved.profiles),
	}
	if resolved.durabilityLimit != "" {
		startup["durability_limit"] = resolved.durabilityLimit
	}
	if jsonMode {
		if err := clifmt.EmitResultJSON(startup); err != nil {
			clifmt.EmitDiagnostic(err.Error())
		}
	} else {
		clifmt.EmitResult(fmt.Sprintf(
			"nodes-runner listening on %s (runner %s, revision %s, durable status %v, retention %s)",
			resolved.listen, headspace.RunnerName, bridge.RunnerRevision(), svc.Durable(), svc.StatusRetention()))
	}
	if resolved.durabilityLimit != "" {
		clifmt.EmitDiagnostic("nodes-runner: " + resolved.durabilityLimit)
	}
}

// listen serves until a signal arrives, then shuts down gracefully.
func listen(addr string, svc *runnerservice.Service) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := &http.Server{
		Addr:              addr,
		Handler:           svc.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serverErrs := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrs <- err
		}
	}()

	select {
	case <-ctx.Done():
		clifmt.EmitDiagnostic("nodes-runner: signal received, shutting down")
	case err := <-serverErrs:
		return &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("serving on %s: %v", addr, err),
			Remediation: "check the listen address is free and permitted",
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		clifmt.EmitDiagnostic(fmt.Sprintf("nodes-runner: shutdown: %v", err))
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func intOrEnv(flagValue int, envName string) (int, *clifmt.CliError) {
	if flagValue > 0 {
		return flagValue, nil
	}
	raw := os.Getenv(envName)
	if raw == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("%s=%q is not a positive integer", envName, raw),
			Remediation: "set " + envName + " to a positive whole number, or unset it for the default",
		}
	}
	return parsed, nil
}

func durationOrEnv(flagValue time.Duration, envName string) (time.Duration, *clifmt.CliError) {
	if flagValue > 0 {
		return flagValue, nil
	}
	raw := os.Getenv(envName)
	if raw == "" {
		return 0, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("%s=%q is not a positive duration", envName, raw),
			Remediation: "set " + envName + " to a Go duration such as 24h, or unset it for the default",
		}
	}
	return parsed, nil
}
