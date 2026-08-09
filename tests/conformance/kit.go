package conformance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/store"
)

// Defaults for a Config that leaves a field zero.
const (
	// DefaultTimeout bounds one invocation.
	DefaultTimeout = 30 * time.Second
	// DefaultCallbackWait bounds how long the kit waits for an asynchronous
	// actor's terminal callback.
	DefaultCallbackWait = 60 * time.Second
	// kitSecret signs the attempt-scoped callback tokens the kit mints. It is
	// a fixed test secret because the kit's tokens authorize reporting to the
	// kit's own in-process receiver and nothing else.
	kitSecret = "culture-nodes-conformance-kit-secret!"
)

// Config describes the actor under test and the inputs to drive it with.
//
// Only Endpoint is required. Every check that needs something Config does not
// supply skips with an explanation rather than failing: an adapter that does
// not implement asynchronous invocation is not a broken adapter, and a suite
// that failed it for that would be measuring the wrong thing.
type Config struct {
	// Endpoint is the actor's base URL. The kit appends §13.1's
	// /v1/invocations.
	Endpoint string
	// AuthToken is the scoped workload token the actor requires. When empty,
	// the authentication check skips — an unauthenticated endpoint is a
	// legitimate local or in-cluster deployment.
	AuthToken string

	// WorkflowName, WorkflowDigest, NodeID, and ContractDigest fill §13.1's
	// workflow and node blocks. They have harmless defaults; supply the real
	// ones if the actor validates them.
	WorkflowName   string
	WorkflowDigest string
	NodeID         string
	ContractDigest string

	// Input is an input the actor should accept and answer synchronously.
	Input json.RawMessage
	// AsyncInput is an input the actor should accept and answer with a §13.3
	// acceptance. Empty skips every asynchronous check.
	AsyncInput json.RawMessage
	// BadInput is an input the actor must REJECT. Empty skips the
	// contract-failure classification check.
	BadInput json.RawMessage

	// CallbackBaseURL overrides the base URL advertised to the actor. Set it
	// when the actor cannot reach the kit's loopback address.
	CallbackBaseURL string

	// Timeout bounds one invocation; CallbackWait bounds waiting for a
	// terminal callback.
	Timeout      time.Duration
	CallbackWait time.Duration

	// ExpectCallbackRetry requires that an actor whose terminal callback is
	// refused (503) redelivers it with the same event id. §13.4 says repeated
	// callbacks are idempotent, which presupposes redelivery; this makes the
	// requirement explicit rather than assumed, and it is opt-in because the
	// PRD does not mandate a retry schedule.
	ExpectCallbackRetry bool

	// RequireCancellation fails the cancellation check when the actor does
	// not declare support for it. Off by default: §13.6 makes cancellation
	// best-effort at the actor.
	RequireCancellation bool
}

func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

func (c Config) callbackWait() time.Duration {
	if c.CallbackWait > 0 {
		return c.CallbackWait
	}
	return DefaultCallbackWait
}

func (c Config) withDefaults() Config {
	if c.WorkflowName == "" {
		c.WorkflowName = "conformance-kit"
	}
	if c.WorkflowDigest == "" {
		c.WorkflowDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	}
	if c.NodeID == "" {
		c.NodeID = "conformance"
	}
	if len(c.Input) == 0 {
		c.Input = json.RawMessage(`{}`)
	}
	return c
}

// suite holds one Run's shared machinery.
type suite struct {
	cfg      Config
	client   *actors.Client
	endpoint actors.Endpoint
	signer   *actors.TokenSigner
	receiver *Receiver
}

