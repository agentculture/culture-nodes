package main

import (
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The two schedule-backoff knobs (task t9, issue #253). Both refuse a value
// they cannot honour rather than silently falling back to the default: an
// operator who set NODES_SCHEDULE_PROBE_INTERVAL=30 (seconds? minutes?)
// meant something by it, and a scheduler that quietly probed every 30
// minutes anyway would leave them believing a setting took effect that did
// not. That is the same argument CreateSchedule makes about a fractional
// interval.

func TestScheduleProbeIntervalReadsTheEnvironmentOrRefusesIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		set     bool
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset selects the store default", want: 0},
		{name: "a duration is honoured", set: true, raw: "2h", want: 2 * time.Hour},
		{name: "whitespace is trimmed", set: true, raw: "  45m  ", want: 45 * time.Minute},
		{name: "a bare number is refused", set: true, raw: "30", wantErr: true},
		{name: "zero is refused", set: true, raw: "0s", wantErr: true},
		{name: "a negative duration is refused", set: true, raw: "-5m", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(envScheduleProbeInterval, tc.raw)
			} else {
				t.Setenv(envScheduleProbeInterval, "")
			}
			got, cliErr := scheduleProbeInterval()
			if tc.wantErr {
				if cliErr == nil {
					t.Fatalf("%s=%q was accepted as %s, want a refusal", envScheduleProbeInterval, tc.raw, got)
				}
				if cliErr.Remediation == "" {
					t.Error("a refusal with no hint: line is not this CLI's error contract")
				}
				return
			}
			if cliErr != nil {
				t.Fatalf("%s=%q refused: %v", envScheduleProbeInterval, tc.raw, cliErr)
			}
			if got != tc.want {
				t.Fatalf("%s=%q = %s, want %s", envScheduleProbeInterval, tc.raw, got, tc.want)
			}
		})
	}
}

func TestSweepFailureAlertAfterReadsTheEnvironmentOrRefusesIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		set     bool
		raw     string
		want    int
		wantErr bool
	}{
		{name: "unset selects the store default", want: 0},
		{name: "a count is honoured", set: true, raw: "5", want: 5},
		// Zero is "never ask a human", and it must not travel as 0 -- that is
		// the value the store reads as "use the default", so an operator who
		// disabled the alert would get it at the default threshold instead.
		{name: "zero disables the alert", set: true, raw: "0", want: -1},
		{name: "a negative count is refused", set: true, raw: "-1", wantErr: true},
		{name: "a non-number is refused", set: true, raw: "three", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(envSweepFailureAlertAfter, tc.raw)
			} else {
				t.Setenv(envSweepFailureAlertAfter, "")
			}
			got, cliErr := sweepFailureAlertAfter()
			if tc.wantErr {
				if cliErr == nil {
					t.Fatalf("%s=%q was accepted as %d, want a refusal", envSweepFailureAlertAfter, tc.raw, got)
				}
				return
			}
			if cliErr != nil {
				t.Fatalf("%s=%q refused: %v", envSweepFailureAlertAfter, tc.raw, cliErr)
			}
			if got != tc.want {
				t.Fatalf("%s=%q = %d, want %d", envSweepFailureAlertAfter, tc.raw, got, tc.want)
			}
		})
	}
}

// TestSchedulerExplainDocumentsTheBackoffKnobs keeps `nodes explain scheduler`
// honest: an operator learns these two variables exist from the CLI, and a
// knob nobody can find is a knob nobody sets.
func TestSchedulerExplainDocumentsTheBackoffKnobs(t *testing.T) {
	for _, want := range []string{envScheduleProbeInterval, envSweepFailureAlertAfter} {
		if !strings.Contains(explainScheduler, want) {
			t.Errorf("nodes explain scheduler does not mention %s", want)
		}
	}
	if postgres.DefaultScheduleProbeInterval != 30*time.Minute {
		t.Errorf("the documented 30m default drifted to %s", postgres.DefaultScheduleProbeInterval)
	}
	if postgres.DefaultSweepFailureAlertAfter != 3 {
		t.Errorf("the documented default of 3 drifted to %d", postgres.DefaultSweepFailureAlertAfter)
	}
}
