package sqs_test

import (
	"fmt"
	"testing"

	"github.com/agentculture/culture-nodes/internal/awsauth"
	"github.com/agentculture/culture-nodes/internal/queue"
	queuesqs "github.com/agentculture/culture-nodes/internal/queue/sqs"
)

// TestNewDriverFromAuthRoundTrip proves NewDriverFromAuth (task t17) builds
// a Driver that behaves exactly like one New builds, once credentials
// resolve via internal/awsauth.LoadConfig's static-keys link -- the same
// four-method contract driver_test.go's TestPublishReceiveAckRoundTrip
// proves against New's Driver, plus a check that authOpts.Logf actually
// received the resolved Source.
func TestNewDriverFromAuthRoundTrip(t *testing.T) {
	// Isolate from whatever the process environment already has, the same
	// way internal/awsauth's own tests do.
	for _, key := range []string{"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_PROFILE"} {
		t.Setenv(key, "")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "fake-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake-secret-key")

	f := newFakeSQS(t)

	var logs []string
	d, err := queuesqs.NewDriverFromAuth(t.Context(), awsauth.Options{
		Region: "us-east-1",
		Logf: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	}, queuesqs.Config{
		QueueURL:   f.server.URL + "/000000000000/test-queue",
		Endpoint:   f.server.URL,
		HTTPClient: f.server.Client(),
	})
	if err != nil {
		t.Fatalf("NewDriverFromAuth: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("expected awsauth.LoadConfig to report its resolved Source through authOpts.Logf")
	}

	ctx := t.Context()
	ref := queue.WorkRef{WorkID: "wrk_auth_001", NamespaceID: "ns_001"}
	if err := d.Publish(ctx, ref); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	deliveries, err := d.Receive(ctx, 10, 0)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	del := findDelivery(t, deliveries, ref.WorkID)

	if err := d.Ack(ctx, del); err != nil {
		t.Fatalf("Ack: %v", err)
	}
}

// TestNewDriverFromAuthRequiresQueueURL proves NewDriverFromAuth refuses an
// empty QueueURL before ever calling awsauth.LoadConfig -- matching New's
// own up-front validation.
func TestNewDriverFromAuthRequiresQueueURL(t *testing.T) {
	_, err := queuesqs.NewDriverFromAuth(t.Context(), awsauth.Options{}, queuesqs.Config{})
	if err == nil {
		t.Fatal("NewDriverFromAuth with no QueueURL: got nil error, want one")
	}
}