// Run executes the whole conformance suite against cfg.Endpoint.
//
// Each check is a subtest, so a failing adapter gets a list of exactly which
// protocol properties it does not hold rather than one opaque failure.
func Run(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.Endpoint == "" {
		t.Fatal("conformance: Config.Endpoint is required")
	}
	cfg = cfg.withDefaults()

	signer, err := actors.NewTokenSigner([]byte(kitSecret), actors.WithTokenTTL(2*time.Hour))
	if err != nil {
		t.Fatalf("conformance: build token signer: %v", err)
	}
	receiver := NewReceiver(signer, cfg.CallbackBaseURL)
	t.Cleanup(receiver.Close)

	s := &suite{
		cfg: cfg,
		// One HTTP request per invocation: the kit is measuring the actor's
		// answers, and a client-side retry would hide a flaky one.
		client:   actors.NewClient(actors.WithMaxRequests(1)),
		endpoint: actors.Endpoint{URL: cfg.Endpoint, AuthToken: cfg.AuthToken},
		signer:   signer,
		receiver: receiver,
	}

	t.Run("authentication-is-required", s.checkAuthRequired)
	t.Run("synchronous-result-shape", s.checkSyncResultShape)
	t.Run("idempotent-re-invocation", s.checkIdempotentReinvoke)
	t.Run("asynchronous-acceptance-and-callbacks", s.checkAsyncFlow)
	t.Run("cancellation-endpoint", s.checkCancellation)
	t.Run("contract-failure-classification", s.checkContractFailure)
}

// newInvocation builds a §13.1 request with a fresh attempt id and a callback
// block pointing at the kit's receiver.
func (s *suite) newInvocation(t *testing.T, input json.RawMessage) actors.InvocationRequest {
	t.Helper()
	attemptID := "att_" + store.NewULID()
	token, err := s.signer.Mint(attemptID)
	if err != nil {
		t.Fatalf("conformance: mint callback token: %v", err)
	}
	deadline := time.Now().UTC().Add(s.cfg.callbackWait())
	return actors.InvocationRequest{
		ProtocolVersion: actors.ProtocolVersion,
		RunID:           "run_" + store.NewULID(),
		TokenID:         "tok_" + store.NewULID(),
		NodeRunID:       "nr_" + store.NewULID(),
		AttemptID:       attemptID,
		Attempt:         1,
		Workflow:        actors.WorkflowRef{Name: s.cfg.WorkflowName, VersionDigest: s.cfg.WorkflowDigest},
		Node:            actors.NodeRef{ID: s.cfg.NodeID, ContractDigest: s.cfg.ContractDigest},
		Input:           input,
		Deadline:        &deadline,
		Callback: actors.Callback{
			URL:   s.receiver.CallbackURL(attemptID),
			Token: token,
		},
	}
}

func (s *suite) ctx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), s.cfg.timeout())
}

// §13.1 sends a scoped workload token. An actor that executes work for an
// unauthenticated caller is an open endpoint.
func (s *suite) checkAuthRequired(t *testing.T) {
	if s.cfg.AuthToken == "" {
		t.Skip("no auth token configured: an unauthenticated endpoint is a legitimate local deployment")
	}
	ctx, cancel := s.ctx(t)
	defer cancel()

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"no credential", ""},
		{"wrong credential", "definitely-not-the-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := actors.Endpoint{URL: s.cfg.Endpoint, AuthToken: tc.token}
			resp, err := s.client.Invoke(ctx, endpoint, s.newInvocation(t, s.cfg.Input))
			if err == nil {
				t.Fatalf("the actor accepted an invocation with %s (status %d); §13.1 requires a scoped workload token",
					tc.name, resp.StatusCode)
			}
			class, ok := actors.ClassOf(err)
			if !ok {
				t.Fatalf("the refusal was not classifiable: %v", err)
			}
			if class != actors.ClassAuthOrPolicy {
				t.Errorf("refusal classified as %s, want %s: answer 401 or 403 so the adapter does not retry it",
					class, actors.ClassAuthOrPolicy)
			}
			if class.Retryable() {
				t.Errorf("class %s is retryable; a credential refusal must not be retried", class)
			}
		})
	}
}

