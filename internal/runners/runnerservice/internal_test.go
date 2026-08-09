package runnerservice

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/runners"
)

func TestBearerTokenParsing(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"BEARER  abc  ", "abc", true},
		{"abc", "", false},
		{"Basic abc", "", false},
		{"Bearer", "", false},
		{"Bearer   ", "", false},
		{"", "", false},
	} {
		got, ok := bearerToken(tc.header)
		if ok != tc.ok || got != tc.want {
			t.Errorf("bearerToken(%q) = (%q, %v), want (%q, %v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

// The comparison is over fixed-width digests rather than the raw strings, so
// neither the secret's length nor a shared prefix is observable from timing.
// What a test can check is the behaviour that discipline must preserve:
// exactly one string authenticates.
func TestOnlyTheExactSecretAuthenticates(t *testing.T) {
	const secret = "a-runner-service-bearer-secret"
	auth, err := newAuthenticator(secret)
	if err != nil {
		t.Fatalf("newAuthenticator: %v", err)
	}

	for _, tc := range []struct {
		name   string
		header string
		want   bool
	}{
		{"exact", "Bearer " + secret, true},
		{"empty", "", false},
		{"prefix", "Bearer " + secret[:5], false},
		{"suffix added", "Bearer " + secret + "!", false},
		{"case changed", "Bearer " + "A-RUNNER-SERVICE-BEARER-SECRET", false},
		{"wrong scheme", "Basic " + secret, false},
	} {
		req, err := http.NewRequest(http.MethodGet, "http://runner.invalid/v1/operations/x", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if tc.header != "" {
			req.Header.Set(runners.AuthorizationHeader, tc.header)
		}
		if got := auth.authenticate(req); got != tc.want {
			t.Errorf("%s: authenticate() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNewAuthenticatorRefusesAnEmptySecret(t *testing.T) {
	if _, err := newAuthenticator(""); err == nil {
		t.Fatal("newAuthenticator accepted an empty secret")
	}
}

// A record is swept only once its declared retention has elapsed since the
// operation finished — never before, because a status that disappears inside
// the retention window makes the outcome unlearnable.
func TestSweepRemovesTerminalRecordsOnlyAfterTheirRetention(t *testing.T) {
	store := NewMemoryStore()
	svc, err := New(Config{
		Runner:  runnerStub{},
		Store:   store,
		Secret:  "a-runner-service-bearer-secret",
		OnError: func(error) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer svc.Close()

	finished := time.Now().UTC()
	expires := finished.Add(runners.MinStatusRetention)
	fresh := Record{
		OperationID: "op-fresh",
		State:       runners.StateCompleted,
		FinishedAt:  &finished,
		ExpiresAt:   &expires,
	}
	stale := fresh
	stale.OperationID = "op-stale"
	if err := store.Put(fresh); err != nil {
		t.Fatalf("put fresh: %v", err)
	}
	if err := store.Put(stale); err != nil {
		t.Fatalf("put stale: %v", err)
	}

	removed, err := svc.sweep(finished.Add(runners.MinStatusRetention - time.Minute))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 0 {
		t.Fatalf("sweep removed %d records before their retention elapsed", removed)
	}

	removed, err = svc.sweep(expires.Add(time.Second))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 2 {
		t.Fatalf("sweep removed %d records after their retention elapsed, want 2", removed)
	}
	if _, ok, _ := store.Get("op-stale"); ok {
		t.Error("an expired record is still readable after the sweep")
	}
}

// runnerStub satisfies runners.Runner for tests that never dispatch.
type runnerStub struct{}

func (runnerStub) Execute(_ context.Context, _ runners.Operation) (runners.Result, error) {
	return runners.Result{}, nil
}
