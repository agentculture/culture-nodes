package pacing_test

import (
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/pacing"
)

// The reset-clock arithmetic (task t10, spec claim c43, honesty condition
// h36).
//
// h36's acceptance is stated as a COMPARISON, not a threshold: "a wave
// started mid-window demonstrably schedules fewer sessions than the same
// wave started at reset, matching remaining-window arithmetic". These tests
// are that comparison, run against the pure arithmetic with no database and
// no clock of its own — every "now" here is an argument.

// anchor is a fixed reset instant. Real session windows reset on a fixed
// clock; the tests use one too, so nothing here depends on when it runs.
var anchor = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)

func fiveHourConfig(limit int) pacing.Config {
	return pacing.Config{Limit: limit, Window: 5 * time.Hour, Anchor: anchor}
}

// TestCapacityShrinksWithTheRemainingWindow is h36 itself: the same declared
// rate admits a whole wave at the reset instant and only part of it halfway
// through the window, because the control spreads sessions across the
// REMAINING window rather than at a naive fixed rate that ignores when the
// window ends.
func TestCapacityShrinksWithTheRemainingWindow(t *testing.T) {
	cfg := fiveHourConfig(10)

	atReset := cfg.Capacity(anchor)
	if atReset != 10 {
		t.Errorf("capacity at the reset instant = %d, want the whole declared budget (10)", atReset)
	}

	midWindow := cfg.Capacity(anchor.Add(2*time.Hour + 30*time.Minute))
	if midWindow != 5 {
		t.Errorf("capacity halfway through the window = %d, want 5 (half the window absorbs half the sessions)", midWindow)
	}
	if midWindow >= atReset {
		t.Errorf("capacity mid-window (%d) must be FEWER than at reset (%d) — h36", midWindow, atReset)
	}

	lateWindow := cfg.Capacity(anchor.Add(4*time.Hour + 42*time.Minute))
	if lateWindow != 0 {
		t.Errorf("capacity with 18 minutes left = %d, want 0: less than one session's worth of window remains", lateWindow)
	}
}

// The window is tiled off the anchor, not off "now": every worker, whenever
// it starts, agrees on which window it is in and when that window ends.
func TestWindowsTileFromTheResetAnchor(t *testing.T) {
	cfg := fiveHourConfig(10)

	for _, tc := range []struct {
		name      string
		now       time.Time
		wantStart time.Time
	}{
		{"at the anchor", anchor, anchor},
		{"inside the first window", anchor.Add(4*time.Hour + 59*time.Minute), anchor},
		{"at the second window's reset", anchor.Add(5 * time.Hour), anchor.Add(5 * time.Hour)},
		{"inside the third window", anchor.Add(11 * time.Hour), anchor.Add(10 * time.Hour)},
		{"before the anchor", anchor.Add(-time.Minute), anchor.Add(-5 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := cfg.WindowAt(tc.now)
			if !w.Start.Equal(tc.wantStart) {
				t.Errorf("window start = %s, want %s", w.Start, tc.wantStart)
			}
			if !w.End.Equal(tc.wantStart.Add(5*time.Hour)) {
				t.Errorf("window end = %s, want %s", w.End, tc.wantStart.Add(5*time.Hour))
			}
		})
	}
}

// Allowance is the second half of the arithmetic: what the remaining window
// can absorb, capped by what the window's own budget still has left. Either
// can bind, and the smaller one wins.
func TestAllowanceIsTheSmallerOfRemainingWindowAndRemainingBudget(t *testing.T) {
	cfg := fiveHourConfig(10)
	mid := anchor.Add(2*time.Hour + 30*time.Minute)

	if got := cfg.Allowance(mid, 0); got != 5 {
		t.Errorf("allowance mid-window with nothing consumed = %d, want 5 (the remaining window binds)", got)
	}
	if got := cfg.Allowance(mid, 8); got != 2 {
		t.Errorf("allowance mid-window with 8 consumed = %d, want 2 (the window budget binds)", got)
	}
	if got := cfg.Allowance(mid, 10); got != 0 {
		t.Errorf("allowance with the whole budget spent = %d, want 0", got)
	}
	if got := cfg.Allowance(anchor, 0); got != 10 {
		t.Errorf("allowance at reset = %d, want the whole budget", got)
	}
}

