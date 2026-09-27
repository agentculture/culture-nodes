package postgres

import "context"

// DeliveredEventHandler is the declaration engine's seat on the delivery
// path (task t38, #328). internal/declengine implements it (its Router); this
// package names only the shape, because declengine already imports postgres
// and the dependency can run one way only.
//
// WHERE THE CALL HAPPENS, and why there. A delivery runs inside one
// transaction that holds advisory locks on every run it resumes (see
// matchUnderRunLocks) and, for a keyed source, the watermark lock. The
// declaration engine's firing loop reads and writes through the pool in
// many small transactions of its own -- claims, evaluations, node closes,
// dispatches that create graph runs -- so calling it from inside that
// transaction would either deadlock against those locks or make a
// declaration firing depend on a delivery that could still roll back. The
// handler is therefore called only AFTER the delivery (or, for a schedule,
// FireSchedule's whole transaction) has committed, holding nothing.
//
// Idempotency is the engine's own: Handle claims a firing once per (event,
// declaration) under 0060's unique index, so a redelivery that the watermark
// answered as a duplicate is passed through too -- that is what lets a
// client retry recover a crash between the commit and this call.
//
// A handler error never fails the delivery: the fact is committed, the
// graph engine already acted on it, and the declaration engine records every
// evaluation failure on declaration_evaluations itself. The error is carried
// back on SignalDelivery.DeclarationErr for the caller to log.
type DeliveredEventHandler interface {
	HandleDeliveredEvent(ctx context.Context, delivery SignalDelivery) error
}

// notifyDelivered offers one committed delivery to h. A suppressed
// pre-cutover history fact appended nothing, so there is nothing to offer.
//
// subject is the delivery input's. SignalEvent.Subject is in-memory only
// (never persisted), so a duplicate delivery -- whose Event is re-read from
// signal_events -- comes back without it; restoring the caller's key keeps a
// redelivered event inside its declaration's per-subject concurrency cap
// instead of letting it bypass the cap by arriving subjectless.
func notifyDelivered(ctx context.Context, h DeliveredEventHandler, d SignalDelivery, subject string) error {
	if h == nil || d.Suppressed || d.Event.ID == "" {
		return nil
	}
	if d.Event.Subject == "" {
		d.Event.Subject = subject
	}
	return h.HandleDeliveredEvent(ctx, d)
}
