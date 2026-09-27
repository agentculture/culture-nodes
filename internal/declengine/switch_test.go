package declengine

import (
	"context"
	"errors"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

func TestValidMode(t *testing.T) {
	for _, m := range []string{ModeBefore, ModeShadow, ModeAfter} {
		if !ValidMode(m) {
			t.Errorf("ValidMode(%q) = false, want true", m)
		}
	}
	if ValidMode("sideways") {
		t.Fatal("ValidMode accepted an unknown mode")
	}
}

// memorySwitch is a minimal in-process SwitchStore for tests that only need
// ShadowGate's routing decision, not the persisted history.
type memorySwitch struct {
	mode string
	err  error
}

func (m *memorySwitch) Mode(context.Context, string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	if m.mode == "" {
		return ModeBefore, nil
	}
	return m.mode, nil
}

func (m *memorySwitch) Flip(_ context.Context, _ string, mode string, _ string, _ string, _ ...FlipHook) (string, error) {
	previous := m.mode
	if previous == "" {
		previous = ModeBefore
	}
	m.mode = mode
	return previous, nil
}

type memoryGraphRuns map[string]string // eventID -> runID

func (m memoryGraphRuns) RunForEvent(_ context.Context, _, eventID string) (string, error) {
	return m[eventID], nil
}

type memoryShadowLineage struct {
	byFiring map[string]string // firingID -> graphRunID
	byRun    map[string]string // graphRunID -> firingID
}

func newMemoryShadowLineage() *memoryShadowLineage {
	return &memoryShadowLineage{byFiring: map[string]string{}, byRun: map[string]string{}}
}

func (m *memoryShadowLineage) RecordShadowLineage(_ context.Context, _, firingID, graphRunID string) error {
	if firingID == "" || graphRunID == "" {
		return errors.New("missing identity")
	}
	if _, ok := m.byFiring[firingID]; ok {
		return nil
	}
	m.byFiring[firingID] = graphRunID
	m.byRun[graphRunID] = firingID
	return nil
}

func (m *memoryShadowLineage) GraphRunFiring(_ context.Context, _, graphRunID string) (string, error) {
	return m.byRun[graphRunID], nil
}

// Acceptance 2: in shadow (and before) the declaration engine dispatches
// zero actions -- the underlying dispatcher is never called -- and the
// would-fire firing's graph run is recorded so a later firing's lineage can
// derive from it.
func TestShadowGateDispatchesZeroActionsInShadow(t *testing.T) {
	calls := 0
	underlying := dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		calls++
		return DispatchResult{}, nil
	})
	runs := memoryGraphRuns{"event-1": "graph-run-1"}
	lineage := newMemoryShadowLineage()
	sw := &memorySwitch{mode: ModeShadow}
	gate := ShadowGate{Switch: sw, Underlying: underlying, GraphRuns: runs, ShadowLine: lineage}

	req := DispatchRequest{Firing: postgres.DeclarationFiring{ID: "firing-1", NamespaceID: "ns", EventID: "event-1"}}
	result, err := gate.Dispatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("underlying dispatcher called %d times in shadow, want 0", calls)
	}
	if result.ArtifactID != "" || result.Async || len(result.Variables) != 0 {
		t.Fatalf("shadow dispatch result not empty: %+v", result)
	}
	if got := lineage.byFiring["firing-1"]; got != "graph-run-1" {
		t.Fatalf("shadow lineage firing-1 -> %q, want graph-run-1", got)
	}
	if got, _ := lineage.GraphRunFiring(context.Background(), "ns", "graph-run-1"); got != "firing-1" {
		t.Fatalf("GraphRunFiring(graph-run-1) = %q, want firing-1", got)
	}
}

// The same zero-dispatch guarantee holds for 'before', the default mode a
// namespace that has never flipped reads as.
func TestShadowGateDispatchesZeroActionsBeforeFlip(t *testing.T) {
	calls := 0
	underlying := dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		calls++
		return DispatchResult{}, nil
	})
	gate := ShadowGate{Switch: &memorySwitch{}, Underlying: underlying}
	req := DispatchRequest{Firing: postgres.DeclarationFiring{ID: "firing-1", NamespaceID: "ns", EventID: "event-1"}}
	if _, err := gate.Dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("underlying dispatcher called %d times in before, want 0", calls)
	}
}

// Flipping to 'after' produces exactly one real dispatch: the switch's
// whole point, per the spec's shadow-parity instruction.
func TestShadowGateDispatchesAfterFlip(t *testing.T) {
	calls := 0
	underlying := dispatchFunc(func(_ context.Context, r DispatchRequest) (DispatchResult, error) {
		calls++
		return DispatchResult{ArtifactID: "pr-1"}, nil
	})
	sw := &memorySwitch{mode: ModeAfter}
	gate := ShadowGate{Switch: sw, Underlying: underlying}
	req := DispatchRequest{Firing: postgres.DeclarationFiring{ID: "firing-1", NamespaceID: "ns", EventID: "event-1"}}
	result, err := gate.Dispatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("underlying dispatcher called %d times in after, want exactly 1", calls)
	}
	if result.ArtifactID != "pr-1" {
		t.Fatalf("after dispatch result = %+v, want the underlying result passed through", result)
	}
}

func TestShadowGateRequiresUnderlyingForAfter(t *testing.T) {
	gate := ShadowGate{Switch: &memorySwitch{mode: ModeAfter}}
	req := DispatchRequest{Firing: postgres.DeclarationFiring{ID: "f", NamespaceID: "ns", EventID: "e"}}
	if _, err := gate.Dispatch(context.Background(), req); err == nil {
		t.Fatal("expected an error dispatching 'after' with no underlying dispatcher")
	}
}

// A shadow firing whose event has no graph-engine counterpart yet still
// dispatches zero actions; the mapping is opportunistic, not required.
func TestShadowGateToleratesMissingGraphRun(t *testing.T) {
	gate := ShadowGate{Switch: &memorySwitch{mode: ModeShadow}, GraphRuns: memoryGraphRuns{}, ShadowLine: newMemoryShadowLineage()}
	req := DispatchRequest{Firing: postgres.DeclarationFiring{ID: "firing-1", NamespaceID: "ns", EventID: "no-run-yet"}}
	if _, err := gate.Dispatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestShadowGatePrepareOriginSkipsBeforeAfter(t *testing.T) {
	calls := 0
	underlying := fakePreparer{fn: func() { calls++ }}
	gate := ShadowGate{Switch: &memorySwitch{mode: ModeShadow}, Underlying: underlying}
	if err := gate.PrepareOrigin(context.Background(), OriginEvent{NamespaceID: "ns"}, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("PrepareOrigin called the underlying preparer %d times in shadow, want 0", calls)
	}

	gate.Switch = &memorySwitch{mode: ModeAfter}
	if err := gate.PrepareOrigin(context.Background(), OriginEvent{NamespaceID: "ns"}, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("PrepareOrigin called the underlying preparer %d times in after, want 1", calls)
	}
}

type fakePreparer struct {
	fn func()
}

func (f fakePreparer) Dispatch(context.Context, DispatchRequest) (DispatchResult, error) {
	return DispatchResult{}, nil
}

func (f fakePreparer) PrepareOrigin(context.Context, OriginEvent, *MarkerService) error {
	f.fn()
	return nil
}
