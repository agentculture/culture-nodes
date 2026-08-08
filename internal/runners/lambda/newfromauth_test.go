package lambda_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/awsauth"
	"github.com/agentculture/culture-nodes/internal/runners"
	runnerlambda "github.com/agentculture/culture-nodes/internal/runners/lambda"
)

// TestNewFromAuthLoadsAndDispatches proves NewFromAuth (task t17) builds an
// Adapter that behaves exactly like one New builds, once credentials
// resolve via internal/awsauth.LoadConfig's static-keys link: Load succeeds
// against the fake (proving GetFunction went through credentials NewFromAuth
// resolved), and a subsequent Execute dispatches normally.
func TestNewFromAuthLoadsAndDispatches(t *testing.T) {
	for _, key := range []string{"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_PROFILE"} {
		t.Setenv(key, "")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "fake-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake-secret-key")

	fake := newFakeLambda(t)
	fake.describe(testARN, healthyFunction())

	var logs []string
	adapter, err := runnerlambda.NewFromAuth(t.Context(), awsauth.Options{
		Region: "us-east-1",
		Logf: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	}, runnerlambda.Config{
		Registry:    registryWith(t, runners.FunctionIdentity{ARN: testARN, ImageDigest: testImage}),
		Endpoint:    fake.server.URL,
		HTTPClient:  fake.server.Client(),
		MaxAttempts: 1,
		Clock:       fixedClock(clockStart, 2*time.Second),
	})
	if err != nil {
		t.Fatalf("NewFromAuth: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("expected awsauth.LoadConfig to report its resolved Source through authOpts.Logf")
	}

	getCount, _ := fake.counts()
	if getCount != 1 {
		t.Fatalf("GetFunction calls = %d, want 1 (NewFromAuth's Load call)", getCount)
	}

	fake.setInvoke(invokeResponse{Payload: `{"exit_code":0}`})
	result, err := adapter.Execute(t.Context(), operation())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if code, ok := result.ExitCode(); !ok || code != 0 {
		t.Fatalf("Execute exit code = %v/%v, want 0", code, ok)
	}
}

// TestNewFromAuthRequiresARegistry proves NewFromAuth refuses an empty
// registry the same way New does -- the registry/revision checks happen for
// both constructors via the shared newAdapter helper.
func TestNewFromAuthRequiresARegistry(t *testing.T) {
	fake := newFakeLambda(t)
	_, err := runnerlambda.NewFromAuth(t.Context(), awsauth.Options{}, runnerlambda.Config{
		Registry: runners.NewFunctionRegistry(),
		Endpoint: fake.server.URL,
	})
	if err == nil {
		t.Fatal("NewFromAuth with an empty registry: got nil error, want one")
	}
}
