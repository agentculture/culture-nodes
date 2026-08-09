package runners_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// The protocol client (task t9) is the caller half of api/runner-protocol.
// Everything asserted here is a rule that document states in prose, and the
// point of testing it at this level is that a violation must be a *dispatch
// error* rather than a fabricated result — the runtime never records an
// execution it did not learn about from an authenticated status read.

const pcDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

// pcSecretRef and pcSecret are the registry's credential *reference* and
// the material a resolver turns it into. They are deliberately different
// strings: a test that used one value for both could not catch a client that
// sent the reference name as the bearer token.
const (
	pcSecretRef = "runner/test/execute-token"
	pcSecret    = "s3cr3t-execute-token"
)

// fakeSecrets is the resolver the client is constructed with. A real
// deployment resolves a secret_ref against its own secret source; nothing in
// this package ever holds secret material of its own.
type fakeSecrets struct {
	secrets map[string]string
	err     error
	calls   int
}

func (f *fakeSecrets) ResolveSecret(_ context.Context, ref string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	secret, ok := f.secrets[ref]
	if !ok {
		return "", errors.New("no such secret reference")
	}
	return secret, nil
}

func pcOperation(operationID string) runners.Operation {
	requiresShell := false
	return runners.Operation{
		OperationID: operationID,
		Runner:      "test-runner",
		Execution: runners.Execution{
			Kind:        runners.ExecutionContainer,
			ImageRef:    "python:3.12-slim@" + pcDigest,
			ImageDigest: pcDigest,
		},
		Command: runners.Command{Argv: []string{"true"}, RequiresShell: &requiresShell},
		Policy:  runners.Policy{TimeoutSeconds: 60, Network: runners.NetworkNone},
	}
}

func pcIdentity(endpoint string) runners.ServiceIdentity {
	return runners.ServiceIdentity{
		Endpoint:               endpoint,
		ImageDigest:            pcDigest,
		SecretRef:              pcSecretRef,
		AllowInsecureTransport: true,
	}
}

func newTestClient(t *testing.T, resolver runners.SecretResolver) *runners.ProtocolClient {
	t.Helper()
	client, err := runners.NewProtocolClient(resolver)
	if err != nil {
		t.Fatalf("NewProtocolClient: %v", err)
	}
	return client
}

func defaultSecrets() *fakeSecrets {
	return &fakeSecrets{secrets: map[string]string{pcSecretRef: pcSecret}}
}

