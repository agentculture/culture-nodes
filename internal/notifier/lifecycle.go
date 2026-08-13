package notifier

// lifecycleEventTypes is the run-lifecycle vocabulary this daemon posts
// about: run.created/completed/failed/cancelled/bounded, matching
// internal/api/events.go's own runLifecycleEventTypes (the event types the
// cross-run stream admits unconditionally regardless of active-run scope)
// and internal/engine/events.go:18-50 (where TypeRunFailed, TypeRunCancelled,
// and TypeRunBounded are declared -- TypeRunCreated and TypeRunCompleted
// come from internal/events/envelope.go, §15.1's own list).
//
// These five strings are duplicated here rather than imported from
// internal/engine on purpose: internal/engine pulls in the outbox relay
// (internal/events/relay.go, which links the PostgreSQL driver) and the
// rest of the control-plane's dependency graph, and this daemon's whole
// point is to be a small standalone process that talks to the control
// plane over HTTP only -- the same boundary internal/notify's own doc
// comment draws for the webhook transport. If this vocabulary ever grows
// or changes, internal/engine/events.go is the source of truth to update
// this list from.
var lifecycleEventTypes = map[string]bool{
	"dev.culture.nodes.run.created":   true,
	"dev.culture.nodes.run.completed": true,
	"dev.culture.nodes.run.failed":    true,
	"dev.culture.nodes.run.cancelled": true,
	"dev.culture.nodes.run.bounded":   true,
}

// isLifecycleEvent reports whether eventType is one this daemon notifies
// on. Everything else (attempt.started, ledger.record-appended, ...) is
// ignored -- acceptance criteria: "config may widen later; keep it
// simple."
func isLifecycleEvent(eventType string) bool {
	return lifecycleEventTypes[eventType]
}