// §13.2: a 200 carries a domain outcome and a JSON output.
func (s *suite) checkSyncResultShape(t *testing.T) {
	ctx, cancel := s.ctx(t)
	defer cancel()

	resp, err := s.client.Invoke(ctx, s.endpoint, s.newInvocation(t, s.cfg.Input))
	if err != nil {
		t.Fatalf("synchronous invocation failed: %v", err)
	}
	if resp.Async {
		t.Skip("the actor answered 202 to the synchronous input; set Config.Input to something it answers inline")
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Result.Outcome == "" {
		t.Error("the result declares no domain outcome; §13.2's `outcome` is what the node routes on")
	}
	if len(resp.Result.Output) > 0 && !json.Valid(resp.Result.Output) {
		t.Errorf("output is not valid JSON: %s", resp.Result.Output)
	}
	for i, record := range resp.Result.Records() {
		if record.RecordType == "" {
			t.Errorf("ledger_delta.records[%d] declares no record type", i)
		}
	}
	if usage := resp.Result.Usage; usage != nil {
		if usage.InputTokens < 0 || usage.OutputTokens < 0 {
			t.Errorf("usage reports negative token counts: %+v", usage)
		}
	}
}

// §20.3: an actor that has already accepted an Idempotency-Key returns the
// result it already produced. This is the property that keeps at-least-once
// delivery from turning a retried hop into duplicated side effects.
func (s *suite) checkIdempotentReinvoke(t *testing.T) {
	ctx, cancel := s.ctx(t)
	defer cancel()

	req := s.newInvocation(t, s.cfg.Input)
	first, err := s.client.Invoke(ctx, s.endpoint, req)
	if err != nil {
		t.Fatalf("first invocation failed: %v", err)
	}
	second, err := s.client.Invoke(ctx, s.endpoint, req)
	if err != nil {
		t.Fatalf("re-invocation with the same Idempotency-Key failed: %v", err)
	}

	if first.Async != second.Async {
		t.Fatalf("re-invocation changed shape: first async=%v, second async=%v", first.Async, second.Async)
	}
	if first.Async {
		if first.Accepted.InvocationID != second.Accepted.InvocationID {
			t.Errorf("re-invocation returned a different invocation_id (%q then %q); the same key must name the same work",
				first.Accepted.InvocationID, second.Accepted.InvocationID)
		}
		return
	}
	if first.Result.Outcome != second.Result.Outcome {
		t.Errorf("re-invocation returned outcome %q, want the recorded %q",
			second.Result.Outcome, first.Result.Outcome)
	}
	if !equalJSON(first.Result.Output, second.Result.Output) {
		t.Errorf("re-invocation returned a different output:\n first: %s\nsecond: %s",
			first.Result.Output, second.Result.Output)
	}
}

// §13.3 and §13.4: acceptance, liveness, a strictly increasing sequence,
// stable event ids, and a terminal event.
func (s *suite) checkAsyncFlow(t *testing.T) {
	if len(s.cfg.AsyncInput) == 0 {
		t.Skip("no async input configured: set Config.AsyncInput to an input the actor answers with 202")
	}

	ctx, cancel := s.ctx(t)
	defer cancel()

	req := s.newInvocation(t, s.cfg.AsyncInput)
	if s.cfg.ExpectCallbackRetry {
		// Refuse the first terminal delivery, so a conforming actor's
		// redelivery — with the SAME event id — becomes observable.
		s.receiver.RefuseNextTerminal(req.AttemptID, 1)
	}

	resp := s.invokeAsync(t, ctx, req)
	terminal, events := s.waitForTerminal(t, req)
	s.assertCallbackDiscipline(t, events, resp.Accepted)
	s.assertTerminalPayload(t, terminal)

	if s.cfg.ExpectCallbackRetry {
		s.assertTerminalRedelivered(t, req.AttemptID, events)
	}
}

// invokeAsync sends req and checks the §13.3 acceptance shape.
func (s *suite) invokeAsync(t *testing.T, ctx context.Context, req actors.InvocationRequest) actors.InvocationResponse {
	t.Helper()
	resp, err := s.client.Invoke(ctx, s.endpoint, req)
	if err != nil {
		t.Fatalf("asynchronous invocation failed: %v", err)
	}
	if !resp.Async {
		t.Skipf("the actor answered %d to the async input; set Config.AsyncInput to something it defers",
			resp.StatusCode)
	}
	if resp.Accepted.InvocationID == "" {
		t.Error("the acceptance declares no invocation_id; there is then nothing to cancel and nothing to correlate")
	}
	if resp.Accepted.HeartbeatAfterSeconds < 0 {
		t.Errorf("heartbeat_after_seconds = %d, want zero or positive", resp.Accepted.HeartbeatAfterSeconds)
	}
	return resp
}

// waitForTerminal waits for req's terminal callback and returns it along
// with every event the receiver recorded for the attempt.
func (s *suite) waitForTerminal(t *testing.T, req actors.InvocationRequest) (actors.CallbackEvent, []actors.CallbackEvent) {
	t.Helper()
	terminal, ok := s.receiver.WaitForTerminal(req.AttemptID, s.cfg.callbackWait())
	if !ok {
		t.Fatalf("no terminal callback arrived within %s; events seen: %v",
			s.cfg.callbackWait(), kindsOf(s.receiver.Events(req.AttemptID)))
	}
	return terminal, s.receiver.Events(req.AttemptID)
}

// assertTerminalPayload checks the §13.2/§13.5 shape of whichever terminal
// event arrived.
func (s *suite) assertTerminalPayload(t *testing.T, terminal actors.CallbackEvent) {
	t.Helper()
	switch terminal.Kind {
	case actors.EventCompleted:
		var payload actors.CompletedPayload
		if len(terminal.Payload) > 0 {
			if err := json.Unmarshal(terminal.Payload, &payload); err != nil {
				t.Errorf("completed payload is not a §13.2 result body: %v", err)
			}
		}
		if payload.Outcome == "" {
			t.Error("the completed event declares no domain outcome")
		}
	case actors.EventFailed:
		var payload actors.FailedPayload
		if len(terminal.Payload) > 0 {
			_ = json.Unmarshal(terminal.Payload, &payload)
		}
		if payload.Class != "" && !payload.Class.Valid() {
			t.Errorf("failed event declares class %q, which is not one of §13.5's classes", payload.Class)
		}
	}
}

// assertTerminalRedelivered checks that a refused terminal delivery was
// retried, per §13.4's idempotent redelivery.
func (s *suite) assertTerminalRedelivered(t *testing.T, attemptID string, events []actors.CallbackEvent) {
	t.Helper()
	if s.receiver.RefusedDeliveries(attemptID) == 0 {
		t.Error("the receiver never refused a delivery; the retry check proved nothing")
	}
	terminalDeliveries := 0
	for _, ev := range events {
		if ev.Kind.Terminal() {
			terminalDeliveries++
		}
	}
	if terminalDeliveries < 2 {
		t.Errorf("the terminal event was delivered %d time(s) after a 503; §13.4's idempotent redelivery "+
			"presupposes the actor retries a refused delivery", terminalDeliveries)
	}
}

// assertCallbackDiscipline checks the §13.4 properties that hold for every
// callback stream, whatever the actor is doing.
func (s *suite) assertCallbackDiscipline(t *testing.T, events []actors.CallbackEvent, accepted *actors.AsyncAccepted) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no callbacks were received at all")
	}

	// Stable event ids: a repeated id must be a REDELIVERY of the same event,
	// not a different one wearing the same name.
	seen := make(map[string]actors.CallbackEvent, len(events))
	highest := int64(0)
	sawNonTerminal := false

	for i, ev := range events {
		nonTerminal, newHighest := assertCallbackEvent(t, i, ev, seen, highest)
		if nonTerminal {
			sawNonTerminal = true
		}
		highest = newHighest
	}

	if accepted != nil && accepted.HeartbeatAfterSeconds > 0 && !sawNonTerminal {
		t.Errorf("the actor declared heartbeat_after_seconds=%d but sent no accepted/heartbeat/progress event; "+
			"a declared heartbeat interval is a promise about liveness", accepted.HeartbeatAfterSeconds)
	}
}

