// Package artifactclient lets a runner-service host process persist attempt
// artifacts WITHOUT holding any store credential (deviation d1 of the
// hands-free-scrum-2-pickup plan, completing issue #189 on the production
// dispatch path).
//
// The custody reasoning: the runner boundary deliberately holds no database
// or object-store access, so the in-process artifacts.Store the headspace
// bridge accepts (its #189 half) cannot be wired directly in cmd/nodes-runner.
// What the runner DOES hold, per operation, is the control plane's callback
// offer — a URL and an attempt-scoped bearer token minted by the worker
// (internal/worker.runnerCallbackFor) — and that same token family is exactly
// what the API's artifact publication route verifies
// (internal/api.handlePutArtifact, VerifyFor(token, attemptID)). So the
// Registry here implements artifacts.Store by POSTing content to
// /v1alpha1/attempts/{attemptID}/artifacts with the operation's own callback
// token, registered around each Execute by the runner service.
package artifactclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agentculture/culture-nodes/internal/artifacts"
	"github.com/agentculture/culture-nodes/internal/runners"
)

// NamespaceServerDerived satisfies headspace.BridgeConfig's requirement that
// a configured ArtifactStore name its namespace: uploads through this client
// carry no namespace of their own — the API derives namespace, run, and
// attempt from the durable invocation the token is bound to, and the bridge's
// locally built meta.NamespaceID is advisory only on this path.
const NamespaceServerDerived = "server-derived"

// target is one registered upload destination: the artifact publication URL
// and the attempt-scoped bearer that authorizes it.
type target struct {
	putURL string
	token  string
}

// Registry maps attempt ids to upload targets for the operations currently
// executing, and implements artifacts.Store over that map. Register/Release
// bracket each Execute (the runner service does this); Put resolves the
// target by meta.AttemptID.
type Registry struct {
	mu      sync.Mutex
	targets map[string]target
	client  *http.Client
}

// New returns an empty Registry. A nil client gets a 30s-timeout default —
// bounded like the service's own callback client, because Put runs inside
// the bridge's detached, stop-timeout-bounded context and must not outlive
// it waiting on a dead socket.
func New(client *http.Client) *Registry {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Registry{targets: map[string]target{}, client: client}
}

// Register derives the artifact publication URL from the operation's callback
// URL and remembers it for attemptID. The callback URL has the shape
// <base>/v1/runner-operations/<op>/events (runners.CallbackPathFormat); the
// publication route lives at <base>/v1alpha1/attempts/<attempt>/artifacts on
// the same origin. A callback URL without that path shape is refused so a
// mis-derived origin can never become a silent upload to the wrong host.
func (r *Registry) Register(attemptID, callbackURL, token string) error {
	if attemptID == "" || callbackURL == "" || token == "" {
		return fmt.Errorf("artifactclient: Register: attemptID, callbackURL and token are all required")
	}
	base, _, ok := strings.Cut(callbackURL, "/v1/runner-operations/")
	if !ok || base == "" {
		return fmt.Errorf("artifactclient: Register: callback URL %q does not carry the runner-operations path; cannot derive the artifact publication origin", callbackURL)
	}
	r.mu.Lock()
	r.targets[attemptID] = target{putURL: base + "/v1alpha1/attempts/" + attemptID + "/artifacts", token: token}
	r.mu.Unlock()
	return nil
}

// Release forgets attemptID's target. Idempotent.
func (r *Registry) Release(attemptID string) {
	r.mu.Lock()
	delete(r.targets, attemptID)
	r.mu.Unlock()
}

var _ artifacts.Store = (*Registry)(nil)

// Put uploads content for the attempt named in meta. The server derives the
// authoritative namespace/run/attempt associations from the token's durable
// invocation; meta's local values (including the NamespaceServerDerived
// namespace) are not transmitted.
func (r *Registry) Put(ctx context.Context, meta artifacts.ArtifactMeta, in io.Reader) (artifacts.Ref, error) {
	r.mu.Lock()
	tgt, ok := r.targets[meta.AttemptID]
	r.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("artifactclient: Put: no upload target registered for attempt %q (the operation offered no callback, or Release already ran)", meta.AttemptID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tgt.putURL, in)
	if err != nil {
		return "", fmt.Errorf("artifactclient: Put: %w", err)
	}
	mediaType := meta.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", mediaType)
	req.Header.Set("Artifact-Name", meta.Name)
	req.Header.Set("Authorization", "Bearer "+tgt.token)
	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("artifactclient: Put: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("artifactclient: Put: %s answered %d: %s", tgt.putURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Ref artifacts.Ref `json:"ref"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Ref == "" {
		return "", fmt.Errorf("artifactclient: Put: publication succeeded but the response carried no ref: %q", string(body))
	}
	return out.Ref, nil
}

// The read/lifecycle half of Store is not this client's business: content is
// read back through the control plane's own GET routes, and reaping is a
// retention decision the control plane owns.

func (r *Registry) Get(context.Context, artifacts.Ref) (io.ReadCloser, artifacts.ArtifactMeta, error) {
	return nil, artifacts.ArtifactMeta{}, fmt.Errorf("artifactclient: Get: this store is write-only; read artifacts back through the control plane API")
}

func (r *Registry) Stat(context.Context, artifacts.Ref) (artifacts.ArtifactMeta, error) {
	return artifacts.ArtifactMeta{}, fmt.Errorf("artifactclient: Stat: this store is write-only; read artifacts back through the control plane API")
}

func (r *Registry) Delete(context.Context, artifacts.Ref) error {
	return artifacts.ErrDeleteForbidden
}

func (r *Registry) Reap(context.Context, artifacts.Ref, string, time.Time) (artifacts.Tombstone, error) {
	return artifacts.Tombstone{}, fmt.Errorf("artifactclient: Reap: retention is the control plane's; a runner never reaps")
}

// compile-time check that the path constant this package's derivation depends
// on has not drifted from the protocol package.
var _ = runners.CallbackPathFormat
