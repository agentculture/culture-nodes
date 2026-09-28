package declengine

import (
	"context"
	"testing"
)

// Acceptance 1 (brief, one test per limit): a re-entry beyond the trigger's
// declared limit stops the chain with a visible loop-limited record;
// nothing is silently dropped (h32).
func TestReentryLimitStopsChainVisibly(t *testing.T) {
	a := active("A")
	a.Declaration.Trigger.ReentryLimit = 1
	a.Declaration.Trigger.AllowSelfRetrigger = true
	m := &memoryBackend{}
	calls := 0
	e := newTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { calls++; return DispatchResult{}, nil }))
	// Two logical ancestors of A: this firing would be its third appearance,
	// one beyond reentry_limit 1 (N re-entries allowed => N+1 appearances).
	lineage := []Ancestor{{FiringID: "b", CanonicalID: "b", DeclarationID: "A"}, {FiringID: "c", CanonicalID: "c", DeclarationID: "A"}}
	ev := Event{NamespaceID: "ns", ID: "e", Variables: map[string]any{"priority": "High"}}
	if err := e.evaluate(context.Background(), ev, a, "b", lineage); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("dispatched past the reentry limit")
	}
	last := m.steps[len(m.steps)-1]
	if last.Outcome != OutcomeLoopLimited || last.Reason == "" {
		t.Fatalf("evaluation %+v, want a visible loop-limited record", last)
	}
}

// Reentry_limit 0 means no re-entry at all: a declaration that already
// appears once in its own lineage may never fire again.
func TestReentryLimitZeroMeansNoReentry(t *testing.T) {
	a := active("A")
	a.Declaration.Trigger.ReentryLimit = 0
	a.Declaration.Trigger.AllowSelfRetrigger = true
	m := &memoryBackend{}
	calls := 0
	e := newTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { calls++; return DispatchResult{}, nil }))
	lineage := []Ancestor{{FiringID: "b", CanonicalID: "b", DeclarationID: "A"}}
	ev := Event{NamespaceID: "ns", ID: "e", Variables: map[string]any{"priority": "High"}}
	if err := e.evaluate(context.Background(), ev, a, "b", lineage); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("dispatched with reentry_limit 0 and an existing appearance")
	}
	if last := m.steps[len(m.steps)-1]; last.Outcome != OutcomeLoopLimited {
		t.Fatalf("outcome %q, want %q", last.Outcome, OutcomeLoopLimited)
	}
}

// Acceptance 1: hop limit 20 (c93) bounds total lineage depth regardless of
// which declarations appear in it, and stops the chain with a visible
// record.
func TestHopLimitStopsChainVisibly(t *testing.T) {
	a := active("A")
	a.Declaration.Trigger.HopLimit = 2
	a.Declaration.Trigger.ReentryLimit = 100
	m := &memoryBackend{}
	calls := 0
	e := newTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { calls++; return DispatchResult{}, nil }))
	lineage := []Ancestor{{FiringID: "x", CanonicalID: "x", DeclarationID: "X"}, {FiringID: "y", CanonicalID: "y", DeclarationID: "Y"}}
	ev := Event{NamespaceID: "ns", ID: "e", Variables: map[string]any{"priority": "High"}}
	if err := e.evaluate(context.Background(), ev, a, "x", lineage); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("dispatched past the hop limit")
	}
	last := m.steps[len(m.steps)-1]
	if last.Outcome != OutcomeLoopLimited || last.Reason == "" {
		t.Fatalf("evaluation %+v, want a visible loop-limited record", last)
	}
	// One hop under the limit still fires.
	m2 := &memoryBackend{}
	e2 := newTestEngine(t, m2, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { calls++; return DispatchResult{}, nil }))
	if err := e2.evaluate(context.Background(), ev, a, "x", lineage[:1]); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("under the hop limit did not fire")
	}
}

// Acceptance 1: a declaration may not directly retrigger itself unless it
// opts in (c93); the default is a visible stop, not a silent drop.
func TestSelfRetriggerBlockedUnlessOptedIn(t *testing.T) {
	a := active("A")
	m := &memoryBackend{}
	calls := 0
	e := newTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { calls++; return DispatchResult{}, nil }))
	lineage := []Ancestor{{FiringID: "prev", CanonicalID: "prev", DeclarationID: "A"}}
	ev := Event{NamespaceID: "ns", ID: "e", Variables: map[string]any{"priority": "High"}}
	if err := e.evaluate(context.Background(), ev, a, "prev", lineage); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("self-retrigger dispatched without opting in")
	}
	last := m.steps[len(m.steps)-1]
	if last.Outcome != OutcomeLoopLimited || last.Reason == "" {
		t.Fatalf("evaluation %+v, want a visible loop-limited record", last)
	}

	// Opted in: the identical lineage now fires.
	a.Declaration.Trigger.AllowSelfRetrigger = true
	m2 := &memoryBackend{}
	e2 := newTestEngine(t, m2, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { calls++; return DispatchResult{}, nil }))
	if err := e2.evaluate(context.Background(), ev, a, "prev", lineage); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("opted-in self-retrigger did not fire")
	}

	// A parent from a DIFFERENT declaration is never a self-retrigger,
	// regardless of the flag.
	a.Declaration.Trigger.AllowSelfRetrigger = false
	m3 := &memoryBackend{}
	e3 := newTestEngine(t, m3, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { calls++; return DispatchResult{}, nil }))
	other := []Ancestor{{FiringID: "prev", CanonicalID: "prev", DeclarationID: "B"}}
	if err := e3.evaluate(context.Background(), ev, a, "prev", other); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("a different predecessor was blocked as self-retrigger")
	}
}

