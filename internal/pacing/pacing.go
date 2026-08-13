// Package pacing is the dispatch-rate arithmetic: given a declared session
// rate, a session window that resets on a fixed clock, and what has already
// been consumed in the window we are in, it answers one question — may this
// dispatch go now, and if not, when.
//
// WHY IT IS ITS OWN PACKAGE. Two callers need the same answer and neither may
// depend on the other. internal/store/postgres applies it inside the
// transaction that holds the rate row's lock (the DB clock is the only clock
// horizontally-scaled workers share, so "now" comes from there), and
// internal/worker configures and explains it. Putting the arithmetic in
// either would make the other import a package it has no business importing.
// Keeping it here also makes it what it actually is: a pure function of
// (config, state, now), unit-testable without a database, a clock, or a
// worker — which is what honesty condition h36 has to be demonstrated
// against.
//
// WHY NOT A NAIVE FIXED RATE (spec claim c43, issue #48 comment item 5).
// "One dispatch every N minutes" is rate limiting for a system with infinite
// time. A subscription session window is not that: it holds a fixed number
// of sessions, it resets on a wall clock nobody here controls, and whatever
// is unspent at the reset is simply gone. Two consequences fall out, and both
// are implemented below rather than left to an operator to reason about:
//
//   - The budget already consumed IN THIS WINDOW is part of the answer. A
//     fresh worker process must not get a fresh allowance; that is the whole
//     reason this state is durable and shared rather than in memory.
//   - What the window can still absorb shrinks as the window runs out.
//     Capacity is the remaining time at the declared pace, so a wave that
//     starts halfway through a five-hour window can place half as many
//     sessions as the same wave started at the reset (honesty condition h36).
//     This is deliberately NOT "compress the remaining budget into whatever
//     time is left" — that reading would let a late wave dispatch faster than
//     the declared pace, which is the burst the pacing exists to prevent, and
//     it would make h36's comparison come out equal.
//
// WHAT IT DOES NOT DO. It has no opinion about what a "session" is, does not
// know an actor from a namespace, and never decides what to do with a
// refusal. A refusal here is a "not yet, look again at T"; turning that into
// a deferred work item, an event, and an operator-visible reason is
// internal/worker's job (see its pacing.go).
package pacing

import (
	"math"
	"time"
)

// Refusal reasons. They are the operator-facing vocabulary for "why did this
// dispatch not go", and they are distinct because the remedies differ: a
// paced dispatch needs nothing but patience, an exhausted window budget
// needs either a bigger declared limit or the next reset, and an exhausted
// window capacity means the wave arrived too late in this window to place
// another session at the declared pace.
const (
	// ReasonSpacing: there is budget left, but the previous dispatch's slot
	// has not elapsed yet.
	ReasonSpacing = "paced"
	// ReasonWindowBudget: this window's declared session count is fully
	// consumed. Nothing more goes out until the window resets.
	ReasonWindowBudget = "window_budget_exhausted"
	// ReasonWindowCapacity: less than one session's worth of window remains
	// at the declared pace, so the rest of this window can absorb nothing
	// more even though budget is unspent. See the package comment for why
	// unspent budget is not compressed into the remaining time.
	ReasonWindowCapacity = "window_capacity_exhausted"
)

// Config is a declared dispatch rate: Limit sessions per Window, with
// windows tiled off Anchor.
//
// Anchor is the load-bearing field and the one a naive rate limiter has no
// equivalent of. It is the reset clock — the instant some window boundary
// falls — and every window boundary before and after it is Anchor ± k*Window.
// Two workers that agree on the anchor agree on which window they are in
// without exchanging a message.
type Config struct {
	// Limit is how many dispatches one window may admit. Zero or negative
	// disables pacing entirely.
	Limit int
	// Window is the length of one session window. Zero or negative disables
	// pacing entirely.
	Window time.Duration
	// Anchor is any instant at which a window boundary falls. The zero value
	// means the Unix epoch, which makes round window lengths tile on round
	// clock times (an hourly window starts on the hour).
	Anchor time.Time
}

// Enabled reports whether this config paces anything. A config that does not
// is free: Decide admits everything and no durable state is touched.
func (c Config) Enabled() bool { return c.Limit > 0 && c.Window > 0 }

