package runnerconformance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/store"
)

// Defaults for a Config that leaves a field zero.
const (
	// DefaultTimeout bounds one HTTP request to the runner service. It is
	// deliberately short: the protocol is asynchronous, so every request the
	// kit makes is a dispatch, a status read, or a cancel — none of which may
	// take long, whatever the operation itself is doing.
	DefaultTimeout = 30 * time.Second
	// DefaultPollInterval is how often the kit samples status. It is faster
	// than runners.DefaultPollInterval because a test that waited five
	// seconds per sample would spend its whole budget sleeping; the kit
	// respects a runner's declared poll_after_seconds when that is slower
	// (see suite.pollInterval).
	DefaultPollInterval = 250 * time.Millisecond
	// DefaultTerminalWait bounds how long the kit waits for an operation to
	// finish. Real container executions take tens of seconds, so this is
	// generous by design.
	DefaultTerminalWait = 5 * time.Minute
	// DefaultRetentionReadDelay is how long the kit waits before re-reading a
	// terminal status. See the package doc for why this is a declaration
	// check and not a retention proof.
	DefaultRetentionReadDelay = time.Second
	// DefaultCancelAfter is how long the kit lets a cancellable operation run
	// before asking for it to stop, so the cancel lands against work that is
	// genuinely in flight rather than against an operation still queued.
	DefaultCancelAfter = 3 * time.Second
)

// Config describes the runner service under test and the operations to drive
// it with.
//
// Endpoint and Operation are required. Every check that needs something
// Config does not supply skips with an explanation rather than failing: a
// runner that does not implement cancellation is not a broken runner
// (the protocol says so out loud), and a suite that failed it for that would
// be measuring the wrong thing.
type Config struct {
	// Endpoint is the runner service's base URL. The kit appends the
	// protocol paths through runners.ServiceIdentity, which is the same
	// construction the runtime uses.
	Endpoint string
	// AuthToken is the bearer secret the runner requires. Unlike the actor
	// kit, an empty token is NOT a legitimate deployment: the runner protocol
	// makes caller authentication mandatory with no loopback exemption, so
	// the authentication check fails rather than skips when this is empty.
	AuthToken string

	// Operation is the operation template the kit dispatches. Its
	// operation_id is replaced with a fresh one per dispatch; everything else
	// crosses the wire as given.
	Operation runners.Operation
	// ExpectTerminalState is the state Operation should reach. Defaults to
	// runners.StateCompleted, because the caller chose an operation and knows
	// what it should do — a kit that accepted any terminal state would pass a
	// runner that refuses everything.
	ExpectTerminalState runners.State

	// RefusedOperation is an operation the runner must NOT execute — a
	// requires_shell command, an unregistered image digest, a policy field it
	// cannot enforce. Empty skips the refusal check.
	RefusedOperation *runners.Operation
	// CancellableOperation is a long-running operation the kit dispatches and
	// then cancels. Empty skips the cancellation check.
	CancellableOperation *runners.Operation

	// Timeout bounds one HTTP request; TerminalWait bounds waiting for an
	// operation to finish; PollInterval is how often status is sampled;
	// RetentionReadDelay is how long the kit waits before re-reading a
	// terminal status; CancelAfter is how long a cancellable operation runs
	// before the cancel is sent.
	Timeout            time.Duration
	TerminalWait       time.Duration
	PollInterval       time.Duration
	RetentionReadDelay time.Duration
	CancelAfter        time.Duration
}

func (c Config) timeout() time.Duration      { return orDefault(c.Timeout, DefaultTimeout) }
func (c Config) terminalWait() time.Duration { return orDefault(c.TerminalWait, DefaultTerminalWait) }
func (c Config) retentionDelay() time.Duration {
	return orDefault(c.RetentionReadDelay, DefaultRetentionReadDelay)
}
func (c Config) cancelAfter() time.Duration    { return orDefault(c.CancelAfter, DefaultCancelAfter) }
func (c Config) configuredPoll() time.Duration { return orDefault(c.PollInterval, DefaultPollInterval) }

func (c Config) expectedTerminal() runners.State {
	if c.ExpectTerminalState == "" {
		return runners.StateCompleted
	}
	return c.ExpectTerminalState
}

