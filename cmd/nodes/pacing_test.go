package main

import (
	"testing"
	"time"
)

// The dispatch-rate declaration read from the environment (task t10).
//
// Two properties are worth pinning, and the second is the one that matters:
// an absent declaration is no pacing, and a MALFORMED declaration refuses to
// start. An operator who mistyped a duration has stated an intent to limit
// spending; a worker that shrugged and dispatched unpaced would be the
// expensive failure mode this whole control exists to avoid.

func TestPacingConfigIsAbsentByDefault(t *testing.T) {
	opts, err := pacingConfig()
	if err != nil {
		t.Fatalf("pacingConfig with nothing set: %v", err)
	}
	if opts.Enabled() {
		t.Errorf("pacing reports enabled with no variables set: %+v", opts)
	}
}

func TestPacingConfigReadsEveryScope(t *testing.T) {
	t.Setenv(envDispatchRateLimit, "12")
	t.Setenv(envDispatchRateWindow, "3h")
	t.Setenv(envDispatchRateAnchor, "2026-08-13T00:00:00Z")
	t.Setenv(envActorDispatchRateLimit, "4")
	t.Setenv(envActorDispatchRateLimits, "company/analyzer=2, company/reviewer=0")

	opts, err := pacingConfig()
	if err != nil {
		t.Fatalf("pacingConfig: %v", err)
	}
	if !opts.Enabled() {
		t.Fatal("pacing reports disabled with a declared rate")
	}
	if opts.Global.Limit != 12 || opts.Global.Window != 3*time.Hour {
		t.Errorf("global rate = %d per %s, want 12 per 3h", opts.Global.Limit, opts.Global.Window)
	}
	if !opts.Global.Anchor.Equal(time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("anchor = %s, want the declared reset clock", opts.Global.Anchor)
	}
	if opts.Actor.Limit != 4 {
		t.Errorf("per-actor default = %d, want 4", opts.Actor.Limit)
	}
	// The window and the anchor describe the subscription's reset cycle, so
	// every scope shares them.
	if opts.Actor.Window != 3*time.Hour || !opts.Actor.Anchor.Equal(opts.Global.Anchor) {
		t.Errorf("per-actor scope uses window %s anchored %s, want the installation's own",
			opts.Actor.Window, opts.Actor.Anchor)
	}
	if got := opts.ActorOverrides["company/analyzer"]; got.Limit != 2 || got.Window != 3*time.Hour {
		t.Errorf("override for company/analyzer = %+v, want 2 per 3h", got)
	}
	// A zero override opts an actor out of the per-actor default entirely.
	if got := opts.ActorOverrides["company/reviewer"]; got.Enabled() {
		t.Errorf("override for company/reviewer = %+v, want an opt-out", got)
	}
}

func TestPacingConfigDefaultsTheWindow(t *testing.T) {
	t.Setenv(envDispatchRateLimit, "5")
	opts, err := pacingConfig()
	if err != nil {
		t.Fatalf("pacingConfig: %v", err)
	}
	if opts.Global.Window != DefaultDispatchRateWindow {
		t.Errorf("window = %s, want the %s default", opts.Global.Window, DefaultDispatchRateWindow)
	}
	if !opts.Global.Anchor.IsZero() {
		t.Errorf("anchor = %s, want the zero value (windows tile from the Unix epoch)", opts.Global.Anchor)
	}
}

func TestPacingConfigRefusesMalformedDeclarations(t *testing.T) {
	for _, tc := range []struct{ name, env, value string }{
		{"a window that is not a duration", envDispatchRateWindow, "five hours"},
		{"a negative window", envDispatchRateWindow, "-1h"},
		{"a limit that is not a number", envDispatchRateLimit, "lots"},
		{"a negative limit", envDispatchRateLimit, "-3"},
		{"an anchor that is not RFC 3339", envDispatchRateAnchor, "monday"},
		{"an override list that is not key=limit", envActorDispatchRateLimits, "company/analyzer"},
		{"an override with a bad limit", envActorDispatchRateLimits, "company/analyzer=some"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			if _, err := pacingConfig(); err == nil {
				t.Fatalf("%s=%q was accepted; a mistyped rate must refuse to start, not dispatch unpaced",
					tc.env, tc.value)
			}
		})
	}
}
