package runners

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// The caller half of api/runner-protocol: how this process submits an
// operation to a registered runner service and how it learns the outcome.
//
// Two properties of this client are load-bearing, and both are the reason it
// is a separate type from the in-process Runner interface:
//
//  1. It is ASYNCHRONOUS ONLY. Dispatch answers an Acceptance or an error;
//     it never answers a Result. There is no code path here that could hold a
//     connection open for the duration of an operation, so there is no code
//     path that could hold a lease open for it either. A runner that answers
//     200 with a result is refused as a contract failure rather than quietly
//     accommodated — accommodating it is how the synchronous fast path grows
//     back, and with it the lease-per-operation cost model §12.6 forbids.
//
//  2. A dispatch failure is NEVER a Result. Every refusal, throttle, auth
//     failure, transport error and unparseable body leaves this package as a
//     *DispatchError. Nothing here ever synthesises a `failed` Result, because
//     a Result is a claim that an execution happened and was observed, and in
//     all of these cases nothing was observed at all.
//
// The secret is resolved through a SecretResolver at call time and lives in a
// local variable for exactly one request. It is never stored on the client,
// never logged, and never included in an error — an error body is something an
// operator pastes into a ticket.

// CallbackPathFormat builds the URL a runner may POST a completion
// notification to, matching the example the protocol document shows
// ("https://nodes.example/v1/runner-operations/op_01JAV.../events"). The
// format argument is the operation id.
//
// Nothing is ever committed on a callback's word (see CallbackNotification),
// so this endpoint's only power is to make the next status sample happen
// sooner.
const CallbackPathFormat = "/v1/runner-operations/%s/events"

// Client defaults for the protocol transport.
const (
	// DefaultDispatchTimeout bounds one HTTP request, not one operation. The
	// protocol obliges a runner to answer 202 quickly ("a dispatch POST that
	// takes minutes is a malfunction, not a long job"), and a status read is
	// a lookup, so a request that outlives this is a broken runner rather
	// than a busy one.
	DefaultDispatchTimeout = 30 * time.Second
	// maxProtocolResponseBytes bounds a response body this client will read.
	// A runaway runner must not be able to exhaust a worker's memory.
	maxProtocolResponseBytes = 8 << 20 // 8 MiB
	// maxCapturedProtocolBodyBytes bounds how much of a failing response body
	// is quoted back in a DispatchError.
	maxCapturedProtocolBodyBytes = 512
)

// SecretResolver turns a registry secret *reference* into the bearer material
// presented to a runner service.
//
// It is an interface, and the registry stores only the reference, because the
// two live in different places on purpose: a ServiceIdentity is built from
// configuration that gets logged, diffed and committed, while the material
// comes from the deployment's secret source at dispatch time. A deployment
// backs this with its own secret manager; a test backs it with StaticSecrets.
type SecretResolver interface {
	// ResolveSecret returns the bearer secret named by ref. An error means no
	// request is sent: presenting no credential to a remote-code-execution
	// surface is not a fallback position.
	ResolveSecret(ctx context.Context, ref string) (string, error)
}

// StaticSecrets is a SecretResolver over an in-memory map, for a single-node
// deployment configured from the environment and for tests. It is deliberately
// the simplest possible implementation rather than a default: a deployment
// that wants a secret manager implements the interface.
type StaticSecrets map[string]string

// ResolveSecret implements SecretResolver.
func (s StaticSecrets) ResolveSecret(_ context.Context, ref string) (string, error) {
	secret, ok := s[ref]
	if !ok || secret == "" {
		return "", fmt.Errorf("runners: no secret is configured for reference %q", ref)
	}
	return secret, nil
}

// CallbackOffer is the optional completion-callback block a dispatch offers.
//
// The zero value offers nothing, and that is a fully supported deployment:
// polling is the runtime's responsibility and is sufficient on its own. When
// both fields are set the two protocol headers ride on the execute request;
// when either is empty neither header is sent, so a runner cannot distinguish
// "this deployment has no callback route" from "this deployment declined to
// offer one" — there is nothing to distinguish.
type CallbackOffer struct {
	// URL is where the runner may POST a CallbackNotification.
	URL string
	// Token is the bearer the runner must present on that POST, so a forged
	// notification is refused before it can even cost a status read.
	Token string
}

// offered reports whether this offer is complete enough to send. A URL with no
// token is never sent: it would advertise an endpoint that refuses every
// notification it receives.
func (o CallbackOffer) offered() bool { return o.URL != "" && o.Token != "" }