// A conformant dispatch: 202 with an acceptance echoing the operation id, and
// every protocol header present on the wire.
func TestProtocolClientDispatchSendsTheContractHeadersAndReadsTheAcceptance(t *testing.T) {
	var (
		gotPath, gotAuth, gotIdem, gotVersion string
		gotCallbackURL, gotCallbackToken      string
		gotBody                               runners.Operation
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get(runners.AuthorizationHeader)
		gotIdem = r.Header.Get(runners.IdempotencyKeyHeader)
		gotVersion = r.Header.Get(runners.ProtocolVersionHeader)
		gotCallbackURL = r.Header.Get(runners.CallbackURLHeader)
		gotCallbackToken = r.Header.Get(runners.CallbackTokenHeader)
		_ = json.NewDecoder(r.Body).Decode(&gotBody)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"operation_id":"op_1","poll_after_seconds":7,` +
			`"status_retention_seconds":86400,"supports_cancellation":true,"supports_callback":true}`))
	}))
	defer server.Close()

	secrets := defaultSecrets()
	client := newTestClient(t, secrets)

	accepted, err := client.Dispatch(context.Background(), pcIdentity(server.URL), pcOperation("op_1"),
		runners.CallbackOffer{URL: "https://nodes.example/v1/runner-operations/op_1/events", Token: "cbtoken"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if gotPath != runners.OperationsPath {
		t.Errorf("POST path = %q, want %q", gotPath, runners.OperationsPath)
	}
	if gotAuth != "Bearer "+pcSecret {
		t.Errorf("Authorization = %q, want the bearer resolved from the registry's secret_ref", gotAuth)
	}
	if secrets.calls != 1 {
		t.Errorf("secret resolver called %d times, want 1 (resolved at dispatch time)", secrets.calls)
	}
	if gotIdem != "op_1" {
		t.Errorf("%s = %q, want the operation id", runners.IdempotencyKeyHeader, gotIdem)
	}
	if gotVersion != runners.ProtocolVersion {
		t.Errorf("%s = %q, want %q", runners.ProtocolVersionHeader, gotVersion, runners.ProtocolVersion)
	}
	if gotCallbackURL == "" || gotCallbackToken != "cbtoken" {
		t.Errorf("callback headers = (%q, %q), want both set from the offer", gotCallbackURL, gotCallbackToken)
	}
	if gotBody.OperationID != "op_1" || len(gotBody.Command.Argv) != 1 {
		t.Errorf("request body = %+v, want the operation document verbatim", gotBody)
	}
	if accepted.PollAfterSeconds != 7 || !accepted.SupportsCallback {
		t.Errorf("acceptance = %+v, want the runner's declarations read back", accepted)
	}
}

// The callback is strictly optional in both directions: an offer-less dispatch
// must send neither header, so a runner cannot tell a callback-capable
// deployment from one that never offers one.
func TestProtocolClientDispatchOmitsCallbackHeadersWhenNoneIsOffered(t *testing.T) {
	var present bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, urlOK := r.Header[http.CanonicalHeaderKey(runners.CallbackURLHeader)]
		_, tokenOK := r.Header[http.CanonicalHeaderKey(runners.CallbackTokenHeader)]
		present = urlOK || tokenOK
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"operation_id":"op_2"}`))
	}))
	defer server.Close()

	if _, err := newTestClient(t, defaultSecrets()).
		Dispatch(context.Background(), pcIdentity(server.URL), pcOperation("op_2"), runners.CallbackOffer{}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if present {
		t.Error("a dispatch with no callback offer sent a callback header")
	}
}

// "There is no synchronous variant. A 200 carrying a result is not a faster
// path, it is a protocol violation." The client must refuse it outright rather
// than quietly accept the result — accepting it would make the async-only
// invariant unenforced, and a lease-holding fast path would grow back.
func TestProtocolClientDispatchRefusesASynchronous200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"operation_id":"op_3","state":"completed","result":{}}`))
	}))
	defer server.Close()

	_, err := newTestClient(t, defaultSecrets()).
		Dispatch(context.Background(), pcIdentity(server.URL), pcOperation("op_3"), runners.CallbackOffer{})
	assertDispatchKind(t, err, runners.ErrorContractFailure)
}

// A 202 that acknowledges a different operation is a contract failure: polling
// it would report on work this attempt never dispatched.
func TestProtocolClientDispatchRefusesAnAcceptanceForAnotherOperation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"operation_id":"op_somebody_else"}`))
	}))
	defer server.Close()

	_, err := newTestClient(t, defaultSecrets()).
		Dispatch(context.Background(), pcIdentity(server.URL), pcOperation("op_4"), runners.CallbackOffer{})
	assertDispatchKind(t, err, runners.ErrorContractFailure)
}

// The HTTP → DispatchError taxonomy the protocol document tabulates. Each row
// is a *dispatch* error: no Result exists, because none was measured.
func TestProtocolClientClassifiesHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		want      runners.ErrorKind
		retryable bool
	}{
		{"rejected input", http.StatusBadRequest, runners.ErrorRejectedInput, false},
		{"unprocessable", http.StatusUnprocessableEntity, runners.ErrorRejectedInput, false},
		{"unauthenticated", http.StatusUnauthorized, runners.ErrorAuthOrPolicy, false},
		{"forbidden", http.StatusForbidden, runners.ErrorAuthOrPolicy, false},
		{"conflicting document", http.StatusConflict, runners.ErrorRejectedInput, false},
		{"oversize payload", http.StatusRequestEntityTooLarge, runners.ErrorRejectedInput, false},
		{"throttled", http.StatusTooManyRequests, runners.ErrorRateLimited, true},
		{"runner down", http.StatusBadGateway, runners.ErrorRunnerUnavailable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			_, err := newTestClient(t, defaultSecrets()).
				Dispatch(context.Background(), pcIdentity(server.URL), pcOperation("op_5"), runners.CallbackOffer{})
			de := assertDispatchKind(t, err, tc.want)
			if de.Retryable() != tc.retryable {
				t.Errorf("Retryable() = %v, want %v", de.Retryable(), tc.retryable)
			}
		})
	}
}

