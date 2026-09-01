package api

// Ad-hoc runs (task t19, issue #36): POST /v1alpha1/adhoc-runs turns a bare
// instruction into a normal, pinned-digest run in one call. The handler
// renders the canonical one-node workflow (mirroring the nodes-operator
// skill's templates/assign.workflow.yaml — a single agent `task` node into a
// `finish` end node, with instruction/repo/sandbox/success_outcome arriving
// as run input bindings), compiles it, and hands it to Engine.CreateRun —
// which publishes the version through the same idempotent
// EnsureWorkflowVersion path POST /v1alpha1/workflows uses (PRD §11.3: an
// identical definition always resolves to the same immutable version) and
// creates the run pinned to that digest in the same transaction. There is
// no publish-semantics bypass anywhere: the run is indistinguishable from
// one created against a hand-published digest, because it *is* one.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// adhocRunRequest is components.schemas.AdhocRunRequest. Instruction,
// ActorRef, and Repo are required; the rest default to the same values the
// nodes-operator skill's `assign` verb uses, so the API lane and the skill
// lane render the same canonical workflow for the same actor.
type adhocRunRequest struct {
	Instruction    string `json:"instruction"`
	ActorRef       string `json:"actor_ref"`
	Repo           string `json:"repo"`
	Sandbox        string `json:"sandbox,omitempty"`
	SuccessOutcome string `json:"success_outcome,omitempty"`
	Timeout        string `json:"timeout,omitempty"`
}

// The defaults mirror the nodes-operator skill's assign verb
// (sandbox=read-only, timeout=15m, outcome=completed,
// retries=1) — an ad-hoc assignment is a billable agent session, so a
// silent retry doubles real spend; maxAttempts stays fixed at 1 here.
const (
	adhocDefaultSandbox = "read-only"
	adhocDefaultOutcome = "completed"
	adhocDefaultTimeout = "15m"
)

// The request-field patterns are the workflow schema's own $defs
// (schemas/workflow/workflow.schema.json: componentRef, outcomeName,
// duration), checked up front so a malformed value is a 400 naming the
// offending field rather than a compile diagnostic about a rendered
// document the caller never wrote.
var (
	adhocActorRefPattern = regexp.MustCompile(`^[a-z][a-z0-9+.-]*://[^\s]+$`)
	adhocOutcomePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	adhocTimeoutPattern  = regexp.MustCompile(`^([0-9]+(ms|s|m|h))+$`)

	// adhocNameSanitize collapses anything the workflow schema's name
	// pattern (^[a-z0-9][a-z0-9-]*$) refuses into a hyphen.
	adhocNameSanitize = regexp.MustCompile(`[^a-z0-9-]+`)
)

// adhocWorkflowTemplate is the canonical one-node workflow, mirroring the
// shape of the nodes-operator skill's assign workflow template (which is
// vendored and must not be edited — this is a deliberate copy of its
// structure, not a citation of its bytes). The instruction, repo,
// sandbox, and success outcome all arrive as run input, so ONE published
// digest serves every ad-hoc run to the same actor with the same timeout
// and outcome: an identical render republishes as a no-op returning the
// same digest.
const adhocWorkflowTemplate = `apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow

metadata:
  name: __NAME__
  version: 1.0.0
  ownerRef: team/platform-ai

spec:
  entry: task

  contract:
    input:
      schema:
        type: object
        required: [instruction, sandbox, success_outcome, repo]
        properties:
          instruction:
            type: string
          sandbox:
            type: string
          success_outcome:
            type: string
          repo:
            type: string
    output:
      schema:
        type: object

  limits:
    maxDuration: 2h
    maxTransitions: 6
    maxVisitsPerNode: 2
    maxParallelTokens: 1

  ledger:
    schemaVersion: nodes.culture.dev/ledger/v1alpha1
    maxRecordsPerNode: 10

  nodes:
    task:
      kind: agent
      ownerRef: team/platform-ai
      uses: __ACTOR_REF__
      input:
        bindings:
          instruction: /run/input/instruction
          repo: /run/input/repo
          sandbox: /run/input/sandbox
          success_outcome: /run/input/success_outcome
      contract:
        input:
          schema:
            type: object
            required: [instruction, repo]
        outcomes:
          __OUTCOME__:
            schema:
              type: object
              required: [summary]
              properties:
                summary:
                  type: string
      ledger:
        propose: [claim]
      policy:
        timeout: __TIMEOUT__
        retry:
          maxAttempts: 1
          backoff: none

    finish:
      kind: end
      ownerRef: team/platform-ai
      output:
        from: /nodes/task/output

  edges:
    - from: task.__OUTCOME__
      to: finish
`

// renderAdhocWorkflow substitutes the render parameters into the canonical
// template — the same placeholder keys the skill's sed render uses, so the
// two lanes stay reviewable side by side.
func renderAdhocWorkflow(name, actorRef, timeout, outcome string) string {
	return strings.NewReplacer(
		"__NAME__", name,
		"__ACTOR_REF__", actorRef,
		"__TIMEOUT__", timeout,
		"__OUTCOME__", outcome,
	).Replace(adhocWorkflowTemplate)
}