// ProtocolOption configures a ProtocolClient.
type ProtocolOption func(*ProtocolClient)

// WithProtocolHTTPClient replaces the underlying HTTP client, e.g. to pin a
// TLS configuration or a proxy.
func WithProtocolHTTPClient(hc *http.Client) ProtocolOption {
	return func(c *ProtocolClient) {
		if hc != nil {
			c.http = hc
		}
	}
}

// WithProtocolUserAgent sets the User-Agent presented to runner services.
func WithProtocolUserAgent(ua string) ProtocolOption {
	return func(c *ProtocolClient) {
		if ua != "" {
			c.userAgent = ua
		}
	}
}

// ProtocolClient speaks api/runner-protocol to registered runner services. It
// is safe for concurrent use and holds no per-operation state — by
// construction, since holding any would be the per-operation goroutine §12.6
// forbids.
type ProtocolClient struct {
	http      *http.Client
	secrets   SecretResolver
	userAgent string
}

// NewProtocolClient returns a client that resolves runner credentials through
// secrets.
//
// A nil resolver is refused rather than defaulted to "send no credential":
// every request on this protocol carries a bearer, and a client that could not
// produce one would fail at the first dispatch with a 401 from the runner
// instead of at construction with a diagnosable message.
func NewProtocolClient(secrets SecretResolver, opts ...ProtocolOption) (*ProtocolClient, error) {
	if secrets == nil {
		return nil, errors.New("runners: NewProtocolClient requires a secret resolver; " +
			"caller authentication is mandatory on the runner protocol")
	}
	c := &ProtocolClient{
		http:      &http.Client{Timeout: DefaultDispatchTimeout},
		secrets:   secrets,
		userAgent: "culture-nodes/runner-protocol " + ProtocolVersion,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c, nil
}

// Dispatch submits one operation and returns the runner's acceptance.
//
// It returns an Acceptance and nil only for a conformant 202 whose acceptance
// validates against the operation submitted. Everything else — including a
// 200 carrying a result — is a *DispatchError, and the caller has learned
// nothing about any execution.
//
// The operation crosses the wire verbatim. The idempotency key is the
// operation id (the schema says so), restated in the header so a gateway can
// act on it without parsing the body: re-sending it must return the acceptance
// the runner already issued, never start the work a second time.
func (c *ProtocolClient) Dispatch(ctx context.Context, identity ServiceIdentity, op Operation, callback CallbackOffer) (Acceptance, error) {
	if op.OperationID == "" {
		return Acceptance{}, refuse(ErrorRejectedInput, ErrUnsupportedOperation, "", identity.Endpoint,
			"the operation declares no operation_id, which is also its idempotency key; "+
				"a dispatch without one cannot be told apart from a second execution")
	}
	// Pinning that is not checked is not pinning. The registry recorded the
	// execution-environment digest this identity is allowed to run; an
	// operation declaring another one is refused here, before it is sent
	// anywhere, exactly as the function form refuses it.
	if identity.ImageDigest != "" && op.Execution.ImageDigest != "" && op.Execution.ImageDigest != identity.ImageDigest {
		return Acceptance{}, refuse(ErrorRejectedInput, ErrDigestMismatch, op.OperationID, identity.Endpoint,
			fmt.Sprintf("the operation declares execution digest %s but %s is registered to run %s",
				op.Execution.ImageDigest, identity.Endpoint, identity.ImageDigest))
	}

	body, err := json.Marshal(op)
	if err != nil {
		return Acceptance{}, refuse(ErrorRejectedInput, ErrUnsupportedOperation, op.OperationID, identity.Endpoint,
			fmt.Sprintf("the operation document could not be encoded: %v", err))
	}

	req, dispatchErr := c.newRequest(ctx, identity, http.MethodPost, identity.ExecuteURL(), op.OperationID, bytes.NewReader(body))
	if dispatchErr != nil {
		return Acceptance{}, dispatchErr
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(IdempotencyKeyHeader, op.OperationID)
	if callback.offered() {
		req.Header.Set(CallbackURLHeader, callback.URL)
		req.Header.Set(CallbackTokenHeader, callback.Token)
	}

	payload, status, dispatchErr := c.do(ctx, req, op.OperationID, identity.Endpoint)
	if dispatchErr != nil {
		return Acceptance{}, dispatchErr
	}

	if status != http.StatusAccepted {
		// A 2xx that is not 202 is the synchronous variant this protocol does
		// not have. It is refused rather than read: accepting a result the
		// runtime never sampled would make "completion is learned only by an
		// authenticated status read" a convention instead of an invariant.
		return Acceptance{}, refuse(ErrorContractFailure, nil, op.OperationID, identity.Endpoint,
			fmt.Sprintf("the runner answered %d to an execute request; this protocol is asynchronous only "+
				"and a dispatch is answered with 202 and an acceptance, never with a result", status))
	}

	var accepted Acceptance
	if err := json.Unmarshal(payload, &accepted); err != nil {
		return Acceptance{}, refuse(ErrorContractFailure, nil, op.OperationID, identity.Endpoint,
			fmt.Sprintf("the runner's 202 body is not an acceptance document: %v (%s)", err, capturedBody(payload)))
	}
	if err := accepted.Validate(op.OperationID); err != nil {
		return Acceptance{}, err
	}
	return accepted, nil
}

// Status reads one operation's authoritative state.
//
// This is the only path by which a completion is ever learned. A non-terminal
// state is a successful call carrying no result, and it is not "unknown yet,
// assume failure": it means the operation has not finished, and the caller
// samples again later.
//
// A 404 is deliberately NOT a completion. A runner that forgot an operation it
// accepted has broken its retention promise; the runtime resamples until the
// attempt's own waiting_external deadline fails it, which is an honest
// timeout rather than an invented outcome.
func (c *ProtocolClient) Status(ctx context.Context, identity ServiceIdentity, operationID string) (OperationStatus, error) {
	if operationID == "" {
		return OperationStatus{}, refuse(ErrorRejectedInput, ErrUnsupportedOperation, "", identity.Endpoint,
			"a status read needs an operation id")
	}

	req, dispatchErr := c.newRequest(ctx, identity, http.MethodGet, identity.StatusURL(operationID), operationID, nil)
	if dispatchErr != nil {
		return OperationStatus{}, dispatchErr
	}

	payload, status, dispatchErr := c.do(ctx, req, operationID, identity.Endpoint)
	if dispatchErr != nil {
		return OperationStatus{}, dispatchErr
	}
	if status != http.StatusOK {
		return OperationStatus{}, refuse(ErrorContractFailure, nil, operationID, identity.Endpoint,
			fmt.Sprintf("the runner answered %d to a status read with no error classification", status))
	}

	var operationStatus OperationStatus
	if err := json.Unmarshal(payload, &operationStatus); err != nil {
		return OperationStatus{}, refuse(ErrorContractFailure, nil, operationID, identity.Endpoint,
			fmt.Sprintf("the runner's status body is not an operation status document: %v (%s)", err, capturedBody(payload)))
	}
	if err := operationStatus.Validate(); err != nil {
		return OperationStatus{}, err
	}
	if operationStatus.OperationID != operationID {
		return OperationStatus{}, refuse(ErrorContractFailure, nil, operationID, identity.Endpoint,
			fmt.Sprintf("the status reports on operation %s, which is not the one that was read",
				operationStatus.OperationID))
	}
	return operationStatus, nil
}

// Cancel asks a runner to stop an operation, best effort.
//
// Cancellation is already durable in the control plane by the time this is
// called, so a 404, a 405, or an unreachable runner changes nothing about the
// run. The error is returned for logging, never as a gate.
func (c *ProtocolClient) Cancel(ctx context.Context, identity ServiceIdentity, operationID string) error {
	if operationID == "" {
		return refuse(ErrorRejectedInput, ErrUnsupportedOperation, "", identity.Endpoint,
			"a cancellation needs an operation id")
	}
	req, dispatchErr := c.newRequest(ctx, identity, http.MethodPost, identity.CancelURL(operationID), operationID, nil)
	if dispatchErr != nil {
		return dispatchErr
	}
	if _, _, err := c.do(ctx, req, operationID, identity.Endpoint); err != nil {
		return err
	}
	return nil
}

// newRequest builds an authenticated protocol request. The secret is resolved
// here, at call time, and lives only as long as this function's caller needs
// it: the registry stores a reference, and this is the one place the reference
// becomes material.
func (c *ProtocolClient) newRequest(
	ctx context.Context, identity ServiceIdentity, method, url, operationID string, body io.Reader,
) (*http.Request, *DispatchError) {
	if identity.SecretRef == "" {
		return nil, refuse(ErrorAuthOrPolicy, ErrAccessDenied, operationID, identity.Endpoint,
			"the registered runner service names no secret reference, so this caller cannot authenticate; "+
				"authentication is mandatory on every request, including status reads")
	}
	secret, err := c.secrets.ResolveSecret(ctx, identity.SecretRef)
	if err != nil {
		// The reference name is safe to print (it is configuration); the
		// resolver's own error may not be, so it is summarised rather than
		// wrapped verbatim.
		return nil, refuse(ErrorAuthOrPolicy, ErrAccessDenied, operationID, identity.Endpoint,
			fmt.Sprintf("the credential named %q could not be resolved from this deployment's secret source: %v",
				identity.SecretRef, err))
	}
	if secret == "" {
		return nil, refuse(ErrorAuthOrPolicy, ErrAccessDenied, operationID, identity.Endpoint,
			fmt.Sprintf("the credential named %q resolved to an empty secret", identity.SecretRef))
	}

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, refuse(ErrorRejectedInput, ErrUnsupportedOperation, operationID, identity.Endpoint,
			fmt.Sprintf("the registered endpoint does not form a usable %s URL: %v", method, err))
	}
	req.Header.Set(AuthorizationHeader, "Bearer "+secret)
	req.Header.Set(ProtocolVersionHeader, ProtocolVersion)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	return req, nil
}

// do performs one request and classifies its outcome. It returns the body and
// the status code for a 2xx, and a classified *DispatchError otherwise.
func (c *ProtocolClient) do(ctx context.Context, req *http.Request, operationID, endpoint string) ([]byte, int, *DispatchError) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, refuse(classifyTransportError(ctx, err), SentinelFor(classifyTransportError(ctx, err)),
			operationID, endpoint,
			fmt.Sprintf("the request to the runner did not complete: %v", err))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProtocolResponseBytes))
		_ = resp.Body.Close()
	}()

	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, maxProtocolResponseBytes))
	if readErr != nil {
		kind := classifyTransportError(ctx, readErr)
		return nil, resp.StatusCode, refuse(kind, SentinelFor(kind), operationID, endpoint,
			fmt.Sprintf("the runner's response body could not be read: %v", readErr))
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return payload, resp.StatusCode, nil
	}

	kind := ClassifyProtocolStatus(resp.StatusCode)
	return nil, resp.StatusCode, refuse(kind, SentinelFor(kind), operationID, endpoint,
		fmt.Sprintf("the runner answered %s (%d): %s",
			http.StatusText(resp.StatusCode), resp.StatusCode, capturedBody(payload)))
}

