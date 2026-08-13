package main

// `nodes run` (task t19, issue #36): the ad-hoc run lane. One invocation
// takes an instruction and an actor and calls POST /v1alpha1/adhoc-runs,
// which renders the canonical one-node workflow, publishes it idempotently
// by digest, and creates a normal pinned-digest run — see
// internal/api/adhoc.go. The CLI is a thin client of that endpoint: it
// holds no render or publish logic of its own, so the API and CLI lanes
// cannot drift.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/engine"
)

// defaultAPIBaseURL is where `nodes run` looks for the control plane when
// neither --api nor NODES_API_URL says otherwise — the same address a
// default `nodes serve` listens on.
const defaultAPIBaseURL = "http://127.0.0.1:8080"

// runWatchPollInterval is how often --watch re-reads the run. State
// changes on an agent run are minutes apart, so a courteous 2s poll is
// plenty.
const runWatchPollInterval = 2 * time.Second

// runRequestTimeout bounds each individual HTTP call (create, and each
// watch poll) — never the overall watch, which legitimately runs as long
// as the run does.
const runRequestTimeout = 30 * time.Second

// adhocRunRequest mirrors components.schemas.AdhocRunRequest
// (api/openapi/openapi.yaml). Optional fields are omitted when empty so
// the server applies its own documented defaults.
type adhocRunRequest struct {
	Instruction    string `json:"instruction"`
	ActorRef       string `json:"actor_ref"`
	Repo           string `json:"repo"`
	Sandbox        string `json:"sandbox,omitempty"`
	SuccessOutcome string `json:"success_outcome,omitempty"`
	Timeout        string `json:"timeout,omitempty"`
}

// adhocRunResponse is the slice of components.schemas.Run this verb reads.
type adhocRunResponse struct {
	ID             string `json:"id"`
	WorkflowDigest string `json:"workflow_digest"`
	State          string `json:"state"`
}

// runResultPayload is `nodes run --json`'s stable result shape.
type runResultPayload struct {
	RunID          string `json:"run_id"`
	WorkflowDigest string `json:"workflow_digest"`
	State          string `json:"state"`
}

// cmdRun implements `nodes run`.
//
// Stream and exit contract (matching `nodes validate`'s domain-outcome
// rule): the created run's id/digest/state are a result on stdout; --watch
// progress notes are diagnostics on stderr; exit 0 when the run was
// created (and, with --watch, completed), exit 1 when --watch saw the run
// end failed or cancelled — a domain outcome, so the final state still
// prints to stdout.
func cmdRun(args []string, jsonMode bool) (int, error) {
	fs := newFlagSet("run")
	apiFlag := fs.String("api", "", "control-plane base URL (defaults to NODES_API_URL, then "+defaultAPIBaseURL+")")
	instruction := fs.String("instruction", "", "what the actor is being asked to do (required)")
	actor := fs.String("actor", "", "agent actor component reference, e.g. actor://company/codex-thor@sha256:... (required)")
	repo := fs.String("repo", "", "checkout path the actor works in (required)")
	sandbox := fs.String("sandbox", "", "sandbox mode for the session (server default: read-only)")
	outcome := fs.String("outcome", "", "declared success outcome name (server default: completed)")
	timeout := fs.String("timeout", "", "task attempt timeout, Go-style duration (server default: 15m)")
	tokenFlag := fs.String("token", "", "bearer token for the ad-hoc lane (defaults to NODES_ADHOC_RUN_TOKEN)")
	watch := fs.Bool("watch", false, "poll the run until it reaches a terminal state")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			clifmt.EmitResult(explainRun)
			return clifmt.ExitSuccess, nil
		}
		return 0, parseError("run", err)
	}
	if rest := fs.Args(); len(rest) != 0 {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     fmt.Sprintf("run takes no positional arguments, got %q", rest),
			Remediation: "pass the task as --instruction \"...\" — run 'nodes run --help' for the full flag list",
		}
	}

	var missing []string
	if *instruction == "" {
		missing = append(missing, "--instruction")
	}
	if *actor == "" {
		missing = append(missing, "--actor")
	}
	if *repo == "" {
		missing = append(missing, "--repo")
	}
	if len(missing) > 0 {
		return 0, &clifmt.CliError{
			Code:        clifmt.ExitUserError,
			Message:     "missing required flags: " + strings.Join(missing, ", "),
			Remediation: "run 'nodes run --help' for the full flag list",
		}
	}

	baseURL := *apiFlag
	if baseURL == "" {
		baseURL = os.Getenv("NODES_API_URL")
	}
	if baseURL == "" {
		baseURL = defaultAPIBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	token := *tokenFlag
	if token == "" {
		token = os.Getenv("NODES_ADHOC_RUN_TOKEN")
	}

	client := &http.Client{Timeout: runRequestTimeout}
	created, cliErr := createAdhocRun(client, baseURL, token, adhocRunRequest{
		Instruction:    *instruction,
		ActorRef:       *actor,
		Repo:           *repo,
		Sandbox:        *sandbox,
		SuccessOutcome: *outcome,
		Timeout:        *timeout,
	})
	if cliErr != nil {
		return 0, cliErr
	}

	state := created.State
	if *watch {
		clifmt.EmitDiagnostic(fmt.Sprintf("nodes run: created run %s (digest %s), watching", created.ID, created.WorkflowDigest))
		finalState, watchErr := watchRun(client, baseURL, created.ID, state)
		if watchErr != nil {
			return 0, watchErr
		}
		state = finalState
	}

	if err := emitRunResult(jsonMode, created, state); err != nil {
		return 0, err
	}
	if *watch && state != string(engine.RunCompleted) {
		// failed / cancelled: a domain outcome carried in the exit code,
		// never a CliError — the result already went to stdout.
		return clifmt.ExitUserError, nil
	}
	return clifmt.ExitSuccess, nil
}