// anchorAt is Anchor with the zero value resolved to the Unix epoch. It also
// keeps the arithmetic away from year 1, where a Sub against a modern
// timestamp would overflow time.Duration rather than return a number.
//
// It truncates to microseconds, which is not cosmetic. The window start this
// produces is written to a PostgreSQL TIMESTAMPTZ, whose resolution is one
// microsecond, and read back to be compared against a freshly computed one
// (that comparison is how a window roll is detected). An anchor carrying
// nanoseconds would make every stored window start a few hundred nanoseconds
// EARLIER than the recomputed one, every row would look like it belonged to
// an older window, and the counter would reset on every single decision --
// a rate limiter that silently limits nothing. Rounding here, once, is the
// cheapest place to make the in-memory and stored values the same value.
func (c Config) anchorAt() time.Time {
	if c.Anchor.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return c.Anchor.UTC().Truncate(time.Microsecond)
}

// Window is one session window: the half-open interval [Start, End).
type Window struct {
	Start time.Time
	End   time.Time
}

// Contains reports whether now falls in this window.
func (w Window) Contains(now time.Time) bool {
	return !now.Before(w.Start) && now.Before(w.End)
}

// WindowAt is the window now falls in, tiled off the anchor in both
// directions (an anchor set in the future is as valid as one in the past --
// it is a phase, not a start date).
func (c Config) WindowAt(now time.Time) Window {
	if c.Window <= 0 {
		return Window{Start: now, End: now}
	}
	anchor := c.anchorAt()
	elapsed := now.Sub(anchor)
	index := floorDiv(int64(elapsed), int64(c.Window))
	// Truncated for the same round-trip reason anchorAt is: a window length
	// carrying sub-microsecond precision would otherwise reintroduce the
	// mismatch the anchor rounding removes.
	start := anchor.Add(time.Duration(index) * c.Window).Truncate(time.Microsecond)
	return Window{Start: start, End: start.Add(c.Window)}
}

// Capacity is how many more dispatches the REST of the current window can
// absorb at the declared pace: the remaining time measured in pace intervals.
//
// This is the reset-clock half of the arithmetic and the one h36 is stated
// against. It ignores consumption entirely -- Allowance combines the two --
// so that "how much window is left" stays a fact about the clock alone.
//
// It rounds UP, and that is a decision rather than an accident. Rounding down
// is the tidier arithmetic and it makes small rates unusable: with a limit of
// one session per window, floor(1 x remaining/window) is 1 only at the exact
// instant of the reset and 0 for every other moment of the window, so the
// rate would admit nothing at all unless a worker happened to ask in the
// first nanosecond. Rounding up says instead that while any of the window
// remains there is room for one more session -- and because Spacing then
// spreads that single session over the whole remaining time, "one more" at
// the tail of a window is one, not a burst.
func (c Config) Capacity(now time.Time) int {
	if !c.Enabled() {
		return 0
	}
	w := c.WindowAt(now)
	remaining := w.End.Sub(now)
	if remaining <= 0 {
		return 0
	}
	if remaining >= c.Window {
		return c.Limit
	}
	// float64 rather than int64: limit * remaining-in-nanoseconds overflows
	// int64 for perfectly ordinary configurations (a thousand sessions across
	// an hour), and the ratio is in [0,1) here so a 53-bit mantissa is more
	// precision than a session count can use.
	n := int(math.Ceil(float64(c.Limit) * (float64(remaining) / float64(c.Window))))
	if n < 0 {
		return 0
	}
	if n > c.Limit {
		return c.Limit
	}
	return n
}

// Allowance is how many more dispatches may go out before this window ends:
// the smaller of what the remaining window can absorb (Capacity) and what
// the window's own budget still has left (Limit - dispatched). Either can
// bind, and which one did is what separates ReasonWindowCapacity from
// ReasonWindowBudget in a refusal.
func (c Config) Allowance(now time.Time, dispatched int) int {
	if !c.Enabled() {
		return 0
	}
	budgetLeft := c.Limit - dispatched
	if budgetLeft < 0 {
		budgetLeft = 0
	}
	capacity := c.Capacity(now)
	if budgetLeft < capacity {
		return budgetLeft
	}
	return capacity
}

// Spacing is how long after a dispatch made at now the next one may go: the
// remaining window divided by the allowance.
//
// It is never shorter than the declared pace interval (Window/Limit), because
// Allowance is never larger than Capacity, which is exactly the remaining
// time measured in pace intervals. It gets LONGER when consumption has run
// ahead of the pace: two sessions left with two and a half hours of window is
// one every seventy-five minutes, not one every thirty.
//
// Spacing on a config that admits nothing is zero -- there is no next slot to
// name, and Decide reports the refusal's retry instant instead.
func (c Config) Spacing(now time.Time, dispatched int) time.Duration {
	allowance := c.Allowance(now, dispatched)
	if allowance <= 0 {
		return 0
	}
	remaining := c.WindowAt(now).End.Sub(now)
	if remaining <= 0 {
		return 0
	}
	return remaining / time.Duration(allowance)
}