// assertCallbackEvent checks one callback event's §13.4 shape (a non-empty
// id, a positive sequence, a valid kind) and its relationship to the events
// seen so far: a repeated id must be a redelivery of the same event (same
// sequence, same payload), and a fresh id's sequence must advance past
// highest. It records ev under its id in seen and returns whether ev is
// non-terminal and the new high-water sequence.
func assertCallbackEvent(t *testing.T, i int, ev actors.CallbackEvent, seen map[string]actors.CallbackEvent, highest int64) (nonTerminal bool, newHighest int64) {
	t.Helper()
	if ev.EventID == "" {
		t.Errorf("event %d carries no event_id; a redelivery would then be indistinguishable from a new event", i)
	}
	if ev.Sequence <= 0 {
		t.Errorf("event %d (%s) carries sequence %d, want a positive value", i, ev.Kind, ev.Sequence)
	}
	if !ev.Kind.Valid() {
		t.Errorf("event %d declares kind %q, which is not one of §13.4's kinds", i, ev.Kind)
	}
	nonTerminal = !ev.Kind.Terminal()

	if previous, repeat := seen[ev.EventID]; repeat {
		if previous.Sequence != ev.Sequence || !equalJSON(previous.Payload, ev.Payload) {
			t.Errorf("event id %q was reused for a different event; §13.4 requires a stable id per event\n first: seq %d %s\nsecond: seq %d %s",
				ev.EventID, previous.Sequence, previous.Payload, ev.Sequence, ev.Payload)
		}
		return nonTerminal, highest // a redelivery does not have to advance the sequence
	}
	seen[ev.EventID] = ev

	if ev.Sequence <= highest {
		t.Errorf("event %d (%s) carries sequence %d, which does not advance past %d; §13.4 requires a monotonically increasing sequence",
			i, ev.Kind, ev.Sequence, highest)
	}
	if ev.Sequence > highest {
		highest = ev.Sequence
	}
	return nonTerminal, highest
}