// createAdhocRun POSTs the request and decodes either the created run or
// the documented {code, message, remediation} error shape, which maps
// directly onto CliError (the API's code buckets are the CLI's).
//
// The route is bearer-gated (NODES_ADHOC_RUN_TOKEN_SECRET server-side, the
// t15 auth-hardening gate): the CLI presents the token from
// NODES_ADHOC_RUN_TOKEN or --token. An absent token still sends the
// request — the server's 401 with its remediation line is the honest
// answer, not a client-side guess.
func createAdhocRun(client *http.Client, baseURL, token string, req adhocRunRequest) (adhocRunResponse, *clifmt.CliError) {
	body, err := json.Marshal(req)
	if err != nil {
		return adhocRunResponse{}, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("encode request: %v", err),
			Remediation: fmt.Sprintf("this is a CLI fault; file a bug at %s", clifmt.IssuesURL),
		}
	}
	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/v1alpha1/adhoc-runs", bytes.NewReader(body))
	if err != nil {
		return adhocRunResponse{}, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("build request: %v", err),
			Remediation: fmt.Sprintf("this is a CLI fault; file a bug at %s", clifmt.IssuesURL),
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return adhocRunResponse{}, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("cannot reach the control plane at %s: %v", baseURL, err),
			Remediation: "check that 'nodes serve' is running there, or point --api / NODES_API_URL at it",
		}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return adhocRunResponse{}, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("read response from %s: %v", baseURL, err),
			Remediation: "check the connection to the control plane and try again",
		}
	}
	if resp.StatusCode != http.StatusCreated {
		return adhocRunResponse{}, apiErrorToCliError(resp.StatusCode, data)
	}
	var created adhocRunResponse
	if err := json.Unmarshal(data, &created); err != nil {
		return adhocRunResponse{}, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("decode created run: %v", err),
			Remediation: "the control plane answered 201 with an unexpected body; check that its version matches this CLI",
		}
	}
	return created, nil
}

// watchRun polls the run until it reaches a terminal state and returns
// that state, emitting a diagnostic on every transition it observes.
func watchRun(client *http.Client, baseURL, runID, lastState string) (string, *clifmt.CliError) {
	for {
		resp, err := client.Get(baseURL + "/v1alpha1/runs/" + runID)
		if err != nil {
			return "", &clifmt.CliError{
				Code:        clifmt.ExitEnvError,
				Message:     fmt.Sprintf("poll run %s: %v", runID, err),
				Remediation: "check the connection to the control plane; the run itself keeps going server-side",
			}
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return "", &clifmt.CliError{
				Code:        clifmt.ExitEnvError,
				Message:     fmt.Sprintf("poll run %s: read response: %v", runID, readErr),
				Remediation: "check the connection to the control plane; the run itself keeps going server-side",
			}
		}
		if resp.StatusCode != http.StatusOK {
			return "", apiErrorToCliError(resp.StatusCode, data)
		}
		var view struct {
			Run adhocRunResponse `json:"run"`
		}
		if err := json.Unmarshal(data, &view); err != nil {
			return "", &clifmt.CliError{
				Code:        clifmt.ExitEnvError,
				Message:     fmt.Sprintf("poll run %s: decode response: %v", runID, err),
				Remediation: "the control plane answered 200 with an unexpected body; check that its version matches this CLI",
			}
		}
		if view.Run.State != lastState {
			lastState = view.Run.State
			clifmt.EmitDiagnostic(fmt.Sprintf("nodes run: run %s is %s", runID, lastState))
		}
		if engine.RunState(view.Run.State).Terminal() {
			return view.Run.State, nil
		}
		time.Sleep(runWatchPollInterval)
	}
}

