package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/scheduler"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/telemetry"
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

	sch := scheduler.New(db, scheduler.Options{
		OwnerID:      os.Getenv("NODES_SCHEDULER_ID"),
		TickInterval: *tickInterval,
		BatchSize:    *batchSize,
		Telemetry:    telemetryProvider,
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
