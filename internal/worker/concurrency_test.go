package worker

import "testing"

// Pure arithmetic/config tests for ConcurrencyOptions (task t16, issue
// #166's second half) -- no database needed, the same split
// internal/pacing's own unit tests keep from pacing.go's enforcement tests.
// package worker (not worker_test) on purpose: forActor is unexported, and
// its "opt an actor out with a non-positive override" branch is worth
// pinning directly rather than only through integration.
//
// The enforcement half (atActorCapacity/deferForCapacity against real
// actor_invocations rows) is proved in
// internal/store/postgres/actorconcurrency_test.go, at the query these
// unexported methods are thin wrappers over -- see that file's doc comment
// for why the full end-to-end dispatch path is not exercised here: it needs
// a DBRegistry-attributed actor row id (actor_invocations.actor_id), which
// this package's shared harness (worker_test.go's newHarness) deliberately
// never resolves, the same documented gap every other ActorRowID-dependent
// feature here has (see registry.go's actorRowIDResolver).

func TestConcurrencyOptionsEnabled(t *testing.T) {
	cases := []struct {
		name string
		opts ConcurrencyOptions
		want bool
	}{
		{"zero value", ConcurrencyOptions{}, false},
		{"default only", ConcurrencyOptions{ActorDefault: 1}, true},
		{"non-positive default, no overrides", ConcurrencyOptions{ActorDefault: 0}, false},
		{"negative default", ConcurrencyOptions{ActorDefault: -1}, false},
		{"an override alone", ConcurrencyOptions{ActorOverrides: map[string]int{"company/x": 2}}, true},
		{"only non-positive overrides", ConcurrencyOptions{ActorOverrides: map[string]int{"company/x": 0, "company/y": -1}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.opts.Enabled(); got != c.want {
				t.Errorf("Enabled() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestConcurrencyOptionsForActor(t *testing.T) {
	opts := ConcurrencyOptions{
		ActorDefault:   3,
		ActorOverrides: map[string]int{"company/pinned": 1, "company/unbounded": 0},
	}

	if got := opts.forActor("company/unlisted"); got != 3 {
		t.Errorf("forActor(unlisted) = %d, want the default 3", got)
	}
	if got := opts.forActor("company/pinned"); got != 1 {
		t.Errorf("forActor(pinned) = %d, want its override 1", got)
	}
	// A key present with a non-positive override opts OUT of the default
	// entirely -- "cap everything except this one" -- PacingOptions'
	// identical escape hatch.
	if got := opts.forActor("company/unbounded"); got != 0 {
		t.Errorf("forActor(unbounded) = %d, want its override 0 (opted out of the default)", got)
	}
}
