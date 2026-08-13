package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/pacing"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Dispatch pacing configuration (task t10; issue #48 item 2, spec claims
// c5/c43).
//
// WHY THE ENVIRONMENT AND NOT THE WORKFLOW. A session rate is a fact about
// the SUBSCRIPTION a deployment dispatches against -- how many sessions the
// account holds per window and when that window resets -- not a fact about
// any workflow. Two installations running the identical published workflow
// have different rates, and one installation running twenty workflows has
// one. Putting it in the workflow document would pin an operational
// constraint into a content-addressed artifact and make changing a
// subscription plan a workflow revision. (The workflow-level economics that
// DO belong to the author -- a budget of sessions one run may spend -- is
// task t11's `budget` block, a different thing with a different owner.)
//
// So this is environment configuration, alongside the other things a worker
// process cannot invent for itself, and changing it is a restart rather than
// a code change. Every variable is optional and the zero configuration is no
// pacing at all.

// Environment variables that declare the dispatch rates a worker holds
// itself to.
const (
	// envDispatchRateLimit is the whole installation's session rate: how
	// many actor dispatches all of this namespace's workers together may
	// start per window. Unset or 0 disables global pacing.
	//
	// This is the meter issue #48 is actually about: "the operator's
	// interactive session, local subagents, and all bridge sessions on the
	// same account share ONE subscription session window -- not independent
	// capacity pools".
	envDispatchRateLimit = "NODES_DISPATCH_RATE_LIMIT"
	// envDispatchRateWindow is the session window's length as a Go duration
	// ("5h", "1h30m"). It applies to every scope, because it describes the
	// subscription's reset cycle rather than any one scope's allowance.
	envDispatchRateWindow = "NODES_DISPATCH_RATE_WINDOW"
	// envDispatchRateAnchor is the reset clock: an RFC 3339 instant at which
	// a window boundary falls. Windows tile off it in both directions, so
	// any past or future boundary will do -- what matters is that every
	// worker uses the same one. Unset means the Unix epoch, which makes
	// round window lengths tile on round clock times.
	envDispatchRateAnchor = "NODES_DISPATCH_RATE_ANCHOR"
	// envActorDispatchRateLimit is the default per-actor rate: each actor
	// key gets its OWN budget of this size, on the same window. Unset or 0
	// means actors are limited only by the global rate.
	envActorDispatchRateLimit = "NODES_ACTOR_DISPATCH_RATE_LIMIT"
	// envActorDispatchRateLimits overrides the default for named actors, as
	// a comma-separated list of actor_key=limit pairs
	// ("company/analyzer=4,company/reviewer=1"). A limit of 0 opts that
	// actor out of the default entirely, which is the only way to say "pace
	// everything except this one".
	envActorDispatchRateLimits = "NODES_ACTOR_DISPATCH_RATE_LIMITS"
)

// DefaultDispatchRateWindow is the window assumed when a rate is declared
// without one. Five hours is the session window the subscription this
// control was written for actually resets on (issue #48's lane accounting);
// it is a default rather than a constant precisely because that is a fact
// about a plan, not about the software.
const DefaultDispatchRateWindow = 5 * time.Hour

// pacingConfig reads the dispatch-rate declaration from the environment.
//
// It refuses malformed input rather than quietly falling back to "no
// pacing": an operator who mistyped a duration has stated an intent to limit
// spending, and starting a worker that ignores it is the expensive failure
// mode. A declaration that is simply absent is not malformed and yields the
// zero configuration.
func pacingConfig() (worker.PacingOptions, *clifmt.CliError) {
	window, cliErr := pacingWindow()
	if cliErr != nil {
		return worker.PacingOptions{}, cliErr
	}
	anchor, cliErr := pacingAnchor()
	if cliErr != nil {
		return worker.PacingOptions{}, cliErr
	}

	globalLimit, cliErr := pacingLimit(envDispatchRateLimit, os.Getenv(envDispatchRateLimit))
	if cliErr != nil {
		return worker.PacingOptions{}, cliErr
	}
	actorLimit, cliErr := pacingLimit(envActorDispatchRateLimit, os.Getenv(envActorDispatchRateLimit))
	if cliErr != nil {
		return worker.PacingOptions{}, cliErr
	}
	overrides, cliErr := pacingOverrides(window, anchor)
	if cliErr != nil {
		return worker.PacingOptions{}, cliErr
	}

	return worker.PacingOptions{
		Global:         pacing.Config{Limit: globalLimit, Window: window, Anchor: anchor},
		Actor:          pacing.Config{Limit: actorLimit, Window: window, Anchor: anchor},
		ActorOverrides: overrides,
	}, nil
}

func pacingWindow() (time.Duration, *clifmt.CliError) {
	raw := strings.TrimSpace(os.Getenv(envDispatchRateWindow))
	if raw == "" {
		return DefaultDispatchRateWindow, nil
	}
	window, err := time.ParseDuration(raw)
	if err != nil || window <= 0 {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("%s=%q is not a positive duration", envDispatchRateWindow, raw),
			Remediation: "set it to a Go duration such as 5h or 90m, or unset it for the " + DefaultDispatchRateWindow.String() + " default",
		}
	}
	return window, nil
}

func pacingAnchor() (time.Time, *clifmt.CliError) {
	raw := strings.TrimSpace(os.Getenv(envDispatchRateAnchor))
	if raw == "" {
		return time.Time{}, nil
	}
	anchor, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("%s=%q is not an RFC 3339 instant", envDispatchRateAnchor, raw),
			Remediation: "set it to a window boundary such as 2026-08-13T00:00:00Z, or unset it to tile windows from the Unix epoch",
		}
	}
	return anchor.UTC(), nil
}

func pacingLimit(name, raw string) (int, *clifmt.CliError) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 0 {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("%s=%q is not a whole number of dispatches per window", name, raw),
			Remediation: "set it to a non-negative integer, or unset it to declare no rate",
		}
	}
	return limit, nil
}

func pacingOverrides(window time.Duration, anchor time.Time) (map[string]pacing.Config, *clifmt.CliError) {
	raw := strings.TrimSpace(os.Getenv(envActorDispatchRateLimits))
	if raw == "" {
		return nil, nil
	}
	overrides := map[string]pacing.Config{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, found := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			return nil, &clifmt.CliError{
				Code:        clifmt.ExitUserError,
				Message:     fmt.Sprintf("%s entry %q is not actor_key=limit", envActorDispatchRateLimits, pair),
				Remediation: "write it as a comma-separated list such as company/analyzer=4,company/reviewer=1",
			}
		}
		limit, cliErr := pacingLimit(envActorDispatchRateLimits+" entry "+key, value)
		if cliErr != nil {
			return nil, cliErr
		}
		overrides[key] = pacing.Config{Limit: limit, Window: window, Anchor: anchor}
	}
	return overrides, nil
}
