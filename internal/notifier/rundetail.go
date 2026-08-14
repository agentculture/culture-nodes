package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agentculture/culture-nodes/internal/notify"
)

// runDetail is the slice of GET /v1alpha1/runs/{id} this daemon reads: just
// enough to build a notify.Payload (RunID, Workflow, Actor) and nothing
// else -- see fetchRunDetail's doc comment for why this is a local,
// hand-trimmed shape rather than an import of internal/api's own response
// types. WorkflowKey is the one field NOT carried by that response: it
// comes from a second, cached lookup against the workflows read API (see
// workflowKeyCache) and may legitimately stay empty when that lookup
// fails -- workflowLabel falls back to the digest.
type runDetail struct {
	RunID          string
	WorkflowDigest string
	WorkflowKey    string
	Actor          string
}

// shortDigestChars is how much of a content digest survives into a
// notification once a name is available: enough to disambiguate two
// versions of the same workflow at a glance, short enough that the name
// stays the thing a human reads first.
const shortDigestChars = 7

// workflowLabel renders the workflow identity for a notification:
// "parallel-live-proof (8d4c768)" when the key resolved, the full digest
// when it did not (honest about what is actually known rather than
// showing a truncated digest nobody can look up), and the bare key when a
// run somehow carries no digest at all.
func (d runDetail) workflowLabel() string {
	short := shortDigest(d.WorkflowDigest)
	switch {
	case d.WorkflowKey == "":
		return d.WorkflowDigest
	case short == "":
		return d.WorkflowKey
	default:
		return d.WorkflowKey + " (" + short + ")"
	}
}

// shortDigest drops a digest's algorithm prefix ("sha256:") and keeps the
// first shortDigestChars characters of what remains. Counts runes, not
// bytes, so a malformed non-ASCII "digest" is never split mid-encoding.
func shortDigest(digest string) string {
	hex := digest
	if _, after, found := strings.Cut(digest, ":"); found {
		hex = after
	}
	runes := []rune(hex)
	if len(runes) <= shortDigestChars {
		return hex
	}
	return string(runes[:shortDigestChars])
}

// payload shapes this detail into the notification internal/notify
// delivers, for the given lifecycle event type and dashboard base URL. It
// lives here, beside the fetch, so the Payload the daemon posts and the
// Payload the tests in rundetail_test.go assert against are built by the
// same code.
func (d runDetail) payload(event, dashboardBase string) notify.Payload {
	return notify.Payload{
		RunID:         d.RunID,
		Workflow:      d.workflowLabel(),
		Event:         event,
		Actor:         d.Actor,
		DashboardLink: strings.TrimRight(dashboardBase, "/") + "/runs/" + d.RunID,
	}
}

// runDetailResponse and its nested types mirror only the fields of
// components.schemas.RunView (internal/api/types.go's RunViewOut) this
// daemon needs. It is deliberately NOT internal/api.RunViewOut: importing
// internal/api would pull the control-plane's own HTTP handlers, its
// PostgreSQL store, and their transitive dependencies into what is
// supposed to be a small, standalone daemon that talks to the control
// plane only over HTTP -- exactly the boundary task t14 exists to keep.
// Extra fields in the real response are simply ignored by
// encoding/json.Unmarshal.
type runDetailResponse struct {
	Run      runOutSlice       `json:"run"`
	NodeRuns []nodeRunOutSlice `json:"node_runs"`
}

type runOutSlice struct {
	ID             string `json:"id"`
	WorkflowDigest string `json:"workflow_digest"`
}