func orDefault(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

// suite holds one Run's shared machinery.
type suite struct {
	cfg       Config
	client    *http.Client
	identity  runners.ServiceIdentity
	validator *contracts.Validator
}

// Run executes the whole conformance suite against cfg.Endpoint.
//
// Each check is a subtest, so a failing runner gets a list of exactly which
// protocol properties it does not hold rather than one opaque failure.
func Run(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.Endpoint == "" {
		t.Fatal("runnerconformance: Config.Endpoint is required")
	}
	if cfg.Operation.OperationID == "" && cfg.Operation.Runner == "" {
		t.Fatal("runnerconformance: Config.Operation is required — the kit has no way to invent an operation a runner would accept")
	}

	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatalf("runnerconformance: build the schema validator: %v", err)
	}

	s := &suite{
		cfg: cfg,
		// One request per call, no client-side retry: the kit is measuring
		// the runner's answers, and a retry would hide a flaky one.
		client:    &http.Client{Timeout: cfg.timeout()},
		identity:  runners.ServiceIdentity{Endpoint: cfg.Endpoint},
		validator: validator,
	}
	s.requireValidOperations(t)

	t.Run("authentication-is-required", s.checkAuthRequired)
	t.Run("operation-lifecycle", s.checkLifecycle)
	t.Run("unknown-operation-is-404", s.checkUnknown404)
	t.Run("refused-operation-is-not-executed", s.checkRefusal)
	t.Run("cancellation", s.checkCancellation)
}

// requireValidOperations fails fast when the kit was handed an operation the
// schema itself rejects, so a typo in a fixture is reported as a bad Config
// rather than as a runner that refused an operation nobody should have sent.
func (s *suite) requireValidOperations(t *testing.T) {
	t.Helper()
	check := func(name string, op *runners.Operation) {
		if op == nil {
			return
		}
		candidate := s.newOperation(*op)
		if err := s.validator.Validate(runners.OperationSchemaPath, candidate); err != nil {
			t.Fatalf("runnerconformance: Config.%s is not schema-valid: %v", name, err)
		}
	}
	check("Operation", &s.cfg.Operation)
	check("RefusedOperation", s.cfg.RefusedOperation)
	check("CancellableOperation", s.cfg.CancellableOperation)
}

// newOperation clones the template with a fresh operation id, so each check
// dispatches work of its own and an idempotency check is not accidentally
// satisfied by a previous subtest's record.
func (s *suite) newOperation(base runners.Operation) runners.Operation {
	op := base
	op.OperationID = "op" + store.NewULID()
	return op
}

// ---------------------------------------------------------------- transport

// do performs one protocol request and returns the status code and body. It
// never retries and never follows a redirect quietly: both would hide a
// property the kit is measuring.
func (s *suite) do(t *testing.T, method, url, token string, body []byte, headers map[string]string) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.timeout())
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		t.Fatalf("runnerconformance: build %s %s: %v", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set(runners.AuthorizationHeader, "Bearer "+token)
	}
	req.Header.Set(runners.ProtocolVersionHeader, runners.ProtocolVersion)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("runnerconformance: %s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("runnerconformance: read %s %s body: %v", method, url, err)
	}
	s.requireNoSecretEcho(t, method, url, payload)
	return resp.StatusCode, payload
}

// requireNoSecretEcho holds the runner to the protocol's "never echo the
// secret in an error body" rule. It is checked on every response rather than
// in a check of its own, because the interesting place for a secret to leak
// is precisely the error path a dedicated check would not exercise.
func (s *suite) requireNoSecretEcho(t *testing.T, method, url string, body []byte) {
	t.Helper()
	if s.cfg.AuthToken == "" {
		return
	}
	if bytes.Contains(body, []byte(s.cfg.AuthToken)) {
		t.Errorf("%s %s echoed the bearer secret in its response body; the protocol forbids logging or echoing it", method, url)
	}
}

func (s *suite) execute(t *testing.T, op runners.Operation, token string) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("runnerconformance: marshal operation: %v", err)
	}
	return s.do(t, http.MethodPost, s.identity.ExecuteURL(), token, body,
		map[string]string{runners.IdempotencyKeyHeader: op.OperationID})
}

