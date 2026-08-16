package actors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Endpoint is a resolved actor: where to POST, what credential to present,
// and the deployment facts about the actor that the invocation itself must
// carry.
//
// It carries no provider, model, or vendor field, and it never will — §9.5
// puts those in telemetry metadata reported *by* the adapter, not in the
// dispatch path. RepositoryIdentity is not one of those: it is not a fact
// about which engine runs behind the actor, it is a fact about which
// repository this deployment registered the actor to work in, and it reaches
// the actor through the same registry read that answers where to POST.
type Endpoint struct {
	// URL is the actor's base URL. InvocationPath is appended to it, so
	// "https://actor.example" and "https://actor.example/" both invoke
	// https://actor.example/v1/invocations.
	//
	// A URL that already ends in InvocationPath is used as-is, so an operator
	// who registered the full invocation URL is not punished for it.
	URL string
	// AuthToken is the scoped workload token §13.1 sends as a bearer
	// credential. Empty means the endpoint is unauthenticated, which is
	// legitimate only for a local or in-cluster actor.
	AuthToken string
	// Header carries any additional static headers the endpoint requires.
	// Protocol headers (Idempotency-Key, Authorization, Content-Type) are set
	// by the client and win over anything here.
	Header http.Header
	// RepositoryIdentity is the repository the registration says this actor
	// works in, sent on every dispatch under RepositoryIdentityKey (issue
	// #125). Empty means the registration declares none, and the dispatch
	// carries no identity at all — see WithRepositoryIdentity for why that
	// is a removal rather than a passthrough.
	RepositoryIdentity string
	// DialIn routes through an authenticated reverse connection. URL remains
	// populated during mixed mode as the outbound fallback.
	DialIn          DialInInvoker
	DialInNamespace string
	DialInActorKey  string
}

// DialInInvoker is implemented by the PostgreSQL-backed durable mailbox.
type DialInInvoker interface {
	InvokeInbound(context.Context, string, string, InvocationRequest) (InvocationResponse, error)
}

// invocationURL is the full URL for a §13.1 invocation.
func (e Endpoint) invocationURL() string {
	base := strings.TrimRight(e.URL, "/")
	if strings.HasSuffix(base, InvocationPath) {
		return base
	}
	return base + InvocationPath
}

// cancelURL is the full URL for a §13.6 cancellation of one invocation.
func (e Endpoint) cancelURL(invocationID string) string {
	return e.invocationURL() + "/" + invocationID + "/cancel"
}

// Client defaults. They are named constants rather than literals because a
// deployment tuning dispatch behaviour should be changing something with a
// name, not a number found by grep.
const (
	// DefaultTimeout bounds one HTTP request, not one invocation: a
	// long-running actor is expected to answer 202 quickly and report the
	// rest through callbacks (§12.6), so an invocation POST that takes
	// minutes is a malfunction, not a long job.
	DefaultTimeout = 60 * time.Second
	// DefaultMaxRequests is how many HTTP requests one Invoke may spend,
	// including retries of retryable classes. The default of 3 is small on
	// purpose: the engine's per-node retry policy is the real retry budget,
	// and this layer only exists to ride out a single unlucky hop.
	DefaultMaxRequests = 3
	// DefaultRetryBackoff is the first delay between retries; each retry
	// doubles it, capped at DefaultRetryBackoffMax.
	DefaultRetryBackoff = 250 * time.Millisecond
	// DefaultRetryBackoffMax caps the exponential growth.
	DefaultRetryBackoffMax = 5 * time.Second
	// maxResponseBytes bounds a response body the client will read. A
	// runaway actor must not be able to exhaust a worker's memory.
	maxResponseBytes = 8 << 20 // 8 MiB
)

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the underlying HTTP client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.http = hc
		}
	}
}

// WithMaxRequests bounds how many HTTP requests one Invoke may spend. One
// means no transport-level retry at all.
func WithMaxRequests(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxRequests = n
		}
	}
}