// A transport failure is a dispatch error too, never a fabricated failed
// result: nothing was measured, so there is nothing honest to record.
func TestProtocolClientClassifiesTransportFailureAsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close() // nothing is listening now

	_, err := newTestClient(t, defaultSecrets()).
		Dispatch(context.Background(), pcIdentity(url), pcOperation("op_6"), runners.CallbackOffer{})
	de := assertDispatchKind(t, err, runners.ErrorRetryableTransport)
	if !de.Retryable() {
		t.Error("a transport failure must be retryable")
	}
	if !errors.Is(err, runners.ErrTransport) {
		t.Error("a transport failure must match runners.ErrTransport")
	}
}

// A secret the deployment cannot resolve is an auth/policy refusal and nothing
// leaves the process: presenting no credential to a remote-code-execution
// surface is not a fallback.
func TestProtocolClientRefusesWhenTheSecretCannotBeResolved(t *testing.T) {
	var reached bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	resolver := &fakeSecrets{err: errors.New("secret source unavailable")}
	_, err := newTestClient(t, resolver).
		Dispatch(context.Background(), pcIdentity(server.URL), pcOperation("op_7"), runners.CallbackOffer{})
	assertDispatchKind(t, err, runners.ErrorAuthOrPolicy)
	if reached {
		t.Error("the client sent a request it had no credential for")
	}
}

// The secret material must never appear in an error string: an error body is
// logged, and a logged bearer token is a leaked bearer token.
func TestProtocolClientNeverEchoesTheSecretInAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))
	defer server.Close()

	_, err := newTestClient(t, defaultSecrets()).
		Dispatch(context.Background(), pcIdentity(server.URL), pcOperation("op_8"), runners.CallbackOffer{})
	if err == nil {
		t.Fatal("want a dispatch error")
	}
	if strings.Contains(err.Error(), pcSecret) {
		t.Fatalf("the error echoes the bearer secret: %v", err)
	}
}

// Pinning that is not checked is not pinning: an operation whose declared
// execution digest disagrees with the registered one is refused before it is
// sent anywhere.
func TestProtocolClientRefusesADigestThatDoesNotMatchTheRegisteredPin(t *testing.T) {
	var reached bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	op := pcOperation("op_9")
	op.Execution.ImageDigest = "sha256:" + strings.Repeat("b", 64)

	_, err := newTestClient(t, defaultSecrets()).Dispatch(context.Background(), pcIdentity(server.URL), op, runners.CallbackOffer{})
	assertDispatchKind(t, err, runners.ErrorRejectedInput)
	if !errors.Is(err, runners.ErrDigestMismatch) {
		t.Errorf("error = %v, want ErrDigestMismatch", err)
	}
	if reached {
		t.Error("an unpinned-mismatch operation was sent to the runner")
	}
}

