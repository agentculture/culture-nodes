// Command nodes-notifier is the Discord notifier daemon (economy-discord-
// graphs task t14): an out-of-process consumer of the control plane's
// cross-run SSE feed (GET /v1alpha1/events) that turns run-lifecycle
// events into internal/notify webhook deliveries.
//
// It is a separate binary from `nodes` on purpose, for the same reason
// cmd/nodes-runner is: the control plane gains zero Discord code and zero
// Discord egress by construction, because the process capable of either is
// a different process. This binary holds no database connection and no
// control-plane credential; it only ever issues GET requests against the
// control plane's public HTTP surface (GET /v1alpha1/events, GET
// /v1alpha1/runs/{id}) and, when a webhook URL is configured
// (CULTURE_NODES_WEBHOOK_URL / DISCORD_WEBHOOK_URL -- read only by
// internal/notify.ResolveWebhook, never by this file), one bounded POST
// per lifecycle event.
//
// See internal/notifier for the daemon's testable core (the SSE consumer,
// the durable cursor, the run-detail fetch, and the lifecycle filter) and
// internal/notify for the webhook transport this daemon composes but never
// modifies.
//
// Configuration is environment-first (mirroring cmd/nodes-runner), with a
// flag for every value except the webhook URL, which has neither -- it is
// internal/notify's own env-only contract, unchanged here.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/notifier"
	"github.com/agentculture/culture-nodes/internal/notify"
)

// version is this binary's own version. Like cmd/nodes-runner, the Go side
// has no CI-enforced versioning scheme yet (only pyproject.toml's is), so
// this is a placeholder rather than a promise.
const version = "0.1.0-dev"

// Environment variables nodes-notifier reads. Every one has a matching
// flag; the webhook URL is deliberately absent from this list -- see the
// package doc comment.
const (
	envAPIBase       = "NODES_NOTIFIER_API_BASE"
	envCursorFile    = "NODES_NOTIFIER_CURSOR_FILE"
	envRuns          = "NODES_NOTIFIER_RUNS"
	envDashboardBase = "NODES_NOTIFIER_DASHBOARD_BASE"
	envReconnectMin  = "NODES_NOTIFIER_RECONNECT_MIN"
	envReconnectMax  = "NODES_NOTIFIER_RECONNECT_MAX"
	envHTTPTimeout   = "NODES_NOTIFIER_HTTP_TIMEOUT"
)