// WithRetryBackoff sets the first retry delay and the cap on its exponential
// growth.
func WithRetryBackoff(base, max time.Duration) Option {
	return func(c *Client) {
		if base >= 0 {
			c.retryBase = base
		}
		if max >= 0 {
			c.retryMax = max
		}
	}
}

// WithUserAgent sets the User-Agent header sent to actors.
func WithUserAgent(ua string) Option {
	return func(c *Client) {
		if ua != "" {
			c.userAgent = ua
		}
	}
}

// WithSleep replaces the retry sleep, so a test can prove the backoff without
// waiting for it.
func WithSleep(sleep func(context.Context, time.Duration) error) Option {
	return func(c *Client) {
		if sleep != nil {
			c.sleep = sleep
		}
	}
}

// Client speaks PRD §13 to actor endpoints. It is safe for concurrent use.
type Client struct {
	http        *http.Client
	maxRequests int
	retryBase   time.Duration
	retryMax    time.Duration
	userAgent   string
	sleep       func(context.Context, time.Duration) error
	now         func() time.Time
}

// NewClient returns a client with the documented defaults.
func NewClient(opts ...Option) *Client {
	c := &Client{
		// Keep-alive is deliberately off: several actor bridges are
		// single-threaded HTTP servers, and a kept-alive dispatch
		// connection parks such a server's only thread reading the next
		// request on that socket — starving every other caller (measured
		// live 2026-08-12: run cancellation could not reach a bridge while
		// a dispatch connection sat idle-open; the bridge never even
		// accepted the cancel). One connection per request costs a TCP
		// handshake on a LAN and buys every request a fair turn.
		http: &http.Client{
			Timeout:   DefaultTimeout,
			Transport: &http.Transport{DisableKeepAlives: true},
		},
		maxRequests: DefaultMaxRequests,
		retryBase:   DefaultRetryBackoff,
		retryMax:    DefaultRetryBackoffMax,
		userAgent:   "culture-nodes/actor-protocol " + ProtocolVersion,
		sleep:       sleepCtx,
		now:         func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// Invoke performs one PRD §13.1 invocation.
//
// The response is §13.2's synchronous result (HTTP 200) or §13.3's
// asynchronous acceptance (HTTP 202); anything else is an *InvocationError
// carrying a §13.5 class.
//
// Every request carries the attempt id as the Idempotency-Key, which is what
// makes the retry loop below safe: a retryable class means the same key may
// be presented again, and §20.3 obliges the actor to return the result it
// already produced rather than start the work twice. Non-retryable classes
// return on the first response.
func (c *Client) Invoke(ctx context.Context, endpoint Endpoint, req InvocationRequest) (InvocationResponse, error) {
	if err := validateInvocation(endpoint, req); err != nil {
		return InvocationResponse{}, err
	}
	if endpoint.DialIn != nil {
		response, err := endpoint.DialIn.InvokeInbound(ctx, endpoint.DialInNamespace, endpoint.DialInActorKey, req)
		if err == nil || endpoint.URL == "" || ctx.Err() != nil {
			return response, err
		}
		// Mixed mode retains the outbound path for a bridge whose durable
		// presence was current at resolution but whose mailbox failed before
		// accepting this invocation. The idempotency key makes this fallback
		// the same dispatch, not a second logical invocation.
	}
	if req.ProtocolVersion == "" {
		req.ProtocolVersion = ProtocolVersion
	}
	// §13.1 shows both as arrays. Encoding them as null would make an actor
	// that iterates the field without a nil check fail on a technicality.
	if req.ArtifactRefs == nil {
		req.ArtifactRefs = []string{}
	}
	if req.ContextRefs == nil {
		req.ContextRefs = []string{}
	}
	if len(req.Input) == 0 {
		req.Input = json.RawMessage(`{}`)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return InvocationResponse{}, &InvocationError{
			Class: ClassContract, Op: "invoke", Requests: 0,
			Message: "invocation body could not be encoded", Err: err,
		}
	}

	url := endpoint.invocationURL()
	var lastErr *InvocationError

	for request := 1; request <= c.maxRequests; request++ {
		resp, invErr := c.invokeOnce(ctx, url, endpoint, req, body)
		if invErr == nil {
			resp.Requests = request
			return resp, nil
		}
		invErr.Requests = request
		lastErr = invErr

		if !invErr.Retryable() || request == c.maxRequests {
			break
		}
		if err := c.sleep(ctx, c.backoff(request, invErr.RetryAfter)); err != nil {
			// The caller's context ended while waiting to retry. Report that
			// as what it is rather than as the actor's last failure.
			lastErr = &InvocationError{
				Class: classifyTransport(ctx, err), Op: "invoke", Requests: request,
				Message: "invocation abandoned while waiting to retry", Err: err,
			}
			break
		}
	}

	return InvocationResponse{}, lastErr
}

// ParseInvocationResponse applies the same protocol parsing to a dial-in
// mailbox response as the outbound HTTP client applies to an HTTP response.
func ParseInvocationResponse(status int, payload []byte) (InvocationResponse, error) {
	switch status {
	case http.StatusOK:
		var result InvocationResult
		if err := json.Unmarshal(payload, &result); err != nil {
			return InvocationResponse{}, fmt.Errorf("actors: dial-in 200 response is not a result: %w", err)
		}
		// A body that parsed cleanly but declared no outcome is rejected for a
		// different reason, and that reason has no underlying cause: there is
		// no json error to wrap. Folding it into the branch above would format
		// a nil error through %w, which renders the literal "%!w(<nil>)" into
		// the operator's message and leaves errors.Unwrap returning nil off a
		// wrapping error. This branch is deliberately unwrapped.
		if result.Outcome == "" {
			return InvocationResponse{}, errors.New("actors: dial-in 200 response is not a result: missing outcome")
		}
		return InvocationResponse{Result: &result, StatusCode: status, Requests: 1}, nil
	case http.StatusAccepted:
		var accepted AsyncAccepted
		if err := json.Unmarshal(payload, &accepted); err != nil {
			return InvocationResponse{}, fmt.Errorf("actors: dial-in 202 response is not an acceptance: %w", err)
		}
		return InvocationResponse{Async: true, Accepted: &accepted, StatusCode: status, Requests: 1}, nil
	default:
		// The dial-in path lifts the bridge's own rejection reason exactly as
		// the outbound path does (task t3): the transport inversion this
		// cycle is running must not re-lose the diagnostic issue #125 was
		// about the moment a bridge stops being reachable by address.
		return InvocationResponse{}, &InvocationError{Class: classifyStatus(status), Op: "invoke", StatusCode: status, Requests: 1, Body: capture(payload), ActorError: actorErrorFrom(payload), Message: "dial-in bridge refused invocation"}
	}
}

// invokeOnce performs exactly one HTTP request and classifies its outcome.
func (c *Client) invokeOnce(ctx context.Context, url string, endpoint Endpoint, req InvocationRequest, body []byte) (InvocationResponse, *InvocationError) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return InvocationResponse{}, &InvocationError{
			Class: ClassContract, Op: "invoke",
			Message: fmt.Sprintf("endpoint URL %q is not usable", url), Err: err,
		}
	}
	c.applyHeaders(httpReq, endpoint)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set(IdempotencyKeyHeader, req.AttemptID)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return InvocationResponse{}, &InvocationError{
			Class: classifyTransport(ctx, err), Op: "invoke",
			Message: "invocation request did not complete", Err: err,
		}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
	}()

	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		return InvocationResponse{}, &InvocationError{
			Class: classifyTransport(ctx, readErr), Op: "invoke", StatusCode: resp.StatusCode,
			Message: "invocation response body could not be read", Err: readErr,
		}
	}

	switch resp.StatusCode {
	case http.StatusOK:
		var result InvocationResult
		if err := json.Unmarshal(payload, &result); err != nil {
			return InvocationResponse{}, &InvocationError{
				Class: ClassContract, Op: "invoke", StatusCode: resp.StatusCode,
				Message: "200 response is not a §13.2 result body",
				Body:    capture(payload), Err: err,
			}
		}
		if result.Outcome == "" {
			return InvocationResponse{}, &InvocationError{
				Class: ClassContract, Op: "invoke", StatusCode: resp.StatusCode,
				Message: "200 response declares no domain outcome",
				Body:    capture(payload),
			}
		}
		return InvocationResponse{Result: &result, StatusCode: resp.StatusCode}, nil

	case http.StatusAccepted:
		var accepted AsyncAccepted
		if err := json.Unmarshal(payload, &accepted); err != nil {
			return InvocationResponse{}, &InvocationError{
				Class: ClassContract, Op: "invoke", StatusCode: resp.StatusCode,
				Message: "202 response is not a §13.3 acceptance body",
				Body:    capture(payload), Err: err,
			}
		}
		if accepted.InvocationID == "" {
			// Without an invocation id there is nothing to cancel and nothing
			// to correlate an operator's question against, so §13.3's field
			// is treated as required rather than advisory.
			return InvocationResponse{}, &InvocationError{
				Class: ClassContract, Op: "invoke", StatusCode: resp.StatusCode,
				Message: "202 response declares no invocation_id",
				Body:    capture(payload),
			}
		}
		return InvocationResponse{Async: true, Accepted: &accepted, StatusCode: resp.StatusCode}, nil
	}

	// classifyBody only ever narrows the status-based class towards
	// capacity_exhausted (see its doc comment in errors.go); every other
	// status code's classification is exactly what classifyStatus alone
	// would have produced, unchanged.
	class := classifyStatus(resp.StatusCode)
	if declared, ok := classifyBody(payload); ok {
		class = declared
	}

	// RetryAfter is attached unconditionally, including for a
	// non-retryable class like capacity_exhausted: Retryable() below keeps
	// this class out of the in-attempt backoff sleep (that would just be
	// the cascade issue #48 describes), but the parsed delay still rides
	// on the returned *InvocationError for whatever consults it next —
	// today an operator reading the attempt's diagnostic, and from task t9
	// on the circuit breaker deciding how long to pause the actor.
	usage, terminationReason, preserve := telemetryFromErrorBody(payload)
	return InvocationResponse{}, &InvocationError{
		Class:      class,
		Op:         "invoke",
		StatusCode: resp.StatusCode,
		Message:    fmt.Sprintf("actor answered %s", http.StatusText(resp.StatusCode)),
		Body:       capture(payload),
		// The actor's own reason, beside this package's summary of the
		// status (task t3, issue #125). Message names the status; this names
		// what the actor said was wrong with the request.
		ActorError:        actorErrorFrom(payload),
		RetryAfter:        parseRetryAfter(resp.Header.Get("Retry-After"), c.now()),
		Usage:             usage,
		TerminationReason: terminationReason,
		Preserve:          preserve,
	}
}