// apiErrorToCliError maps the API's documented {code, message, remediation}
// error body onto a CliError, falling back to the raw body when the shape
// does not decode (e.g. a proxy answered instead of the control plane).
func apiErrorToCliError(status int, body []byte) *clifmt.CliError {
	var apiErr struct {
		Code        int    `json:"code"`
		Message     string `json:"message"`
		Remediation string `json:"remediation"`
	}
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Message != "" {
		code := clifmt.ExitUserError
		if apiErr.Code == clifmt.ExitEnvError {
			code = clifmt.ExitEnvError
		}
		remediation := apiErr.Remediation
		if remediation == "" {
			remediation = "see the control plane's response above"
		}
		return &clifmt.CliError{Code: code, Message: apiErr.Message, Remediation: remediation}
	}
	return &clifmt.CliError{
		Code:        clifmt.ExitEnvError,
		Message:     fmt.Sprintf("control plane answered %d: %s", status, strings.TrimSpace(string(body))),
		Remediation: "check that --api / NODES_API_URL points at a Culture Nodes control plane",
	}
}

// emitRunResult prints the verb's result: the run id, its pinned digest,
// and its (final, when watched) state.
func emitRunResult(jsonMode bool, created adhocRunResponse, state string) error {
	if jsonMode {
		return clifmt.EmitResultJSON(runResultPayload{
			RunID:          created.ID,
			WorkflowDigest: created.WorkflowDigest,
			State:          state,
		})
	}
	clifmt.EmitResult(fmt.Sprintf("run: %s\ndigest: %s\nstate: %s", created.ID, created.WorkflowDigest, state))
	return nil
}

// explainRun is the `nodes explain run` entry and the `nodes run --help`
// text. It lives here rather than in explain.go so the verb's
// implementation and its documentation move together (the same pattern
// validate.go uses).
const explainRun = `# nodes run

The first-class ad-hoc run lane (issue #36): one invocation takes an
instruction and an agent actor and yields a normal, pinned-digest run —
no hand-authored workflow needed. The CLI calls POST /v1alpha1/adhoc-runs,
which renders the canonical one-node workflow (a task agent node into a
finish end node), publishes it idempotently by digest (identical
parameters re-render to the same digest), and creates the run through the
ordinary engine path. The instruction rides as run input, never workflow
content, so one published digest serves every assignment to the same
actor with the same timeout and outcome.

## Usage

    nodes run --instruction "review the CHANGELOG" \
        --actor actor://company/codex-thor@sha256:... \
        --repo /home/thor/git/culture-nodes-agent
    nodes run --instruction ... --actor ... --repo ... --watch
    nodes run --instruction ... --actor ... --repo ... --json

## Flags

- ` + "`--instruction`" + ` (required) — what the actor is being asked to do.
- ` + "`--actor`" + ` (required) — agent actor component reference.
- ` + "`--repo`" + ` (required) — checkout path the actor works in.
- ` + "`--sandbox`" + ` — sandbox mode (server default: read-only).
- ` + "`--outcome`" + ` — declared success outcome (server default: completed).
- ` + "`--timeout`" + ` — task attempt timeout (server default: 15m).
- ` + "`--watch`" + ` — poll until the run reaches a terminal state.
- ` + "`--api`" + ` — control-plane base URL (default: NODES_API_URL, then
  ` + defaultAPIBaseURL + `).

## Output

Text mode prints ` + "`run:`" + `, ` + "`digest:`" + `, and ` + "`state:`" + ` lines;
--json prints ` + "`{run_id, workflow_digest, state}`" + `. With --watch,
progress notes go to stderr and the final state is the stdout result.

## Exit codes

- ` + "`0`" + ` the run was created (and, with --watch, completed)
- ` + "`1`" + ` invalid invocation, a refused request, or --watch saw the run
  end failed/cancelled (a domain outcome, reported on stdout)
- ` + "`2`" + ` the control plane could not be reached
`