// §13.6: cancellation is durable in Culture Nodes and best-effort at the
// actor. The check requires the endpoint to exist and answer when the actor
// declared it supports cancellation — not that the work actually stops.
func (s *suite) checkCancellation(t *testing.T) {
	if len(s.cfg.AsyncInput) == 0 {
		t.Skip("no async input configured: there is nothing in flight to cancel")
	}
	ctx, cancel := s.ctx(t)
	defer cancel()

	resp, err := s.client.Invoke(ctx, s.endpoint, s.newInvocation(t, s.cfg.AsyncInput))
	if err != nil {
		t.Fatalf("asynchronous invocation failed: %v", err)
	}
	if !resp.Async {
		t.Skip("the actor answered synchronously; there is nothing in flight to cancel")
	}
	if !resp.Accepted.SupportsCancellation {
		if s.cfg.RequireCancellation {
			t.Fatal("the actor does not declare supports_cancellation, and this suite was configured to require it")
		}
		t.Skip("the actor declares supports_cancellation=false; §13.6 makes cancellation best-effort at the actor")
	}

	if err := s.client.Cancel(ctx, s.endpoint, resp.Accepted.InvocationID, "conformance check"); err != nil {
		t.Fatalf("the actor declared supports_cancellation but its cancellation endpoint refused: %v", err)
	}
}

// §13.5: an input the actor rejects is a NON-retryable failure. This is the
// check that stops a contract failure being retried forever.
func (s *suite) checkContractFailure(t *testing.T) {
	if len(s.cfg.BadInput) == 0 {
		t.Skip("no bad input configured: set Config.BadInput to an input the actor must reject")
	}
	ctx, cancel := s.ctx(t)
	defer cancel()

	resp, err := s.client.Invoke(ctx, s.endpoint, s.newInvocation(t, s.cfg.BadInput))
	if err == nil {
		t.Fatalf("the actor accepted an input it should reject (status %d, outcome %q)",
			resp.StatusCode, outcomeOf(resp))
	}
	class, ok := actors.ClassOf(err)
	if !ok {
		t.Fatalf("the refusal was not classifiable: %v", err)
	}
	switch class {
	case actors.ClassActorRejectedInput, actors.ClassContract:
	default:
		t.Errorf("a rejected input was classified as %s; answer 400 or 422 so it maps to %s",
			class, actors.ClassActorRejectedInput)
	}
	if class.Retryable() {
		t.Errorf("class %s is retryable; an input the actor refuses will be refused again", class)
	}
}

func outcomeOf(resp actors.InvocationResponse) string {
	if resp.Result != nil {
		return resp.Result.Outcome
	}
	return ""
}

func kindsOf(events []actors.CallbackEvent) []actors.EventKind {
	kinds := make([]actors.EventKind, 0, len(events))
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	return kinds
}

// equalJSON compares two JSON documents by value, so formatting differences
// between a first response and a replayed one are not reported as a change in
// what the actor said.
func equalJSON(a, b json.RawMessage) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	var av, bv any
	if err := json.Unmarshal(nonEmpty(a), &av); err != nil {
		return false
	}
	if err := json.Unmarshal(nonEmpty(b), &bv); err != nil {
		return false
	}
	ac, err := json.Marshal(av)
	if err != nil {
		return false
	}
	bc, err := json.Marshal(bv)
	if err != nil {
		return false
	}
	return string(ac) == string(bc)
}

func nonEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}