// telemetryFromErrorBody decodes the optional §13.2 usage block, the
// optional termination reason beside it, and the optional preserve-on-
// failure block (task t25/t26) that a bridge attaches to a non-2xx error
// body when its failed session still produced a parseable terminal result
// (issue #32; the bridges' sync_response failure branch). It reads the full
// payload rather than the bounded Body capture so a large error document
// cannot truncate the accounting away. Anything that is not a JSON object
// carrying those keys yields nils — absent stays absent, never fabricated
// zeros.
//
// The three are returned separately rather than as one block because they
// are independently present: a turn cancelled or capped before it produced
// a result names its reason with no usage to report (ADR 0009), and
// preserve-on-failure only ever runs on a genuine technical failure, not on
// every error body that carries usage.
func telemetryFromErrorBody(payload []byte) (*Usage, *string, *Preserve) {
	var body struct {
		Usage             *Usage    `json:"usage"`
		TerminationReason *string   `json:"termination_reason"`
		Preserve          *Preserve `json:"preserve"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, nil, nil
	}
	return body.Usage, body.TerminationReason, body.Preserve
}

// Cancel asks an actor to stop an in-flight invocation (PRD §13.6).
//
// It is best-effort by design. §13.6 is explicit that "workflow state does not
// depend on an external process acknowledging cancellation" — Culture Nodes
// has already recorded the cancellation durably by the time this is called,
// so a refusal, a 404, or an unreachable actor changes nothing about the run.
// The error is returned for logging and telemetry, not as a gate.
func (c *Client) Cancel(ctx context.Context, endpoint Endpoint, invocationID, reason string) error {
	if invocationID == "" {
		return &InvocationError{Class: ClassContract, Op: "cancel", Message: "invocation id is required"}
	}
	body, err := json.Marshal(CancelRequest{InvocationID: invocationID, Reason: reason})
	if err != nil {
		return &InvocationError{Class: ClassContract, Op: "cancel", Message: "cancel body could not be encoded", Err: err}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.cancelURL(invocationID), bytes.NewReader(body))
	if err != nil {
		return &InvocationError{Class: ClassContract, Op: "cancel", Message: "endpoint URL is not usable", Err: err}
	}
	c.applyHeaders(httpReq, endpoint)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(IdempotencyKeyHeader, "cancel:"+invocationID)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return &InvocationError{
			Class: classifyTransport(ctx, err), Op: "cancel", Requests: 1,
			Message: "cancellation request did not complete", Err: err,
		}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	return &InvocationError{
		Class: classifyStatus(resp.StatusCode), Op: "cancel", StatusCode: resp.StatusCode, Requests: 1,
		Message: fmt.Sprintf("actor answered %s to cancellation", http.StatusText(resp.StatusCode)),
		Body:    capture(payload),
	}
}

func (c *Client) applyHeaders(req *http.Request, endpoint Endpoint) {
	for name, values := range endpoint.Header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	if endpoint.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+endpoint.AuthToken)
	}
	req.Header.Set("User-Agent", c.userAgent)
}

// backoff is the delay before request number request+1. An actor's own
// Retry-After always wins over the client's schedule: it is the only party
// that knows when it will be ready.
func (c *Client) backoff(request int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if c.retryMax > 0 && retryAfter > c.retryMax {
			return c.retryMax
		}
		return retryAfter
	}
	if c.retryBase <= 0 {
		return 0
	}
	delay := c.retryBase << (request - 1)
	if c.retryMax > 0 && delay > c.retryMax {
		return c.retryMax
	}
	return delay
}

func validateInvocation(endpoint Endpoint, req InvocationRequest) error {
	fail := func(detail string) error {
		return &InvocationError{Class: ClassContract, Op: "invoke", Message: detail}
	}
	switch {
	case strings.TrimSpace(endpoint.URL) == "" && endpoint.DialIn == nil:
		return fail("endpoint URL is required")
	case req.AttemptID == "":
		// The attempt id is the Idempotency-Key. Sending an invocation
		// without one would make a retry indistinguishable from a second
		// dispatch, which is the one thing §20.3 exists to prevent.
		return fail("attempt id is required: it is the Idempotency-Key")
	case req.RunID == "":
		return fail("run id is required")
	case req.NodeRunID == "":
		return fail("node run id is required")
	case req.Node.ID == "":
		return fail("node id is required")
	}
	return nil
}

func capture(body []byte) string {
	if len(body) <= maxCapturedBodyBytes {
		return string(body)
	}
	return string(body[:maxCapturedBodyBytes]) + "…"
}

// sleepCtx waits for d, or returns the context's error if it ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// compile-time proof the sentinel matching in errors.go actually works.
var _ = errors.Is(&InvocationError{}, ErrInvocation)