func (s *suite) readStatus(t *testing.T, operationID, token string) (int, []byte) {
	t.Helper()
	return s.do(t, http.MethodGet, s.identity.StatusURL(operationID), token, nil, nil)
}

func (s *suite) requestCancel(t *testing.T, operationID, token string) (int, []byte) {
	t.Helper()
	return s.do(t, http.MethodPost, s.identity.CancelURL(operationID), token, nil, nil)
}

// ------------------------------------------------------------------ decoding

// statusEnvelope is the wire form of a status read, kept separate from
// runners.OperationStatus so the kit can validate the embedded result
// document as raw JSON against the schema rather than against a Go struct
// that has already normalised it.
type statusEnvelope struct {
	OperationID string          `json:"operation_id"`
	State       runners.State   `json:"state"`
	Result      json.RawMessage `json:"result"`
}

// sample is one observed status read: the envelope, its typed form, and the
// exact bytes, so a later re-read can be compared byte-for-byte.
type sample struct {
	envelope statusEnvelope
	status   runners.OperationStatus
	raw      []byte
}

func (s *suite) decodeAcceptance(t *testing.T, body []byte) runners.Acceptance {
	t.Helper()
	var acceptance runners.Acceptance
	if err := json.Unmarshal(body, &acceptance); err != nil {
		t.Fatalf("runnerconformance: the 202 body is not an acceptance document: %v\nbody: %s", err, body)
	}
	return acceptance
}

func (s *suite) decodeStatus(t *testing.T, body []byte) sample {
	t.Helper()
	var envelope statusEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("runnerconformance: the 200 body is not a status document: %v\nbody: %s", err, body)
	}
	status := runners.OperationStatus{OperationID: envelope.OperationID, State: envelope.State}
	if len(envelope.Result) > 0 && !bytes.Equal(bytes.TrimSpace(envelope.Result), []byte("null")) {
		var result runners.Result
		if err := json.Unmarshal(envelope.Result, &result); err != nil {
			t.Fatalf("runnerconformance: the status carries a result that is not a result document: %v\nresult: %s",
				err, envelope.Result)
		}
		status.Result = &result
	}
	return sample{envelope: envelope, status: status, raw: body}
}

// ------------------------------------------------------------------- checks

// Caller authentication is mandatory on every request — execute and status
// alike — and there is no loopback exemption. This is the check the protocol
// document names explicitly as run against the reference deployment.
func (s *suite) checkAuthRequired(t *testing.T) {
	if s.cfg.AuthToken == "" {
		t.Fatal("runnerconformance: Config.AuthToken is empty. " +
			"Unlike the actor protocol, the runner protocol has no unauthenticated deployment posture: " +
			"a runner service accepting operations over the network is a remote-code-execution surface")
	}

	credentials := []struct {
		name  string
		token string
	}{
		{"no credential", ""},
		{"wrong credential", "definitely-not-the-secret"},
	}

	for _, credential := range credentials {
		t.Run("execute/"+credential.name, func(t *testing.T) {
			code, body := s.execute(t, s.newOperation(s.cfg.Operation), credential.token)
			requireRefusedStatus(t, "execute", credential.name, code, body)
		})
		t.Run("status/"+credential.name, func(t *testing.T) {
			// Deliberately an operation id that was never dispatched: auth is
			// decided before existence, so this must be 401/403 and not 404.
			// A runner that answers 404 here is an operation-id oracle for
			// anyone who can reach it.
			code, body := s.readStatus(t, "op"+store.NewULID(), credential.token)
			requireRefusedStatus(t, "status", credential.name, code, body)
		})
	}
}

func requireRefusedStatus(t *testing.T, path, credential string, code int, body []byte) {
	t.Helper()
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return
	}
	t.Fatalf("%s with %s answered %d, want 401 or 403; the protocol makes caller authentication mandatory "+
		"on execute and status alike, with no loopback exemption\nbody: %s", path, credential, code, body)
}