// Spacing spreads whatever is left across whatever remains of the window --
// never faster than the declared pace, and slower once consumption has run
// ahead of it.
func TestSpacingSpreadsTheAllowanceAcrossTheRemainingWindow(t *testing.T) {
	cfg := fiveHourConfig(10)

	if got := cfg.Spacing(anchor, 0); got != 30*time.Minute {
		t.Errorf("spacing at reset = %s, want 30m (10 sessions across 5h)", got)
	}
	mid := anchor.Add(2*time.Hour + 30*time.Minute)
	if got := cfg.Spacing(mid, 0); got != 30*time.Minute {
		t.Errorf("spacing mid-window with nothing consumed = %s, want 30m: the declared pace, unchanged", got)
	}
	if got := cfg.Spacing(mid, 8); got != 75*time.Minute {
		t.Errorf("spacing mid-window with 8 of 10 consumed = %s, want 75m: two sessions stretched over 2h30m", got)
	}
}

// A wave, simulated: hand Decide its own previous decision and count how
// many sessions get through before the window ends. This is h36's "wave"
// end to end, over the arithmetic alone.
func waveSize(t *testing.T, cfg pacing.Config, start time.Time) int {
	t.Helper()
	state := pacing.State{}
	now := start
	end := cfg.WindowAt(start).End
	admitted := 0
	for now.Before(end) {
		d := cfg.Decide(now, state)
		if d.Allowed {
			admitted++
			state = d.Next
			continue
		}
		if !d.RetryAt.After(now) {
			t.Fatalf("a refusal must name a FUTURE retry instant; got %s at %s (reason %q)", d.RetryAt, now, d.Reason)
		}
		now = d.RetryAt
	}
	return admitted
}

// TestAWaveStartedMidWindowSchedulesFewerSessions is the acceptance test the
// plan names for t10, played out over a whole window.
func TestAWaveStartedMidWindowSchedulesFewerSessions(t *testing.T) {
	cfg := fiveHourConfig(10)

	atReset := waveSize(t, cfg, anchor)
	midWindow := waveSize(t, cfg, anchor.Add(2*time.Hour+30*time.Minute))
	nearlyOver := waveSize(t, cfg, anchor.Add(4*time.Hour+45*time.Minute))

	if atReset != 10 {
		t.Errorf("wave started at reset scheduled %d sessions, want the whole declared budget (10)", atReset)
	}
	if midWindow != 5 {
		t.Errorf("wave started mid-window scheduled %d sessions, want 5", midWindow)
	}
	if nearlyOver != 0 {
		t.Errorf("wave started 15 minutes from the reset scheduled %d sessions, want 0", nearlyOver)
	}
	if !(atReset > midWindow && midWindow > nearlyOver) {
		t.Errorf("h36: later starts must schedule fewer sessions; got reset=%d mid=%d late=%d",
			atReset, midWindow, nearlyOver)
	}
}

// Consumption already made in this window is not forgotten: the wave that
// arrives after somebody else spent most of the budget gets what is left,
// not a fresh allowance.
func TestPriorConsumptionInTheWindowIsHonored(t *testing.T) {
	cfg := fiveHourConfig(10)
	now := anchor.Add(time.Hour)
	state := pacing.State{WindowStart: anchor, Dispatched: 9}

	d := cfg.Decide(now, state)
	if !d.Allowed {
		t.Fatalf("the tenth session must still be allowed: %+v", d)
	}
	if d.Dispatched != 10 {
		t.Errorf("dispatched after the tenth = %d, want 10", d.Dispatched)
	}

	// An hour later, still inside the same window: the budget is spent and
	// the refusal says so, rather than reporting the spacing that happens to
	// share its deadline.
	next := cfg.Decide(now.Add(time.Hour), d.Next)
	if next.Allowed {
		t.Fatalf("the eleventh session must be refused: the window's budget is spent")
	}
	if next.Reason != pacing.ReasonWindowBudget {
		t.Errorf("refusal reason = %q, want %q", next.Reason, pacing.ReasonWindowBudget)
	}
	if !next.RetryAt.Equal(cfg.WindowAt(now).End) {
		t.Errorf("retry at = %s, want the next window's reset (%s)", next.RetryAt, cfg.WindowAt(now).End)
	}
}

