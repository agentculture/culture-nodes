package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// probeHandler is a DeliveredEventHandler that, at the moment it is called,
// looks at the database from OTHER connections: the event must already be
// visible (so the delivery committed) and the locks the delivery held must be
// free (so nothing the handler does can wait on them). A handler called from
// inside the delivery transaction fails both.
type probeHandler struct {
	t          *testing.T
	s          *postgres.Store
	lockKey    string // advisory lock name the delivery took, if any
	scheduleID string // schedule row FireSchedule locked, if any
	calls      []postgres.SignalDelivery
	err        error
}

func (p *probeHandler) HandleDeliveredEvent(ctx context.Context, d postgres.SignalDelivery) error {
	p.t.Helper()
	p.calls = append(p.calls, d)
	var visible int
	if err := p.s.Pool().QueryRow(ctx, `SELECT count(*) FROM signal_events WHERE id=$1`, d.Event.ID).Scan(&visible); err != nil {
		p.t.Fatal(err)
	}
	if visible != 1 {
		p.t.Errorf("handler ran before the delivery committed: event %s visible %d times from another connection", d.Event.ID, visible)
	}
	if p.lockKey != "" {
		conn, err := p.s.Pool().Acquire(ctx)
		if err != nil {
			p.t.Fatal(err)
		}
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, p.lockKey).Scan(&got); err != nil {
			p.t.Fatal(err)
		}
		if got {
			_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, p.lockKey)
		} else {
			p.t.Errorf("handler ran while the delivery still held advisory lock %q", p.lockKey)
		}
		conn.Release()
	}
	if p.scheduleID != "" {
		tx, err := p.s.Pool().Begin(ctx)
		if err != nil {
			p.t.Fatal(err)
		}
		var id string
		if err := tx.QueryRow(ctx, `SELECT id FROM schedules WHERE id=$1 FOR UPDATE NOWAIT`, p.scheduleID).Scan(&id); err != nil {
			p.t.Errorf("handler ran while FireSchedule still held the schedule row lock: %v", err)
		}
		_ = tx.Rollback(ctx)
	}
	return p.err
}

// Task t38, acceptance 1: the declaration handler runs after the delivery
// commits and holds none of its locks; a redelivery the watermark answers as
// a duplicate is offered again (Handle dedupes on the event id) with the
// caller's subject restored; a handler error never fails the delivery.
func TestDeliveredEventHandlerRunsAfterCommitWithoutTheDeliveryLocks(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "declhook")
	probe := &probeHandler{t: t, s: s, lockKey: "signal-watermark:" + ns.ID + ":test:source"}

	in := postgres.DeliverSignalEventInput{
		NamespaceID: ns.ID, Name: "pr-upkeep.pr", Emitter: "test", Subject: "ISSUE-7",
		SourceKey: "test:source", Watermark: json.RawMessage(`{"seq":"1"}`),
		Declarations: probe,
	}
	first, err := s.DeliverSignalEvent(ctx, in)
	if err != nil || first.DeclarationErr != nil {
		t.Fatalf("deliver: err=%v declarationErr=%v", err, first.DeclarationErr)
	}
	if len(probe.calls) != 1 || probe.calls[0].Event.ID != first.Event.ID || probe.calls[0].Event.Subject != "ISSUE-7" {
		t.Fatalf("handler calls = %+v, want one call for %s carrying subject ISSUE-7", probe.calls, first.Event.ID)
	}

	again, err := s.DeliverSignalEvent(ctx, in)
	if err != nil || !again.Duplicate || again.Event.ID != first.Event.ID {
		t.Fatalf("redelivery: %+v err=%v, want the same event as a duplicate", again, err)
	}
	if len(probe.calls) != 2 || !probe.calls[1].Duplicate || probe.calls[1].Event.Subject != "ISSUE-7" {
		t.Fatalf("redelivery was not offered with its subject restored: %+v", probe.calls)
	}

	probe.err = errors.New("declaration engine unavailable")
	in.Watermark = json.RawMessage(`{"seq":"2"}`)
	failed, err := s.DeliverSignalEvent(ctx, in)
	if err != nil {
		t.Fatalf("a declaration failure failed the delivery: %v", err)
	}
	if failed.Event.ID == "" || failed.DeclarationErr == nil {
		t.Fatalf("delivery = %+v, want a committed event with DeclarationErr carried back", failed)
	}

	// No handler: exactly the pre-t38 delivery.
	plain, err := s.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{NamespaceID: ns.ID, Name: "pr-upkeep.pr", Emitter: "test"})
	if err != nil || plain.DeclarationErr != nil || len(probe.calls) != 3 {
		t.Fatalf("handler-less delivery: err=%v declErr=%v calls=%d", err, plain.DeclarationErr, len(probe.calls))
	}
}

// Task t38, acceptance 1 for schedules: a fired schedule's event reaches the
// handler only after FireSchedule's own transaction (which holds the
// schedule row lock) has committed.
func TestDeliveredEventHandlerRunsAfterFireScheduleCommits(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "declhook-sched")
	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	sc := mustSchedule(t, s, postgres.CreateScheduleInput{
		NamespaceID: ns.ID, Name: "decl-timer", EventName: "timer",
		Payload: json.RawMessage(`{}`), Interval: time.Hour, FirstFireAt: base,
	})
	probe := &probeHandler{t: t, s: s, scheduleID: sc.ID}
	res, err := s.FireSchedule(ctx, postgres.FireScheduleInput{ScheduleID: sc.ID, Now: base, Declarations: probe})
	if err != nil || !res.Fired {
		t.Fatalf("FireSchedule: %+v err=%v", res, err)
	}
	if len(probe.calls) != 1 || probe.calls[0].Event.ID != res.Delivery.Event.ID {
		t.Fatalf("handler calls = %+v, want exactly the fired event %s", probe.calls, res.Delivery.Event.ID)
	}
	// Not due again: nothing fired, nothing offered.
	if _, err := s.FireSchedule(ctx, postgres.FireScheduleInput{ScheduleID: sc.ID, Now: base, Declarations: probe}); err != nil {
		t.Fatal(err)
	}
	if len(probe.calls) != 1 {
		t.Fatalf("a schedule that did not fire offered %d events", len(probe.calls)-1)
	}
}