// The whole authenticated lifecycle in one check, against one dispatched
// operation: acceptance, an immediately readable status, polling to a
// terminal state with result-iff-terminal held on every sample, a
// schema-valid result that agrees with its envelope, a status still readable
// afterwards, and a re-dispatch that returns the same acceptance without
// re-running the work.
//
// It is one subtest rather than six because each of those properties is about
// the SAME operation, and six subtests would mean six real executions on a
// runner that actually runs containers.
func (s *suite) checkLifecycle(t *testing.T) {
	op := s.newOperation(s.cfg.Operation)

	acceptance := s.dispatch(t, op)
	s.requireImmediateStatus(t, op.OperationID)
	terminal := s.pollToTerminal(t, op.OperationID, acceptance)
	s.requireTerminalResult(t, op.OperationID, terminal)
	s.requireStatusStillReadable(t, op.OperationID, terminal)
	s.requireIdempotentRedispatch(t, op, acceptance, terminal)
}

// A dispatch answers 202 with an acceptance that echoes the operation id and
// promises at least the protocol's minimum retention.
func (s *suite) dispatch(t *testing.T, op runners.Operation) runners.Acceptance {
	t.Helper()
	code, body := s.execute(t, op, s.cfg.AuthToken)
	if code != http.StatusAccepted {
		t.Fatalf("dispatch answered %d, want 202; the protocol has no synchronous variant — "+
			"a 200 carrying a result is a violation, not a faster path\nbody: %s", code, body)
	}
	acceptance := s.decodeAcceptance(t, body)
	// Acceptance.Validate is the runtime's own admission check: the id must
	// echo, and a declared retention shorter than the protocol minimum is
	// refused at dispatch rather than discovered at the first missed
	// completion.
	if err := acceptance.Validate(op.OperationID); err != nil {
		t.Fatalf("the acceptance is not one the runtime would accept: %v", err)
	}
	return acceptance
}

// Status is answerable immediately after acceptance: polling starts before
// the work does.
func (s *suite) requireImmediateStatus(t *testing.T, operationID string) {
	t.Helper()
	code, body := s.readStatus(t, operationID, s.cfg.AuthToken)
	if code != http.StatusOK {
		t.Fatalf("status read immediately after acceptance answered %d, want 200; "+
			"a 404 on an operation the runner just accepted is a dispatch error, never a completion\nbody: %s", code, body)
	}
	observed := s.decodeStatus(t, body)
	if err := observed.status.Validate(); err != nil {
		t.Fatalf("the first status envelope is not valid: %v\nbody: %s", err, body)
	}
	if observed.status.OperationID != operationID {
		t.Fatalf("status reports operation %q, want %q", observed.status.OperationID, operationID)
	}
}

// pollInterval honours a runner's declared poll_after_seconds when it is
// slower than the kit's own cadence: sampling faster than a runner asked for
// is load it said it did not want.
func (s *suite) pollInterval(acceptance runners.Acceptance) time.Duration {
	configured := s.cfg.configuredPoll()
	if acceptance.PollAfterSeconds > 0 {
		if declared := acceptance.PollInterval(); declared > configured {
			return declared
		}
	}
	return configured
}

// pollToTerminal samples status until the operation finishes, holding the
// result-iff-terminal invariant on every single sample along the way.
func (s *suite) pollToTerminal(t *testing.T, operationID string, acceptance runners.Acceptance) sample {
	t.Helper()
	interval := s.pollInterval(acceptance)
	deadline := time.Now().Add(s.cfg.terminalWait())
	var last sample
	for {
		code, body := s.readStatus(t, operationID, s.cfg.AuthToken)
		if code != http.StatusOK {
			t.Fatalf("status read answered %d while polling; a runner may not forget an operation it accepted\nbody: %s",
				code, body)
		}
		last = s.decodeStatus(t, body)
		// OperationStatus.Validate is exactly the result-iff-terminal rule:
		// a non-terminal status carrying a result and a terminal one carrying
		// none are both contract failures, and so is an envelope whose state
		// disagrees with the result it carries.
		if err := last.status.Validate(); err != nil {
			t.Fatalf("status envelope is not valid: %v\nbody: %s", err, body)
		}
		if last.status.Terminal() {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s was still %s after %s; the kit gave up waiting",
				operationID, last.status.State, s.cfg.terminalWait())
		}
		time.Sleep(interval)
	}
}