// adhocWorkflowName derives the rendered workflow's metadata.name from the
// actor reference — `adhoc-<actor key>` (e.g. adhoc-planner for
// actor://company/planner@sha256:...), mirroring the skill's
// assign-<actor> naming. The key is sanitized to the schema's name pattern;
// a ref whose key sanitizes away entirely still gets a valid name.
func adhocWorkflowName(actorRef string) string {
	key := actorRef
	if i := strings.Index(key, "://"); i >= 0 {
		key = key[i+len("://"):]
	}
	if i := strings.IndexByte(key, '@'); i >= 0 {
		key = key[:i]
	}
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		key = key[i+1:]
	}
	key = adhocNameSanitize.ReplaceAllString(strings.ToLower(key), "-")
	key = strings.Trim(key, "-")
	if key == "" {
		key = "actor"
	}
	return "adhoc-" + key
}

// requireAdhocRunAuth is requireEventAuth's pattern applied to the ad-hoc
// lane: its own secret (Server.adhocRunSecret, NODES_ADHOC_RUN_TOKEN_SECRET),
// constant-time digest comparison, closed by default. An ad-hoc run renders,
// publishes, and starts real (often billable) work in one call — the t15
// auth-hardening gate (spec c27) requires every mutating surface this batch
// added to refuse unauthenticated requests.
func (s *Server) requireAdhocRunAuth(r *http.Request) error {
	if _, ok := PrincipalFromContext(r.Context()); ok {
		return nil
	}
	if len(s.adhocRunSecret) == 0 {
		return unauthorized(
			"configure the server with an ad-hoc run secret (NODES_ADHOC_RUN_TOKEN_SECRET) to enable ad-hoc runs",
			"ad-hoc runs require a configured bearer secret and none is configured")
	}

	const prefix = "bearer "
	header := r.Header.Get("Authorization")
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return unauthorized("send Authorization: Bearer <token>", "missing or malformed Authorization header")
	}

	presented := sha256.Sum256([]byte(header[len(prefix):]))
	expected := sha256.Sum256(s.adhocRunSecret)
	if subtle.ConstantTimeCompare(presented[:], expected[:]) != 1 {
		return unauthorized("the bearer token is not valid for this deployment", "authorization failed")
	}
	return nil
}

// handleCreateAdhocRun is POST /v1alpha1/adhoc-runs.
func (s *Server) handleCreateAdhocRun(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireAdhocRunAuth(r); err != nil {
		return err
	}

	var req adhocRunRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return badRequest("send a JSON body matching AdhocRunRequest: {instruction, actor_ref, repo, sandbox?, success_outcome?, timeout?}", "decode request body: %v", err)
	}

	if strings.TrimSpace(req.Instruction) == "" {
		return badRequest("instruction is required", "instruction must not be empty")
	}
	if req.ActorRef == "" {
		return badRequest("actor_ref is required, e.g. actor://company/planner@sha256:...", "actor_ref must not be empty")
	}
	if !adhocActorRefPattern.MatchString(req.ActorRef) {
		return badRequest("actor_ref must be a component reference like actor://company/planner@sha256:...", "malformed actor_ref %q", req.ActorRef)
	}
	if req.Repo == "" {
		return badRequest("repo is required: the checkout path the actor works in", "repo must not be empty")
	}
	if req.Sandbox == "" {
		req.Sandbox = adhocDefaultSandbox
	}
	if req.SuccessOutcome == "" {
		req.SuccessOutcome = adhocDefaultOutcome
	}
	if !adhocOutcomePattern.MatchString(req.SuccessOutcome) {
		return badRequest("success_outcome must match the workflow schema's outcomeName pattern ^[a-z][a-z0-9_]*$", "malformed success_outcome %q", req.SuccessOutcome)
	}
	if req.Timeout == "" {
		req.Timeout = adhocDefaultTimeout
	}
	if !adhocTimeoutPattern.MatchString(req.Timeout) {
		return badRequest("timeout must be a Go-style duration literal like 15m, 900s, or 1h30m", "malformed timeout %q", req.Timeout)
	}

	source := renderAdhocWorkflow(adhocWorkflowName(req.ActorRef), req.ActorRef, req.Timeout, req.SuccessOutcome)
	compiled, diagnostics, err := compiler.Compile([]byte(source), compiler.FormatYAML)
	if err != nil {
		return internalError(fmt.Errorf("compiler: %w", err))
	}
	if compiled == nil {
		// Every templated value was validated above, so a non-compiling
		// render points at a template/compiler drift, not at the caller —
		// but report it honestly with the diagnostic count either way.
		errCount, _ := compiler.CountByLevel(diagnostics)
		return unprocessable(
			"the rendered canonical workflow does not compile — this is a server-side template fault; file a bug",
			"rendered ad-hoc workflow does not compile: %d error diagnostic(s)", errCount)
	}

	input, err := json.Marshal(map[string]string{
		"instruction":     req.Instruction,
		"repo":            req.Repo,
		"sandbox":         req.Sandbox,
		"success_outcome": req.SuccessOutcome,
	})
	if err != nil {
		return internalError(fmt.Errorf("marshal run input: %w", err))
	}

	// Engine.CreateRun publishes the version via the same idempotent
	// EnsureWorkflowVersion path handlePublishWorkflow uses and creates the
	// run pinned to its digest, all in one transaction — a NORMAL run.
	run, err := s.Engine.CreateRun(r.Context(), compiled, input)
	if err != nil {
		return classify(err)
	}
	// Zero attempts by construction — same deterministic empty rollup
	// handleCreateRun returns for a just-created run.
	writeJSON(w, http.StatusCreated, runOut(run, postgres.UsageRollup{}, runMetadata{}))
	return nil
}