type nodeRunOutSlice struct {
	UpdatedAt   time.Time         `json:"updated_at"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
	Attempts    []attemptOutSlice `json:"attempts"`
}

type attemptOutSlice struct {
	ActorID     string     `json:"actor_id,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// fetchRunDetail calls GET {apiBase}/v1alpha1/runs/{runID} -- the response
// nests the run itself under a "run" key alongside "tokens" and
// "node_runs" (RunViewOut) -- and reduces it to the fields a notification
// payload needs. It never reads run.input, run.output, any ledger
// projection, or any node's own output: boundary c40 (economy-discord-
// graphs) is enforced by this function simply never decoding those fields
// into anything, not by a runtime filter over richer data it already read.
func fetchRunDetail(ctx context.Context, client *http.Client, apiBase, runID string) (runDetail, error) {
	url := strings.TrimRight(apiBase, "/") + "/v1alpha1/runs/" + runID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return runDetail{}, fmt.Errorf("notifier: build run-detail request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return runDetail{}, fmt.Errorf("notifier: GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return runDetail{}, fmt.Errorf("notifier: GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out runDetailResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return runDetail{}, fmt.Errorf("notifier: decode run detail from %s: %w", url, err)
	}

	return runDetail{
		RunID:          out.Run.ID,
		WorkflowDigest: out.Run.WorkflowDigest,
		Actor:          deriveActor(out.NodeRuns),
	}, nil
}

// workflowVersionSlice mirrors only the two fields of
// components.schemas.WorkflowVersion (internal/api's WorkflowVersionOut)
// this daemon needs from GET /v1alpha1/workflows/{digest}. That response
// carries the entire workflow -- source_format, source, normalized_ir,
// every node definition and prompt in it -- and none of that may reach a
// notification (boundary c40). As with runDetailResponse, the guarantee is
// structural: there is no field here to decode any of it into.
type workflowVersionSlice struct {
	WorkflowKey string `json:"workflow_key"`
}

// workflowKeyCache memoizes digest -> workflow key. The mapping is
// immutable by construction: a content digest addresses exactly one
// normalized workflow version forever (see the PRD's content-addressing
// rule), so an entry can never go stale and needs no expiry. That is what
// makes it safe to hold a name for the whole life of the process and
// cheap to consult on every lifecycle event -- a busy run posting a dozen
// notifications costs exactly one workflows lookup, not a dozen.
//
// Only successful lookups are cached. A failure (control plane still
// starting, a transient 5xx) is left uncached so the next event retries
// rather than pinning a run's notifications to the digest forever.
type workflowKeyCache struct {
	mu   sync.RWMutex
	keys map[string]string
}

func newWorkflowKeyCache() *workflowKeyCache {
	return &workflowKeyCache{keys: make(map[string]string)}
}

// lookup returns the human-readable workflow key for digest, from cache
// when already known and otherwise from GET
// {apiBase}/v1alpha1/workflows/{digest}. An empty digest yields an empty
// key and no request. The error is the caller's to treat as non-fatal:
// a name is legibility, not correctness (see Daemon.handleFrame).
func (c *workflowKeyCache) lookup(ctx context.Context, client *http.Client, apiBase, digest string) (string, error) {
	if digest == "" {
		return "", nil
	}
	c.mu.RLock()
	key, ok := c.keys[digest]
	c.mu.RUnlock()
	if ok {
		return key, nil
	}

	url := strings.TrimRight(apiBase, "/") + "/v1alpha1/workflows/" + digest
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("notifier: build workflow-version request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("notifier: GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("notifier: GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out workflowVersionSlice
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("notifier: decode workflow version from %s: %w", url, err)
	}
	if out.WorkflowKey == "" {
		return "", fmt.Errorf("notifier: GET %s: response carries no workflow_key", url)
	}

	c.mu.Lock()
	c.keys[digest] = out.WorkflowKey
	c.mu.Unlock()
	return out.WorkflowKey, nil
}

// deriveActor picks the actor identity of the most recently active attempt
// across every node run in a run-detail response: the run-level view
// (RunOut) has no single "actor" field of its own -- a run can carry many
// node runs, each dispatched to a different actor -- so this is the
// closest honest answer to "who was last active on this run" available
// from one GET /v1alpha1/runs/{id} call. "Most recent" is judged by an
// attempt's CompletedAt when set, else its StartedAt; an actor-less run (no
// attempt has dispatched yet, e.g. a very early run.created notification)
// returns "" -- Payload.Actor's doc comment names this as an expected,
// valid case, not an error.
func deriveActor(nodeRuns []nodeRunOutSlice) string {
	var (
		best    string
		bestAt  time.Time
		haveAny bool
	)
	for _, nr := range nodeRuns {
		for _, at := range nr.Attempts {
			if at.ActorID == "" {
				continue
			}
			candidate := at.StartedAt
			if at.CompletedAt != nil {
				candidate = *at.CompletedAt
			}
			if !haveAny || candidate.After(bestAt) {
				best, bestAt, haveAny = at.ActorID, candidate, true
			}
		}
	}
	return best
}
