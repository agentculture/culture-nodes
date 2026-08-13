package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// runDetail is the slice of GET /v1alpha1/runs/{id} this daemon reads: just
// enough to build a notify.Payload (RunID, Workflow, Actor) and nothing
// else -- see fetchRunDetail's doc comment for why this is a local,
// hand-trimmed shape rather than an import of internal/api's own response
// types.
type runDetail struct {
	RunID          string
	WorkflowDigest string
	Actor          string
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
