package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/scheduler"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/telemetry"
	"github.com/agentculture/culture-nodes/internal/ticketreport"
)

// `nodes scheduler`: the process that fires durable timers and sweeps expired
// leases (PRD §12.7, §20.4).
//
// Every scheduler process runs the same loop and contends for one advisory
// lock; exactly one is active and the rest are standbys that take over when
// it goes away. So there is no --active flag and no leader configuration:
// which instance is active is a runtime fact, not a deployment decision, and
// making it a flag would create the one thing the advisory lock exists to
// prevent — two processes both believing they are the leader.

func cmdScheduler(args []string, jsonMode bool) (int, error) {
	fs := newFlagSet("scheduler")
	databaseURL := fs.String("database-url", "", "PostgreSQL connection URL (defaults to "+envDatabaseURL+")")
	tickInterval := fs.Duration("tick-interval", 0, "how often an active scheduler claims due timers (default 1s)")
	batchSize := fs.Int("batch", 0, "how many due timers one tick claims (default 100)")
	if err := fs.Parse(args); err != nil {
		return 0, parseError("scheduler", err)
	}

	url := firstNonEmpty(*databaseURL, os.Getenv(envDatabaseURL))
	if url == "" {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     "no database URL configured",
			Remediation: "set " + envDatabaseURL + " or pass --database-url postgres://...",
		}
	}

	ctx, stop := shutdownContext()
	defer stop()

	// task t19: env-gated (OTEL_EXPORTER_OTLP_ENDPOINT); telemetry.New
	// returns a safe no-op Provider when it is unset, so a standalone
	// `nodes scheduler` process with no collector configured fires
	// deadline timers exactly as it did before this instrumentation
	// existed. This is the one engine-transition-commit path this package
	// drives itself (deadline timeouts, scheduler.go's commitTimeout).
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
			clifmt.EmitDiagnostic(fmt.Sprintf("nodes scheduler: telemetry shutdown: %v", err))
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

	probeInterval, cliErr := scheduleProbeInterval()
	if cliErr != nil {
		return 0, cliErr
	}
	alertAfter, cliErr := sweepFailureAlertAfter()
	if cliErr != nil {
		return 0, cliErr
	}

	sch := scheduler.New(db, scheduler.Options{
		OwnerID:                   os.Getenv("NODES_SCHEDULER_ID"),
		TickInterval:              *tickInterval,
		BatchSize:                 *batchSize,
		Telemetry:                 telemetryProvider,
		TicketReports:             ticketreport.New(db, nil),
		ScheduleProbeInterval:     probeInterval,
		ScheduleFailureAlertAfter: alertAfter,
	})

	startup := map[string]any{
		"mode":     "scheduler",
		"owner_id": sch.OwnerID(),
		"status":   string(sch.Health().Status),
	}
	if jsonMode {
		if err := clifmt.EmitResultJSON(startup); err != nil {
			return 0, err
		}
	} else {
		clifmt.EmitResult(fmt.Sprintf(
			"scheduler %s starting as standby; it becomes active on acquiring the single-active advisory lock\n"+
				"press Ctrl-C or send SIGTERM to stop", sch.OwnerID()))
	}

	if err := sch.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("scheduler stopped: %v", err),
			Remediation: "check the database is reachable and re-start the scheduler",
		}
	}
	return clifmt.ExitSuccess, nil
}

const explainScheduler = `# nodes scheduler

Runs the scheduler process: claim due durable timers in bounded batches,
apply each timer's effect and mark it fired in one transaction, and sweep
expired work-item leases so a dead worker's work becomes claimable again
(PRD §12.7, §20.4).

Every scheduler process runs the same loop and contends for a single
advisory lock. Exactly one is active; the rest poll as standbys and take
over automatically when the active instance's session ends — so running
several is how the role is made highly available, not a misconfiguration.

## Configuration

    NODES_DATABASE_URL           PostgreSQL connection URL (required)
    NODES_SCHEDULER_ID           instance identity stamped into timers.claimed_by
    NODES_SCHEDULE_PROBE_INTERVAL
                                 how often a SUPPRESSED schedule still mints a
                                  probe run (default 30m). A schedule whose last
                                  two runs failed with the same reason stops
                                  minting on its declared cadence and mints at
                                  most this often, so a repaired environment is
                                  discovered without an operator resuming
                                  anything. A completed run clears it.
    NODES_SWEEP_FAILURE_ALERT_AFTER
                                 how many consecutive identical failures raise
                                  ONE pending schedule_failing human task per
                                  schedule (default 3). It is not re-raised
                                  while one is pending, and is raised again
                                  after a human decides it if failures continue.
    OTEL_EXPORTER_OTLP_ENDPOINT  OTLP collector endpoint; unset disables all
                                  tracing/metrics export (no exporter, no
                                  goroutine, no dial -- see internal/telemetry)

## Usage

    nodes scheduler
    nodes scheduler --tick-interval 500ms --batch 50
    nodes scheduler --json

Stops cleanly on SIGINT or SIGTERM, releasing the advisory lock so a
standby takes over immediately.
`

const (
	// envScheduleProbeInterval and envSweepFailureAlertAfter are the two knobs
	// task t9 (issue #253) puts on schedule failure backoff. Both are read
	// here rather than in internal/store/postgres because they are deployment
	// configuration -- how noisy this environment is willing to be -- not
	// properties of a schedule declaration, which is the same line
	// migrations/0033 draws between a cadence and a workflow.
	envScheduleProbeInterval  = "NODES_SCHEDULE_PROBE_INTERVAL"
	envSweepFailureAlertAfter = "NODES_SWEEP_FAILURE_ALERT_AFTER"
)

// scheduleProbeInterval reads NODES_SCHEDULE_PROBE_INTERVAL. Unset selects the
// store's default; anything that is not a positive duration is refused rather
// than silently defaulted, because an operator who set it meant something by
// it and a typo that halves the probe rate is invisible otherwise.
func scheduleProbeInterval() (time.Duration, *clifmt.CliError) {
	raw := strings.TrimSpace(os.Getenv(envScheduleProbeInterval))
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, &clifmt.CliError{
			Code:    clifmt.ExitUserError,
			Message: fmt.Sprintf("%s=%q is not a positive duration", envScheduleProbeInterval, raw),
			Remediation: "set it to a Go duration such as 30m or 2h, or unset it for the " +
				postgres.DefaultScheduleProbeInterval.String() + " default",
		}
	}
	return d, nil
}

// sweepFailureAlertAfter reads NODES_SWEEP_FAILURE_ALERT_AFTER. 0 disables the
// alert (mapped to a negative sentinel, since 0 means "use the default" on the
// way through), and a negative value is a refusal rather than a second way to
// spell "off".
func sweepFailureAlertAfter() (int, *clifmt.CliError) {
	raw := strings.TrimSpace(os.Getenv(envSweepFailureAlertAfter))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, &clifmt.CliError{
			Code:    clifmt.ExitUserError,
			Message: fmt.Sprintf("%s=%q is not a non-negative whole number", envSweepFailureAlertAfter, raw),
			Remediation: fmt.Sprintf("set it to how many consecutive identical failures should raise a human task "+
				"(0 disables the task), or unset it for the default of %d", postgres.DefaultSweepFailureAlertAfter),
		}
	}
	if n == 0 {
		return -1, nil
	}
	return n, nil
}