var usageText = `nodes-notifier - Culture Nodes Discord notifier daemon

Consumes GET /v1alpha1/events (the cross-run SSE feed) and posts a webhook
delivery for each run-lifecycle event (run.created/completed/failed/
cancelled/bounded) via internal/notify -- Discord if the resolved webhook
URL is a Discord webhook endpoint, a generic flat-JSON POST otherwise.

Usage:
  nodes-notifier [flags]

Flags:
  --api-base URL          control plane base URL (default ` + envAPIBase + `)
  --cursor-file PATH      durable cursor file (default ` + envCursorFile + `)
  --runs ID,ID            scope to exactly these run ids (default: every
                          active run, plus lifecycle events for any run)
  --dashboard-base URL    base URL for dashboard links (defaults to
                          --api-base)
  --reconnect-min D       minimum SSE reconnect backoff (default 500ms)
  --reconnect-max D       maximum SSE reconnect backoff (default 30s)
  --http-timeout D        bound on each run-detail fetch (default 10s)
  --json                  emit machine-readable output
  --version               print the version and exit

Environment (flags win where both exist):
  ` + envAPIBase + `        control plane base URL
  ` + envCursorFile + `     durable cursor file path
  ` + envRuns + `           comma-separated run id scope
  ` + envDashboardBase + `  dashboard link base URL
  ` + envReconnectMin + `   minimum SSE reconnect backoff
  ` + envReconnectMax + `   maximum SSE reconnect backoff
  ` + envHTTPTimeout + `    run-detail fetch timeout
  CULTURE_NODES_WEBHOOK_URL   webhook URL (internal/notify; primary)
  DISCORD_WEBHOOK_URL         webhook URL (internal/notify; fallback)

Neither webhook variable has a flag: the URL itself embeds a bearer token,
and a secret on a command line is visible in every process listing.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

// run parses, resolves, and serves. It never calls os.Exit, so it stays
// unit-testable, and it routes every failure through clifmt's CliError so
// the error:/hint: contract holds here exactly as it does in cmd/nodes and
// cmd/nodes-runner.
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

// settings is the resolved configuration, separated from serve so it can
// be tested without opening a connection.
type settings struct {
	apiBase       string
	cursorFile    string
	runs          []string
	dashboardBase string
	reconnectMin  time.Duration
	reconnectMax  time.Duration
	httpTimeout   time.Duration
}

func resolve(args []string) (settings, *clifmt.CliError) {
	fs := flag.NewFlagSet("nodes-notifier", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	apiBase := fs.String("api-base", "", "control plane base URL (defaults to "+envAPIBase+")")
	cursorFile := fs.String("cursor-file", "", "durable cursor file (defaults to "+envCursorFile+")")
	runs := fs.String("runs", "", "comma-separated run id scope (defaults to "+envRuns+")")
	dashboardBase := fs.String("dashboard-base", "", "dashboard link base URL (defaults to "+envDashboardBase+", then --api-base)")
	reconnectMin := fs.Duration("reconnect-min", 0, "minimum SSE reconnect backoff (defaults to "+envReconnectMin+")")
	reconnectMax := fs.Duration("reconnect-max", 0, "maximum SSE reconnect backoff (defaults to "+envReconnectMax+")")
	httpTimeout := fs.Duration("http-timeout", 0, "run-detail fetch timeout (defaults to "+envHTTPTimeout+")")

	if err := fs.Parse(args); err != nil {
		return settings{}, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     err.Error(),
			Remediation: "run 'nodes-notifier --help' to see usage",
		}
	}

	resolved := settings{
		apiBase:       firstNonEmpty(*apiBase, os.Getenv(envAPIBase)),
		cursorFile:    firstNonEmpty(*cursorFile, os.Getenv(envCursorFile)),
		runs:          splitRuns(firstNonEmpty(*runs, os.Getenv(envRuns))),
		dashboardBase: firstNonEmpty(*dashboardBase, os.Getenv(envDashboardBase)),
	}

	if resolved.apiBase == "" {
		return settings{}, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     "no control plane base URL configured",
			Remediation: "set --api-base (or " + envAPIBase + ") to the control plane's base URL, e.g. http://localhost:8080",
		}
	}
	if resolved.cursorFile == "" {
		return settings{}, &clifmt.CliError{
			Code: clifmt.ExitUserError,
			Message: "no cursor file configured; a notifier that cannot remember its position turns every " +
				"restart into either a dropped or a duplicated batch of notifications",
			Remediation: "set --cursor-file (or " + envCursorFile + ") to a writable, durable path",
		}
	}

	value, cliErr := durationOrEnv(*reconnectMin, envReconnectMin)
	if cliErr != nil {
		return settings{}, cliErr
	}
	resolved.reconnectMin = value

	value, cliErr = durationOrEnv(*reconnectMax, envReconnectMax)
	if cliErr != nil {
		return settings{}, cliErr
	}
	resolved.reconnectMax = value

	value, cliErr = durationOrEnv(*httpTimeout, envHTTPTimeout)
	if cliErr != nil {
		return settings{}, cliErr
	}
	resolved.httpTimeout = value

	return resolved, nil
}

// serve resolves configuration, loads the durable cursor, builds the
// Daemon, and runs it until SIGINT or SIGTERM.
func serve(args []string, jsonMode bool) error {
	resolved, cliErr := resolve(args)
	if cliErr != nil {
		return cliErr
	}

	cursor, err := notifier.LoadCursor(resolved.cursorFile)
	if err != nil {
		return &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("loading cursor file: %v", err),
			Remediation: "verify " + envCursorFile + " points at a readable, writable path holding only this daemon's cursor",
		}
	}

	daemon, err := notifier.NewDaemon(notifier.Config{
		APIBase:       resolved.apiBase,
		CursorPath:    resolved.cursorFile,
		Runs:          resolved.runs,
		DashboardBase: resolved.dashboardBase,
		ReconnectMin:  resolved.reconnectMin,
		ReconnectMax:  resolved.reconnectMax,
		HTTPTimeout:   resolved.httpTimeout,
	}, cursor,
		notifier.WithDiagnostic(clifmt.EmitDiagnostic),
		notifier.WithJournal(journal),
	)
	if err != nil {
		return &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("building the notifier daemon: %v", err),
			Remediation: "check --api-base and --cursor-file",
		}
	}

	emitStartup(resolved, jsonMode)
	return listen(daemon)
}

// journal is the notify.JournalFunc this daemon wires to every
// notify.Notify call: a stderr diagnostic per delivery outcome, never the
// webhook URL or the payload body -- see internal/notify/journal.go's
// JournalEntry, which structurally carries neither.
func journal(entry notify.JournalEntry) {
	clifmt.EmitDiagnostic(fmt.Sprintf("nodes-notifier: %s run=%s outcome=%s", entry.Event, entry.RunID, entry.Outcome))
}

func emitStartup(resolved settings, jsonMode bool) {
	_, webhookEnabled := notify.ResolveWebhook()
	startup := map[string]any{
		"mode":            "notifier",
		"api_base":        resolved.apiBase,
		"cursor_file":     resolved.cursorFile,
		"dashboard_base":  resolved.dashboardBase,
		"runs_scope":      resolved.runs,
		"webhook_enabled": webhookEnabled,
	}
	if jsonMode {
		if err := clifmt.EmitResultJSON(startup); err != nil {
			clifmt.EmitDiagnostic(err.Error())
		}
	} else {
		scope := "every active run (default)"
		if len(resolved.runs) > 0 {
			scope = strings.Join(resolved.runs, ",")
		}
		clifmt.EmitResult(fmt.Sprintf(
			"nodes-notifier consuming %s (cursor %s, runs %s, webhook %s)",
			resolved.apiBase, resolved.cursorFile, scope, enabledWord(webhookEnabled)))
	}
	if !webhookEnabled {
		clifmt.EmitDiagnostic("nodes-notifier: no webhook URL configured (CULTURE_NODES_WEBHOOK_URL / " +
			"DISCORD_WEBHOOK_URL); every lifecycle event will be consumed and journaled as disabled, none delivered")
	}
}

func enabledWord(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

// listen runs daemon until a signal arrives, then returns.
func listen(daemon *notifier.Daemon) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := daemon.Run(ctx); err != nil {
		return &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("running the notifier daemon: %v", err),
			Remediation: "check --api-base is reachable and the cursor file's directory stays writable",
		}
	}
	clifmt.EmitDiagnostic("nodes-notifier: signal received, shutting down")
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

func splitRuns(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, id := range strings.Split(raw, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
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
			Remediation: "set " + envName + " to a Go duration such as 500ms or 30s, or unset it for the default",
		}
	}
	return parsed, nil
}