// The window rolls on the clock, not on a sweep: state carried over from the
// previous window counts as nothing in this one.
func TestTheWindowRollsOnTheAnchorClock(t *testing.T) {
	cfg := fiveHourConfig(10)
	spent := pacing.State{WindowStart: anchor, Dispatched: 10, NextDispatchAt: anchor.Add(4 * time.Hour)}

	if d := cfg.Decide(anchor.Add(4*time.Hour+30*time.Minute), spent); d.Allowed {
		t.Fatalf("a spent window must refuse: %+v", d)
	}
	d := cfg.Decide(anchor.Add(5*time.Hour), spent)
	if !d.Allowed {
		t.Fatalf("the reset must admit again with no sweep and no operator action: %+v", d)
	}
	if d.Dispatched != 1 {
		t.Errorf("dispatched after the first session of a new window = %d, want 1", d.Dispatched)
	}
	if !d.Next.WindowStart.Equal(anchor.Add(5 * time.Hour)) {
		t.Errorf("next state window start = %s, want the new window's reset", d.Next.WindowStart)
	}
}

// Spacing refuses in the middle of a window that still has budget: this is
// the "backlog does not drain at dispatch speed" half.
func TestSpacingRefusesUntilTheNextSlot(t *testing.T) {
	cfg := fiveHourConfig(10)
	first := cfg.Decide(anchor, pacing.State{})
	if !first.Allowed {
		t.Fatalf("the first dispatch of a fresh window must be allowed: %+v", first)
	}

	immediately := cfg.Decide(anchor.Add(time.Second), first.Next)
	if immediately.Allowed {
		t.Fatalf("a second dispatch one second later must be paced, not allowed: %+v", immediately)
	}
	if immediately.Reason != pacing.ReasonSpacing {
		t.Errorf("refusal reason = %q, want %q", immediately.Reason, pacing.ReasonSpacing)
	}
	if !immediately.RetryAt.Equal(anchor.Add(30 * time.Minute)) {
		t.Errorf("retry at = %s, want the next paced slot %s", immediately.RetryAt, anchor.Add(30*time.Minute))
	}
	if d := cfg.Decide(anchor.Add(30*time.Minute), first.Next); !d.Allowed {
		t.Errorf("the slot's own instant must be allowed: %+v", d)
	}
}

// A configuration that declares no limit is off, and "off" must be free:
// no window, no state, no refusals.
func TestAnUnsetConfigIsDisabled(t *testing.T) {
	for _, cfg := range []pacing.Config{
		{},
		{Limit: 0, Window: time.Hour},
		{Limit: 5},
		{Limit: -1, Window: time.Hour},
	} {
		if cfg.Enabled() {
			t.Errorf("%+v reports enabled, want disabled", cfg)
		}
		if d := cfg.Decide(anchor, pacing.State{}); !d.Allowed {
			t.Errorf("%+v refused a dispatch while disabled: %+v", cfg, d)
		}
	}
}

// A zero anchor is the Unix epoch, not year 1: windows still tile, and the
// arithmetic never has to handle a duration overflow.
func TestAZeroAnchorTilesFromTheUnixEpoch(t *testing.T) {
	cfg := pacing.Config{Limit: 4, Window: time.Hour}
	now := time.Date(2026, 8, 13, 9, 20, 0, 0, time.UTC)
	w := cfg.WindowAt(now)
	if !w.Start.Equal(time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("window start = %s, want the top of the hour", w.Start)
	}
}
