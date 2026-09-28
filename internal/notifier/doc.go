// Package notifier is the testable core of cmd/nodes-notifier: an
// out-of-process daemon that consumes the control plane's cross-run SSE
// feed (GET /v1alpha1/events) and turns run-lifecycle events into
// internal/notify webhook deliveries.
//
// # Composition, not modification
//
// This package composes two things it never changes: the control plane's
// public HTTP surface (GET /v1alpha1/events, GET /v1alpha1/runs/{id}, GET
// /v1alpha1/workflows/{digest} -- read over plain net/http, no Go-level
// dependency on internal/api, internal/engine, or internal/worker) and
// internal/notify's already-built, already-tested webhook transport
// (ResolveWebhook/BuildMessage/Post, composed here through the single
// Notify entry point). Everything new in this package is the SSE consumer
// (sse.go), the durable cursor (cursor.go), the run-detail fetch and the
// cached digest->workflow-name lookup (rundetail.go), the lifecycle filter
// (lifecycle.go), and the glue between one committed event and one
// notify.Payload (daemon.go).
//
// # Zero control-plane footprint
//
// The daemon only ever issues GET requests against the control plane. It
// holds no database connection, no control-plane credential, and never
// calls any control-plane write endpoint -- a webhook outage, a run-detail
// fetch failure, or this daemon crashing outright cannot affect dispatch,
// the engine, or the SSE feed itself, matching the economy-discord-graphs
// honesty condition "a webhook outage never stalls dispatch or the SSE
// feed."
//
// # Skip-list
//
// Config.SkipWorkflows (NODES_NOTIFIER_SKIP_WORKFLOWS in cmd/nodes-notifier)
// mutes named workflows: once handleFrame knows a run's workflow key it
// checks the list (workflowSkipped, lifecycle.go) for every lifecycle event
// type alike, and a match is consumed -- the cursor advances past it -- and
// journaled as OutcomeSkipped instead of reaching notify.Notify. A run a
// declaration fired is matched by its declaration name, the key the run
// view's firing carries and the notification would show, not by the
// "decl-<declaration id>" name of the one-node envelope workflow
// internal/declengine compiles for it.
//
// # Durable cursor, mark-before-deliver
//
// See Cursor's doc comment (cursor.go) for the restart guarantee this
// package makes: a crash can cause this daemon to miss a notification, but
// never to duplicate one, because every mutation to the on-disk cursor
// happens before the webhook attempt it corresponds to, not after.
package notifier
