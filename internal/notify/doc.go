// Package notify is the Discord webhook transport for run-lifecycle
// notifications (economy-discord-graphs spec, requirement "Discord updates
// port devex's proven webhook design"). It is a standalone library: nothing
// in this package is wired into a binary. The out-of-process notifier
// daemon that calls it is a later task (economy-discord-graphs t14) — this
// package only has to be correct and hermetically testable on its own.
//
// # Ported, not imported
//
// This is a Go port of devex's proven design
// (/home/spark/git/devex/src/devex/core/webhook.py and
// /home/spark/git/devex/src/devex/commands/pr/scripts/_webhook.py): a
// content-agnostic HTTP transport (webhook.go) plus a payload-shaping
// layer (payload.go) that knows about Discord's embed format and nothing
// about ledger records, node output, or workflow input. devex is Python;
// this is Go. The design constraints below are carried over deliberately,
// not reinvented:
//
//   - the webhook URL is env-only, never in a config file or a log line,
//     because the URL itself embeds a bearer token;
//   - the POST is fail-open: one bounded attempt, no retries, and every
//     failure mode (bad scheme, redirect, non-2xx, timeout, transport
//     error) collapses to the same Failed outcome rather than propagating
//     an error a caller might let abort real work;
//   - a 3xx response is a failure, not something to follow — an
//     unfollowed redirect closes the SSRF/scheme-guard bypass a bounced
//     POST would otherwise open;
//   - outcomes are journaled without ever recording the URL or the
//     payload body.
//
// # Boundary: minimal metadata only
//
// Payload (payload.go) carries exactly five fields — run id, workflow
// name, event/state, actor, and a dashboard link — and nothing else. No
// ledger record data, no node output, no workflow input ever reaches this
// package, matching the events-carry-IDs-not-content rule
// (internal/events/doc.go). The payload-shaping tests in payload_test.go
// exist to keep that true by construction, not just by convention: they
// walk the built Discord/generic JSON and assert every string value
// traces back to one of Payload's five fields or a static label, never to
// anything else.
//
// # Hermetic tests, by construction
//
// TestMain (testmain_test.go) unsets CULTURE_NODES_WEBHOOK_URL and
// DISCORD_WEBHOOK_URL before any test in this package runs, so the suite
// can never pick up an ambient production webhook URL from the
// environment it happens to run in. The one test that talks to a real
// endpoint gates on a separate CULTURE_NODES_WEBHOOK_TEST_URL variable and
// is skipped whenever it is unset — which is every normal `go test` and
// every CI run, since nothing sets it there. This mirrors devex's own
// hermetic pattern (economy-discord-graphs non-goal: "the test suite
// follows devex's hermetic pattern").
package notify