// A declared retention shorter than the protocol minimum is refused AT
// DISPATCH: a runner that forgets an operation before it can be sampled has
// made the outcome unlearnable, and finding that out at the first missed
// completion is finding out too late.
func TestProtocolClientRefusesAnAcceptanceWithTooShortARetention(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"operation_id":"op_10","status_retention_seconds":30}`))
	}))
	defer server.Close()

	_, err := newTestClient(t, defaultSecrets()).
		Dispatch(context.Background(), pcIdentity(server.URL), pcOperation("op_10"), runners.CallbackOffer{})
	assertDispatchKind(t, err, runners.ErrorContractFailure)
}

// Status: the authoritative completion path. Non-terminal states carry no
// result and are not read as failures.
func TestProtocolClientStatusReadsRunningAndTerminalStates(t *testing.T) {
	var gotAuth, gotPath string
	terminal := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get(runners.AuthorizationHeader)
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if !terminal {
			_, _ = w.Write([]byte(`{"operation_id":"op_11","state":"running"}`))
			return
		}
		_, _ = w.Write([]byte(`{"operation_id":"op_11","state":"completed","result":` + completedResultJSON("op_11", 0) + `}`))
	}))
	defer server.Close()

	client := newTestClient(t, defaultSecrets())
	identity := pcIdentity(server.URL)

	running, err := client.Status(context.Background(), identity, "op_11")
	if err != nil {
		t.Fatalf("Status (running): %v", err)
	}
	if running.Terminal() || running.Result != nil {
		t.Fatalf("running status = %+v, want non-terminal with no result", running)
	}
	if gotAuth != "Bearer "+pcSecret {
		t.Errorf("status Authorization = %q, want the bearer: a status read leaks what ran, where, and with what digests", gotAuth)
	}
	if gotPath != runners.OperationsPath+"/op_11" {
		t.Errorf("status path = %q, want %s/op_11", gotPath, runners.OperationsPath)
	}

	terminal = true
	done, err := client.Status(context.Background(), identity, "op_11")
	if err != nil {
		t.Fatalf("Status (terminal): %v", err)
	}
	if !done.Terminal() || done.Result == nil {
		t.Fatalf("terminal status = %+v, want a result document", done)
	}
	if code, ok := done.Result.ExitCode(); !ok || code != 0 {
		t.Errorf("result exit code = (%d, %v), want (0, true)", code, ok)
	}
}

// "A 404 on the status of an operation the runtime dispatched is not a
// completion and is never read as one." It is a retryable dispatch error, so
// the sampler keeps sampling until the attempt's deadline decides.
func TestProtocolClientStatus404IsRetryableAndNeverACompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	_, err := newTestClient(t, defaultSecrets()).Status(context.Background(), pcIdentity(server.URL), "op_12")
	de := assertDispatchKind(t, err, runners.ErrorRunnerUnavailable)
	if !de.Retryable() {
		t.Error("a forgotten operation must be resampled, not concluded")
	}
}

// "An envelope that disagrees with its own evidence is a contract failure."
func TestProtocolClientStatusRefusesAnEnvelopeThatDisagreesWithItsResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"operation_id":"op_13","state":"failed","result":` + completedResultJSON("op_13", 0) + `}`))
	}))
	defer server.Close()

	_, err := newTestClient(t, defaultSecrets()).Status(context.Background(), pcIdentity(server.URL), "op_13")
	assertDispatchKind(t, err, runners.ErrorContractFailure)
}

// A terminal status with no result document is a completion claim, not a
// result — and the runtime records nothing it was not given.
func TestProtocolClientStatusRefusesATerminalStateWithNoResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"operation_id":"op_14","state":"completed"}`))
	}))
	defer server.Close()

	_, err := newTestClient(t, defaultSecrets()).Status(context.Background(), pcIdentity(server.URL), "op_14")
	assertDispatchKind(t, err, runners.ErrorContractFailure)
}

// assertDispatchKind asserts err is a *DispatchError of the given kind and
// returns it, so a caller can make further assertions on the same value.
func assertDispatchKind(t *testing.T, err error, want runners.ErrorKind) *runners.DispatchError {
	t.Helper()
	if err == nil {
		t.Fatalf("want a *DispatchError of kind %s, got nil", want)
	}
	var de *runners.DispatchError
	if !errors.As(err, &de) {
		t.Fatalf("error %v is not a *DispatchError", err)
	}
	if de.Kind != want {
		t.Fatalf("dispatch error kind = %s, want %s (%v)", de.Kind, want, err)
	}
	return de
}

// completedResultJSON is a minimal schema-shaped runner result document.
func completedResultJSON(operationID string, exitCode int) string {
	res := runners.Result{
		OperationID: operationID,
		State:       runners.StateCompleted,
		Exit:        &runners.Exit{Code: &exitCode},
	}
	encoded, err := json.Marshal(res)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