// ClassifyProtocolStatus maps an HTTP status onto the error kind the protocol
// document's table declares for it.
//
// It is exported because a runner *implementation* wants the same mapping when
// it decides which status to answer with, and two copies of this table would
// be two places for the vocabulary to drift.
//
// The one row worth restating: 404 on a status read is runner_unavailable and
// retryable, never a completion. A runner that forgets an operation has broken
// a promise; reading its silence as an outcome would put an unmeasured claim
// in the ledger.
func ClassifyProtocolStatus(status int) ErrorKind {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity,
		http.StatusConflict, http.StatusRequestEntityTooLarge:
		return ErrorRejectedInput
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrorAuthOrPolicy
	case http.StatusNotFound:
		return ErrorRunnerUnavailable
	case http.StatusTooManyRequests:
		return ErrorRateLimited
	}
	if status >= 500 {
		return ErrorRunnerUnavailable
	}
	// Any other unexpected status tells the runtime nothing it can record.
	return ErrorContractFailure
}

// classifyTransportError separates "the runner could not be reached" from
// "this process gave up waiting". A cancelled context is the caller's own
// decision and is reported as a cancellation rather than blamed on the runner.
func classifyTransportError(ctx context.Context, err error) ErrorKind {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return ErrorCancellation
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorTimeout
	}
	return ErrorRetryableTransport
}

// capturedBody quotes a bounded prefix of a failing response body. It is
// bounded because an error string ends up in a log line, and unbounded runner
// output in a log line is how a disk fills.
func capturedBody(payload []byte) string {
	if len(payload) == 0 {
		return "the response carried no body"
	}
	if len(payload) <= maxCapturedProtocolBodyBytes {
		return string(payload)
	}
	return string(payload[:maxCapturedProtocolBodyBytes]) + "…"
}
