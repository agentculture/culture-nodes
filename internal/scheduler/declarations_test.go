package scheduler_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/scheduler"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// Task t38 (#328), acceptance 2: the declaration engine's periodic half runs
// inside the existing scheduler -- on becoming active, on every tick, at the
// scheduler's own (caller-supplied) clock -- and the scheduler's schedule
// fires reach the declaration engine after they commit.

type fakeDeclarations struct {
	mu        sync.Mutex
	drives    []time.Time
	delivered []postgres.SignalDelivery
}

func (f *fakeDeclarations) Drive(_ context.Context, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drives = append(f.drives, now)
	return nil
}

func (f *fakeDeclarations) HandleDeliveredEvent(_ context.Context, d postgres.SignalDelivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, d)
	return nil
}

func (f *fakeDeclarations) driveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.drives)
}

func TestTickDrivesDeclarationsAtTheSchedulersClockAndOffersScheduleFires(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "sched-decl")
	clock := &fakeClock{at: time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)}
	fake := &fakeDeclarations{}
	sch := scheduler.New(s, scheduler.Options{Now: clock.now, Declarations: fake})

	sc := mustSchedule(t, s, postgres.CreateScheduleInput{
		NamespaceID: ns.ID, Name: "decl-timer", EventName: "timer",
		Payload: json.RawMessage(`{}`), Interval: time.Hour, FirstFireAt: clock.at,
	})
	if err := sch.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(fake.drives) != 1 || !fake.drives[0].Equal(clock.at) {
		t.Fatalf("Drive calls = %v, want one at the scheduler's clock %s", fake.drives, clock.at)
	}
	var fired string
	if err := s.Pool().QueryRow(ctx, `SELECT last_event_id FROM schedules WHERE id=$1`, sc.ID).Scan(&fired); err != nil {
		t.Fatal(err)
	}
	if len(fake.delivered) != 1 || fake.delivered[0].Event.ID != fired {
		t.Fatalf("schedule fire offered %+v, want the fired event %s", fake.delivered, fired)
	}

	clock.at = clock.at.Add(90 * time.Second)
	if err := sch.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.drives) != 2 || !fake.drives[1].Equal(clock.at) {
		t.Fatalf("second tick Drive calls = %v, want the advanced clock %s", fake.drives, clock.at)
	}
}

// On becoming active the scheduler drives declarations immediately, not
// after its first tick interval -- a thaw a crashed predecessor left
// half-done is replayed on startup.
func TestRunDrivesDeclarationsOnBecomingActive(t *testing.T) {
	s := requireStore(t)
	fake := &fakeDeclarations{}
	sch := scheduler.New(s, scheduler.Options{TickInterval: time.Hour, Declarations: fake})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sch.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for fake.driveCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if n := fake.driveCount(); n != 1 {
		t.Fatalf("Drive calls on becoming active (tick interval 1h) = %d, want exactly 1", n)
	}
}

type noopDispatcher struct{}

func (noopDispatcher) Dispatch(context.Context, declengine.DispatchRequest) (declengine.DispatchResult, error) {
	return declengine.DispatchResult{}, nil
}

// End to end with the real Driver: a landing node's deadline expires when
// the SCHEDULER's clock passes it, not the wall clock.
func TestTickExpiresDeclarationNodesOnTheSchedulersClock(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "sched-decl-expiry").ID
	d := decl.Declaration{Name: "expiring", Condition: "true",
		Trigger:     decl.Trigger{Kind: "pr-upkeep.pr", ReentryLimit: 3, HopLimit: 20, RateCeiling: "30/h"},
		Action:      decl.Action{Kind: "agent.work", With: json.RawMessage(`{"uses":"actor://test"}`)},
		StartNode:   decl.Node{Name: declengine.RootNode, Deadline: "none"},
		LandingNode: decl.Node{Name: "waiting", Deadline: "1h"}}
	body, err := d.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.PublishDeclaration(ctx, postgres.PublishDeclarationInput{NamespaceID: ns, Name: d.Name, Body: body, Author: "human"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDeclarationActivation(ctx, ns, v.ID, "activate", "human", ""); err != nil {
		t.Fatal(err)
	}
	sw := declengine.PostgresSwitchStore{Store: s}
	if _, err := sw.Flip(ctx, ns, declengine.ModeShadow, "human:ops", "shadow"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TCA_SCHED_KEY", strings.Repeat("c", 32))
	eng, err := declengine.New(declengine.Config{MarkerKeyEnv: "TCA_SCHED_KEY"}, declengine.PostgresBackend{Store: s}, declengine.PostgresMarkerStore{Store: s},
		declengine.ShadowGate{Switch: sw, Underlying: noopDispatcher{}})
	if err != nil {
		t.Fatal(err)
	}
	driver := declengine.Driver{Engine: eng, Store: s}
	ev, err := s.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{NamespaceID: ns, Name: "pr-upkeep.pr", Emitter: "test", Declarations: driver})
	if err != nil || ev.DeclarationErr != nil {
		t.Fatalf("deliver: err=%v declarationErr=%v", err, ev.DeclarationErr)
	}
	state := func() (string, string) {
		t.Helper()
		var st, reason string
		if err := s.Pool().QueryRow(ctx, `SELECT n.state,COALESCE(n.closed_reason,'') FROM declaration_nodes n JOIN declaration_firings f ON f.namespace_id=n.namespace_id AND f.id=n.opening_firing_id
			WHERE n.namespace_id=$1 AND f.event_id=$2`, ns, ev.Event.ID).Scan(&st, &reason); err != nil {
			t.Fatalf("landing node for the delivered event: %v", err)
		}
		return st, reason
	}
	if st, _ := state(); st != declengine.NodeStateOpen {
		t.Fatalf("shadow delivery left the landing node %q, want open", st)
	}

	clock := &fakeClock{at: time.Now().UTC()}
	sch := scheduler.New(s, scheduler.Options{Now: clock.now, Declarations: driver})
	if err := sch.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := state(); st != declengine.NodeStateOpen {
		t.Fatalf("node closed before the scheduler's clock reached its deadline: %q", st)
	}
	clock.at = clock.at.Add(2 * time.Hour)
	if err := sch.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if st, reason := state(); st != declengine.NodeStateClosed || reason != declengine.NodeReasonExpired {
		t.Fatalf("node after the scheduler's clock passed its deadline = %s/%s, want closed/expired", st, reason)
	}
}