// The terminal status carries a schema-valid result whose state matches the
// envelope, and the state is the one the caller said to expect.
func (s *suite) requireTerminalResult(t *testing.T, operationID string, terminal sample) {
	t.Helper()
	if terminal.status.State != s.cfg.expectedTerminal() {
		t.Errorf("operation reached %s, want %s", terminal.status.State, s.cfg.expectedTerminal())
	}
	if len(terminal.envelope.Result) == 0 {
		t.Fatal("the terminal status carries no result document; a completion without evidence is a claim, not a result")
	}
	if err := s.validator.ValidateJSON(runners.ResultSchemaPath, terminal.envelope.Result); err != nil {
		t.Errorf("the terminal result is not schema-valid: %v\nresult: %s", err, terminal.envelope.Result)
	}
	if terminal.status.Result.OperationID != operationID {
		t.Errorf("the result names operation %q, want %q", terminal.status.Result.OperationID, operationID)
	}
	// The four named observations are the honesty block the whole document
	// exists for. Schema validation above already requires them; this reads
	// the raw object so the failure names the missing key rather than
	// reporting a generic schema violation.
	var raw struct {
		Observations map[string]json.RawMessage `json:"observations"`
	}
	if err := json.Unmarshal(terminal.envelope.Result, &raw); err == nil {
		for _, name := range []string{"exit_status", "changed_paths", "logs", "resource_usage"} {
			if _, ok := raw.Observations[name]; !ok {
				t.Errorf("the result declares no %q observation; every result states, per fact, whether it measured it", name)
			}
		}
	}
}

// The terminal status stays readable. See the package doc: this is a
// declaration check plus a short-delay re-read, not a proof of the full
// declared retention.
func (s *suite) requireStatusStillReadable(t *testing.T, operationID string, terminal sample) {
	t.Helper()
	time.Sleep(s.cfg.retentionDelay())
	code, body := s.readStatus(t, operationID, s.cfg.AuthToken)
	if code != http.StatusOK {
		t.Fatalf("re-reading the terminal status %s after completion answered %d; "+
			"a runner that forgets an operation before its declared retention makes the outcome unlearnable\nbody: %s",
			s.cfg.retentionDelay(), code, body)
	}
	if !bytes.Equal(bytes.TrimSpace(body), bytes.TrimSpace(terminal.raw)) {
		t.Errorf("the terminal status changed between reads; a terminal state is final\nfirst:  %s\nsecond: %s",
			terminal.raw, body)
	}
}

// Re-dispatching the same operation_id returns the acceptance already issued
// and does not start the work a second time. The byte-identical terminal
// result is the proof of the second half: a re-run would produce different
// timing at the very least.
func (s *suite) requireIdempotentRedispatch(t *testing.T, op runners.Operation, first runners.Acceptance, terminal sample) {
	t.Helper()
	code, body := s.execute(t, op, s.cfg.AuthToken)
	if code != http.StatusAccepted {
		t.Fatalf("re-dispatching the same operation_id answered %d, want 202 with the acceptance already issued\nbody: %s",
			code, body)
	}
	second := s.decodeAcceptance(t, body)
	if second.OperationID != first.OperationID {
		t.Errorf("re-dispatch returned operation id %q, want the already-accepted %q",
			second.OperationID, first.OperationID)
	}

	_, statusBody := s.readStatus(t, op.OperationID, s.cfg.AuthToken)
	after := s.decodeStatus(t, statusBody)
	if !after.status.Terminal() {
		t.Fatalf("re-dispatching a finished operation moved it back to %s; at-least-once delivery must not become "+
			"at-least-twice execution", after.status.State)
	}
	if !bytes.Equal(bytes.TrimSpace(statusBody), bytes.TrimSpace(terminal.raw)) {
		t.Errorf("re-dispatching a finished operation changed its result; the work ran twice\nbefore: %s\nafter:  %s",
			terminal.raw, statusBody)
	}
}

