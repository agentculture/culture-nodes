package notify_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/notify"
)

// culturNodesWebhookTestURLEnv is the ONLY env var that can make this
// package's tests touch the network. It is deliberately not
// CULTURE_NODES_WEBHOOK_URL or DISCORD_WEBHOOK_URL — those are unset for
// the whole suite by TestMain — so there is no way to accidentally fire a
// real production webhook by running `go test` with a production
// environment sourced. A developer who wants to exercise this test against
// a real Discord (or generic) webhook sets
// CULTURE_NODES_WEBHOOK_TEST_URL=... explicitly and only for that run.
const culturNodesWebhookTestURLEnv = "CULTURE_NODES_WEBHOOK_TEST_URL"

// TestPostLiveNetworkOptional is the one test in this package allowed to
// make a real network call. It is skipped by every normal `go test ./...`
// and every CI run, because nothing sets CULTURE_NODES_WEBHOOK_TEST_URL
// there — it exists purely so a human can opt in locally to prove the
// transport round-trips against a real endpoint before wiring it into the
// notifier daemon (t14).
func TestPostLiveNetworkOptional(t *testing.T) {
	liveURL := strings.TrimSpace(os.Getenv(culturNodesWebhookTestURLEnv))
	if liveURL == "" {
		t.Skipf("%s not set; skipping the one live-network test in this package", culturNodesWebhookTestURLEnv)
	}

	payload := notify.Payload{
		RunID:         "run_hermetic_live_test",
		Workflow:      "internal/notify live test",
		Event:         "test.ping",
		Actor:         "go test",
		DashboardLink: "https://example.invalid/dashboard",
	}

	body, err := notify.BuildMessage(liveURL, payload)
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}

	result := notify.Post(context.Background(), liveURL, body)
	if result != notify.Posted {
		t.Fatalf("live webhook POST to %s did not report Posted, got %v", culturNodesWebhookTestURLEnv, result)
	}
}
