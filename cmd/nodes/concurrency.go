package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Per-actor concurrency-ceiling configuration (task t16, issue #166's
// second half). Same reasoning as pacing.go's own header: a "how many of
// this actor may run at once" ceiling is a fact about the MACHINE(S) behind
// the actor, not about any workflow, so it is environment configuration
// read at worker startup, alongside pacing and everything else a worker
// process cannot invent for itself. The zero configuration is no ceiling at
// all.

// Environment variables that declare the concurrency ceilings a worker
// holds itself to.
const (
	// envActorMaxConcurrent is the default per-actor ceiling: each actor key
	// gets its own cap of this size on in-flight invocations. Unset or 0
	// means uncapped.
	envActorMaxConcurrent = "NODES_ACTOR_MAX_CONCURRENT"
	// envActorMaxConcurrentOverrides overrides the default for named actors,
	// as a comma-separated list of actor_key=limit pairs
	// ("company/analyzer=1,company/reviewer=2"). A limit of 0 opts that
	// actor out of the default entirely, the same escape hatch
	// envActorDispatchRateLimits (pacing.go) offers.
	envActorMaxConcurrentOverrides = "NODES_ACTOR_MAX_CONCURRENT_OVERRIDES"
)

// concurrencyConfig reads the per-actor concurrency declaration from the
// environment. Like pacingConfig it refuses malformed input rather than
// quietly starting uncapped: an operator who mistyped a limit stated an
// intent to bound concurrency, and silently ignoring it is the expensive
// failure mode.
func concurrencyConfig() (worker.ConcurrencyOptions, *clifmt.CliError) {
	defaultLimit, cliErr := concurrencyLimit(envActorMaxConcurrent, os.Getenv(envActorMaxConcurrent))
	if cliErr != nil {
		return worker.ConcurrencyOptions{}, cliErr
	}
	overrides, cliErr := concurrencyOverrides()
	if cliErr != nil {
		return worker.ConcurrencyOptions{}, cliErr
	}
	return worker.ConcurrencyOptions{ActorDefault: defaultLimit, ActorOverrides: overrides}, nil
}

func concurrencyLimit(name, raw string) (int, *clifmt.CliError) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 0 {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("%s=%q is not a whole number of concurrent invocations", name, raw),
			Remediation: "set it to a non-negative integer, or unset it to declare no ceiling",
		}
	}
	return limit, nil
}

func concurrencyOverrides() (map[string]int, *clifmt.CliError) {
	raw := strings.TrimSpace(os.Getenv(envActorMaxConcurrentOverrides))
	if raw == "" {
		return nil, nil
	}
	overrides := map[string]int{}
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
				Message:     fmt.Sprintf("%s entry %q is not actor_key=limit", envActorMaxConcurrentOverrides, pair),
				Remediation: "write it as a comma-separated list such as company/analyzer=1,company/reviewer=2",
			}
		}
		limit, cliErr := concurrencyLimit(envActorMaxConcurrentOverrides+" entry "+key, value)
		if cliErr != nil {
			return nil, cliErr
		}
		overrides[key] = limit
	}
	return overrides, nil
}