// Acceptance 1: 30 firings/declaration/hour by default (and whatever a
// declaration overrides it to); the 31st (or Nth+1) is stopped with a
// visible record instead of dispatching.
func TestRateCeilingStopsChainVisibly(t *testing.T) {
	a := active("A")
	a.Declaration.Trigger.RateCeiling = "2/h"
	m := &memoryBackend{recentFirings: map[string]int{"A": 2}}
	calls := 0
	e := newTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { calls++; return DispatchResult{}, nil }))
	ev := Event{NamespaceID: "ns", ID: "e", Variables: map[string]any{"priority": "High"}}
	if err := e.evaluate(context.Background(), ev, a, "", nil); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("dispatched at the rate ceiling")
	}
	last := m.steps[len(m.steps)-1]
	if last.Outcome != OutcomeLoopLimited || last.Reason == "" {
		t.Fatalf("evaluation %+v, want a visible loop-limited record", last)
	}

	// Under the ceiling still fires.
	m2 := &memoryBackend{recentFirings: map[string]int{"A": 1}}
	e2 := newTestEngine(t, m2, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { calls++; return DispatchResult{}, nil }))
	if err := e2.evaluate(context.Background(), ev, a, "", nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("under the rate ceiling did not fire")
	}
}

// Acceptance 2: a burst of N events on one subject with cap K yields at
// most K in-flight firings; the rest are deferred (never dropped) and the
// deferral is visible as its own outcome, distinct from loop-limited.
func TestSubjectConcurrencyDefersBeyondCap(t *testing.T) {
	a := active("A")
	a.Declaration.Trigger.MaxConcurrentSubject = 2
	m := &memoryBackend{inFlight: map[string]int{"A/ISSUE-1": 2}}
	calls := 0
	e := newTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { calls++; return DispatchResult{}, nil }))
	ev := Event{NamespaceID: "ns", ID: "e", Subject: "ISSUE-1", Variables: map[string]any{"priority": "High"}}
	if err := e.evaluate(context.Background(), ev, a, "", nil); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || len(m.claims) != 0 {
		t.Fatalf("dispatched or claimed at the subject concurrency cap: calls=%d claims=%d", calls, len(m.claims))
	}
	last := m.steps[len(m.steps)-1]
	if last.Outcome != OutcomeDeferred || last.Reason == "" {
		t.Fatalf("evaluation %+v, want a visible deferred record", last)
	}
	if len(m.deferred) != 1 || m.deferred[0].event.ID != "e" {
		t.Fatalf("deferred queue = %+v", m.deferred)
	}

	// A second event for the SAME subject, while still queued, collapses
	// onto the one remembered entry (the replace rule) rather than adding a
	// sibling.
	ev2 := ev
	ev2.ID = "e2"
	if err := e.evaluate(context.Background(), ev2, a, "", nil); err != nil {
		t.Fatal(err)
	}
	if len(m.deferred) != 1 || m.deferred[0].event.ID != "e2" {
		t.Fatalf("deferred queue after replace = %+v", m.deferred)
	}

	// Under the cap still fires (and claims), and never touches the queue.
	m2 := &memoryBackend{inFlight: map[string]int{"A/ISSUE-1": 1}}
	e2ng := newTestEngine(t, m2, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { calls++; return DispatchResult{}, nil }))
	if err := e2ng.evaluate(context.Background(), ev, a, "", nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(m2.deferred) != 0 {
		t.Fatalf("under the cap: calls=%d deferred=%+v", calls, m2.deferred)
	}
}

// A declaration that never sets max_concurrent_subject, or an event that
// carries no subject, is entirely unaffected: pre-t10 behavior.
func TestSubjectConcurrencyNoOpWithoutCapOrSubject(t *testing.T) {
	a := active("A")
	for _, tc := range []struct {
		name    string
		cap     int
		subject string
	}{{"no cap declared", 0, "ISSUE-1"}, {"no subject on event", 3, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			d := a
			d.Declaration.Trigger.MaxConcurrentSubject = tc.cap
			m := &memoryBackend{inFlight: map[string]int{"A/" + tc.subject: 999}}
			calls := 0
			e := newTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { calls++; return DispatchResult{}, nil }))
			ev := Event{NamespaceID: "ns", ID: "e", Subject: tc.subject, Variables: map[string]any{"priority": "High"}}
			if err := e.evaluate(context.Background(), ev, d, "", nil); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d, want 1 (no-op guard)", calls)
			}
		})
	}
}

// DrainSubject replays the oldest queued entry through the ordinary Handle
// path and removes it from the queue; a namespace/declaration with nothing
// queued is a no-op.
func TestDrainSubjectReplaysOldestQueuedEntry(t *testing.T) {
	a := active("A")
	m := &memoryBackend{active: []ActiveDeclaration{a}}
	calls := 0
	e := newTestEngine(t, m, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) { calls++; return DispatchResult{}, nil }))
	if err := e.DrainSubject(context.Background(), "ns", "A"); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("drained with nothing queued")
	}
	ev := Event{NamespaceID: "ns", ID: "queued", Kind: "pr-upkeep.pr", Node: "ready", Variables: map[string]any{"priority": "High"}}
	if err := m.DeferSubject(context.Background(), DeferSubjectInput{NamespaceID: "ns", DeclarationID: "A", Subject: "ISSUE-1", Event: ev}); err != nil {
		t.Fatal(err)
	}
	if err := e.DrainSubject(context.Background(), "ns", "A"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1 after drain", calls)
	}
	if len(m.deferred) != 0 {
		t.Fatalf("drained entry still queued: %+v", m.deferred)
	}
}