// State is the durable half: what one rate scope has consumed, and when its
// next slot opens. The zero value is a scope that has never dispatched.
//
// WindowStart is stored rather than derived so a window roll is a comparison
// and not a sweep: a row whose WindowStart is older than the current window's
// has consumed nothing in THIS window, whatever its counter says, and no
// process has to go and reset it.
type State struct {
	WindowStart    time.Time
	Dispatched     int
	NextDispatchAt time.Time
}

// Decision is Decide's answer.
type Decision struct {
	// Allowed reports whether this dispatch may go now.
	Allowed bool
	// Reason names why it may not, one of the Reason* constants. Empty when
	// Allowed.
	Reason string
	// RetryAt is when it is worth asking again. Always in the future on a
	// refusal: the next paced slot, or the next window's reset.
	RetryAt time.Time
	// Window is the session window this decision was made in.
	Window Window
	// Allowance is how many dispatches the window could still admit at the
	// moment of the decision (before this one was counted).
	Allowance int
	// Dispatched is the window's consumption after this decision -- including
	// this dispatch when it was allowed, so a caller can record it without
	// re-deriving it.
	Dispatched int
	// Next is the state to persist. It is only meaningful when Allowed; a
	// refusal changes nothing, which is what makes asking cheap and
	// idempotent.
	Next State
}

// Decide answers whether a dispatch may go at now, given what this scope has
// already consumed.
//
// It is a pure function and it never mutates State: the caller persists
// Decision.Next if and only if the dispatch was allowed. A refusal is not an
// error and leaves no trace -- a work item that is deferred and asks again in
// five minutes must not have spent anything by asking.
func (c Config) Decide(now time.Time, s State) Decision {
	if !c.Enabled() {
		// Pacing off: everything goes, and nothing is recorded. Next is
		// deliberately the zero state rather than a counted one, so turning
		// pacing on later starts from an honest zero rather than from a
		// counter that was accumulating while nothing was enforcing it.
		return Decision{Allowed: true}
	}

	now = now.UTC()
	w := c.WindowAt(now)

	// A window roll is a comparison, not a sweep. State from an older window
	// counts as nothing here; state from a LATER window (a clock that went
	// backwards, or a config whose anchor moved) is left alone rather than
	// trusted -- treating it as consumption of this window would be
	// arbitrary, and treating it as zero would let a backwards clock hand out
	// a second full budget.
	dispatched := s.Dispatched
	switch {
	case s.WindowStart.IsZero():
		dispatched = 0
	case s.WindowStart.Before(w.Start):
		dispatched = 0
	}
	if dispatched < 0 {
		dispatched = 0
	}

	allowance := c.Allowance(now, dispatched)
	if allowance <= 0 {
		reason := ReasonWindowCapacity
		if dispatched >= c.Limit {
			// The budget bound first: say so, because "come back after the
			// reset" and "this window ran out of room" are different facts
			// even though they share a retry instant.
			reason = ReasonWindowBudget
		}
		return Decision{
			Reason:     reason,
			RetryAt:    w.End,
			Window:     w,
			Allowance:  0,
			Dispatched: dispatched,
			Next:       s,
		}
	}

	// Spacing. The stored slot is honored whatever window it came from: by
	// construction it never lands beyond the end of the window it was
	// computed in (Spacing divides the remaining window by a positive
	// allowance), so a slot carried across a roll is already in the past and
	// this comparison is a no-op there.
	if !s.NextDispatchAt.IsZero() && now.Before(s.NextDispatchAt) {
		return Decision{
			Reason:     ReasonSpacing,
			RetryAt:    s.NextDispatchAt,
			Window:     w,
			Allowance:  allowance,
			Dispatched: dispatched,
			Next:       s,
		}
	}

	spacing := c.Spacing(now, dispatched)
	return Decision{
		Allowed:    true,
		Window:     w,
		Allowance:  allowance,
		Dispatched: dispatched + 1,
		Next: State{
			WindowStart:    w.Start,
			Dispatched:     dispatched + 1,
			NextDispatchAt: now.Add(spacing),
		},
	}
}

// floorDiv divides towards negative infinity, which is what tiling a window
// off an anchor needs: an instant one minute before the anchor belongs to the
// window that ENDS at the anchor, not to the one that starts there.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