// An authenticated read of an operation that was never dispatched is 404 —
// which the runtime reads as a dispatch error, never as a completion.
func (s *suite) checkUnknown404(t *testing.T) {
	unknown := "op" + store.NewULID()
	code, body := s.readStatus(t, unknown, s.cfg.AuthToken)
	if code != http.StatusNotFound {
		t.Fatalf("status of an operation that was never dispatched answered %d, want 404\nbody: %s", code, body)
	}
}

// A refusable operation is refused, not executed.
//
// The protocol's error table describes the synchronous refusal (400/422 at
// dispatch). A runner that only discovers the refusal after it has answered
// 202 has no HTTP error channel left, and the honest answer there is a
// terminal `rejected` status whose result declares that nothing was measured.
// The kit accepts either, and requires only that the operation never runs.
func (s *suite) checkRefusal(t *testing.T) {
	if s.cfg.RefusedOperation == nil {
		t.Skip("no RefusedOperation configured: set one (a requires_shell command, an unregistered image digest, " +
			"a policy the runner cannot enforce) to check the policy boundary")
	}
	op := s.newOperation(*s.cfg.RefusedOperation)

	code, body := s.execute(t, op, s.cfg.AuthToken)
	switch {
	case code == http.StatusBadRequest || code == http.StatusUnprocessableEntity ||
		code == http.StatusForbidden || code == http.StatusConflict:
		return // refused synchronously, which is the shape the error table describes
	case code != http.StatusAccepted:
		t.Fatalf("dispatching a refusable operation answered %d; expected a 4xx refusal or a 202 followed by a "+
			"terminal rejected status\nbody: %s", code, body)
	}

	acceptance := s.decodeAcceptance(t, body)
	terminal := s.pollToTerminal(t, op.OperationID, acceptance)
	if terminal.status.State != runners.StateRejected {
		t.Fatalf("a refusable operation reached %s; a policy the runner cannot enforce is refused, never run "+
			"under a different limit silently", terminal.status.State)
	}
	if terminal.status.Result.Error == nil {
		t.Fatal("the rejected result carries no error block; a refusal must name what it refused")
	}
	switch kind := terminal.status.Result.Error.Kind; kind {
	case runners.ErrorRejectedInput, runners.ErrorAuthOrPolicy, runners.ErrorContractFailure:
	default:
		t.Errorf("the rejected result declares error kind %q; a refusal is rejected_input, auth_or_policy, "+
			"or contract_failure", kind)
	}
	if obs, ok := terminal.status.Result.Observations.Get("exit_status"); ok && obs.Measured {
		t.Error("a rejected operation reports a measured exit status; nothing ran, so nothing was measured")
	}
}

// Cancellation is optional and best-effort. When the runner declares it, the
// cancel path must answer and the operation must reach a terminal state; when
// it does not, a 404 or 405 is conformant and changes nothing about a run.
func (s *suite) checkCancellation(t *testing.T) {
	if s.cfg.CancellableOperation == nil {
		t.Skip("no CancellableOperation configured: set a long-running operation to check the cancel path")
	}
	op := s.newOperation(*s.cfg.CancellableOperation)
	acceptance := s.dispatch(t, op)

	if !acceptance.SupportsCancellation {
		code, _ := s.requestCancel(t, op.OperationID, s.cfg.AuthToken)
		if code != http.StatusNotFound && code != http.StatusMethodNotAllowed &&
			code != http.StatusAccepted && code != http.StatusNoContent {
			t.Errorf("the runner declared supports_cancellation=false and answered cancel with %d; "+
				"404 or 405 is the conformant answer for an unimplemented path", code)
		}
		t.Skip("the runner declares supports_cancellation=false, which is fully conformant")
	}

	time.Sleep(s.cfg.cancelAfter())
	code, body := s.requestCancel(t, op.OperationID, s.cfg.AuthToken)
	if code != http.StatusAccepted && code != http.StatusNoContent {
		t.Fatalf("cancel answered %d, want 202 or 204 from a runner that declares supports_cancellation\nbody: %s",
			code, body)
	}

	terminal := s.pollToTerminal(t, op.OperationID, acceptance)
	if terminal.status.State != runners.StateCancelled {
		t.Errorf("after an accepted cancel the operation reached %s, want %s",
			terminal.status.State, runners.StateCancelled)
	}
}
