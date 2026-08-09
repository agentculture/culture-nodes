package runnerservice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// The optional completion callback.
//
// Everything about it is deliberately weak. It carries no result, so a forged
// or replayed notification costs the runtime at most one extra status read.
// It is never retried, so a receiver that is down cannot become this
// service's problem. It cannot fail an operation, because the operation was
// already terminal before the POST was attempted — the callback is a hint
// that a status is worth sampling now rather than at the next poll, and
// nothing else. A runner that never calls back at all is fully conformant.

// callbackTarget is where a completion notification goes and the bearer token
// the receiver issued for it. It lives in memory for the operation's lifetime
// and is never persisted: it is caller-issued bearer material, and the
// contract's own "best-effort, never retried" posture means losing it across
// a restart costs latency and nothing else.
type callbackTarget struct {
	url   string
	token string
}

// callbackFromHeaders reads the optional callback offer off a dispatch.
//
// Both headers are required together: a URL with no token is an endpoint this
// service could POST to without being able to prove who it is, which the
// receiving ingress must refuse anyway.
func callbackFromHeaders(r *http.Request, allow func(*url.URL) bool) (callbackTarget, bool) {
	raw := r.Header.Get(runners.CallbackURLHeader)
	token := r.Header.Get(runners.CallbackTokenHeader)
	if raw == "" || token == "" {
		return callbackTarget{}, false
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return callbackTarget{}, false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return callbackTarget{}, false
	}
	if parsed.User != nil {
		return callbackTarget{}, false
	}
	if allow != nil && !allow(parsed) {
		return callbackTarget{}, false
	}
	return callbackTarget{url: parsed.String(), token: token}, true
}

// notify POSTs the resultless completion notification, once, best-effort.
//
// A failure is reported as a diagnostic and then forgotten. It cannot be
// escalated: the operation is terminal, its result is on the status endpoint,
// and the runtime's polling learns the outcome whether this call lands or not.
func (s *Service) notify(target callbackTarget, operationID string, state runners.State) {
	body, err := json.Marshal(runners.CallbackNotification{OperationID: operationID, State: state})
	if err != nil {
		s.report(fmt.Errorf("runnerservice: encode the completion notification for %s: %w", operationID, err))
		return
	}

	// context.Background rather than the service's root context: a
	// notification about an operation that is already finished should not be
	// cancelled by the shutdown that finished it.
	ctx, cancel := context.WithTimeout(context.Background(), s.callbackTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.url, bytes.NewReader(body))
	if err != nil {
		s.report(fmt.Errorf("runnerservice: build the completion notification for %s: %w", operationID, err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(runners.AuthorizationHeader, "Bearer "+target.token)
	req.Header.Set(runners.ProtocolVersionHeader, runners.ProtocolVersion)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.report(fmt.Errorf("runnerservice: completion notification for %s was not delivered (polling still "+
			"learns the outcome): %w", operationID, err))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= http.StatusBadRequest {
		s.report(fmt.Errorf("runnerservice: completion notification for %s was refused with %d (polling still "+
			"learns the outcome)", operationID, resp.StatusCode))
	}
}

// defaultCallbackTimeout bounds one notification attempt. There is no retry,
// so this is the entire budget a callback ever gets.
const defaultCallbackTimeout = 10 * time.Second
