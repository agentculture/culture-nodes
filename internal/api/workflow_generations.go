package api

// Workflow generation is deliberately implemented as a normal workflow run.
// The control plane renders and compiles the fixed orchestration graph below;
// the registered fleet actor named by the caller is the only component that
// turns prose into workflow source.  In particular, this package imports no
// model SDK and holds no model credential.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

type workflowGenerationRequest struct {
	Description string `json:"description"`
	ActorRef    string `json:"actor_ref"`
	BaseDigest  string `json:"base_digest,omitempty"`
}

type workflowGenerationOutput struct {
	Format string `json:"format"`
	Source string `json:"source"`
}

type workflowGenerationOut struct {
	RunID       string                `json:"run_id"`
	Status      string                `json:"status"`
	BaseDigest  string                `json:"base_digest,omitempty"`
	Format      string                `json:"format,omitempty"`
	Source      string                `json:"source,omitempty"`
	Diff        string                `json:"diff,omitempty"`
	Valid       bool                  `json:"valid"`
	Digest      string                `json:"digest,omitempty"`
	Diagnostics []compiler.Diagnostic `json:"diagnostics"`
}

const workflowGenerationTemplate = `apiVersion: nodes.culture.dev/v1alpha1
kind: Workflow

metadata:
  name: workflow-generator-__ACTOR__
  version: 1.0.0
  ownerRef: team/platform-ai

spec:
  entry: generate
  contract:
    input:
      schema:
        type: object
        required: [instruction]
        properties:
          instruction: {type: string}
    output:
      schema: {type: object}
  limits:
    maxDuration: 1h
    maxTransitions: 8
    maxVisitsPerNode: 3
    maxParallelTokens: 1
  ledger:
    schemaVersion: nodes.culture.dev/ledger/v1alpha1
    maxRecordsPerNode: 10
  nodes:
    generate:
      kind: agent
      ownerRef: team/platform-ai
      uses: __ACTOR_REF__
      input:
        from: /run/input
      contract:
        outcomes:
          generated:
            schema:
              type: object
              required: [format, source]
              properties:
                format: {type: string, enum: [yaml, json]}
                source: {type: string, minLength: 1}
      ledger:
        propose: [claim]
      policy:
        timeout: 10m
        retry: {maxAttempts: 1, backoff: none}
      continue:
        while: ['node.state == "incomplete"']
        bounds:
          maxContinuations: 2
          maxWallClock: 30m
          maxSessions: 2
        onExhausted: generation_exhausted
    confirm:
      kind: approval
      ownerRef: team/platform-ai
      approverRef: group/workflow-authors
      deadline: 24h
      input:
        from: /nodes/generate/output
    accepted:
      kind: end
      ownerRef: team/platform-ai
      output:
        from: /nodes/generate/output
    declined:
      kind: end
      ownerRef: team/platform-ai
      output:
        from: /nodes/generate/output
    exhausted:
      kind: end
      ownerRef: team/platform-ai
      output:
        from: /run/input
  edges:
    - from: generate.generated
      to: confirm
    - from: generate.generation_exhausted
      to: exhausted
    - from: confirm.approved
      to: accepted
    - from: confirm.rejected
      to: declined
`

var generationActorName = regexp.MustCompile(`[^a-z0-9-]+`)

func renderWorkflowGeneration(actorRef string) string {
	name := strings.Trim(generationActorName.ReplaceAllString(strings.ToLower(actorRef), "-"), "-")
	if len(name) > 40 {
		name = name[len(name)-40:]
	}
	if name == "" {
		name = "actor"
	}
	return strings.NewReplacer("__ACTOR__", name, "__ACTOR_REF__", actorRef).Replace(workflowGenerationTemplate)
}

func generationInstruction(description, baseDigest, baseSource string) string {
	var b strings.Builder
	b.WriteString("Author a Culture Nodes workflow from the following plain-text description. ")
	b.WriteString("Return outcome generated with JSON output {format, source}. Before returning, call POST /v1alpha1/workflows/validate with the exact source and continue until it reports valid=true with zero error diagnostics. Do not publish.\n\nDescription:\n")
	b.WriteString(description)
	if baseDigest != "" {
		fmt.Fprintf(&b, "\n\nEdit the pinned workflow %s. Preserve intent outside the requested change. Base source:\n%s", baseDigest, baseSource)
	}
	return b.String()
}

func (s *Server) handleCreateWorkflowGeneration(w http.ResponseWriter, r *http.Request) error {
	var req workflowGenerationRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return badRequest("send {description, actor_ref, base_digest?}", "decode request body: %v", err)
	}
	if strings.TrimSpace(req.Description) == "" {
		return badRequest("description is required", "description must not be empty")
	}
	if !adhocActorRefPattern.MatchString(req.ActorRef) {
		return badRequest("actor_ref must be a component reference on the registered fleet", "malformed actor_ref %q", req.ActorRef)
	}
	baseSource := ""
	if req.BaseDigest != "" {
		base, err := s.workflowVersionByDigest(r.Context(), req.BaseDigest)
		if errors.Is(err, postgres.ErrNotFound) {
			return notFound("pin an existing workflow digest", "no workflow version with digest %s", req.BaseDigest)
		}
		if err != nil {
			return internalError(err)
		}
		baseSource = base.Source
	}
	source := renderWorkflowGeneration(req.ActorRef)
	compiled, diagnostics, err := compiler.Compile([]byte(source), compiler.FormatYAML)
	if err != nil {
		return internalError(fmt.Errorf("compile generation workflow: %w", err))
	}
	if compiled == nil {
		return internalError(fmt.Errorf("generation workflow template does not compile: %+v", diagnostics))
	}
	input, err := json.Marshal(map[string]string{
		"instruction": generationInstruction(req.Description, req.BaseDigest, baseSource),
		"base_digest": req.BaseDigest,
	})
	if err != nil {
		return internalError(err)
	}
	run, err := s.Engine.CreateRun(r.Context(), compiled, input,
		engine.WithRunMetadata("Generate workflow", req.Description, "workflow-generation"))
	if err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusAccepted, workflowGenerationOut{
		RunID: run.ID, Status: "proposed", BaseDigest: req.BaseDigest,
		Diagnostics: []compiler.Diagnostic{},
	})
	return nil
}

func (s *Server) handleGetWorkflowGeneration(w http.ResponseWriter, r *http.Request) error {
	runID := r.PathValue("id")
	run, err := s.engineStore.Run(r.Context(), runID)
	if err != nil {
		return classify(err)
	}
	var input struct {
		BaseDigest string `json:"base_digest"`
	}
	_ = json.Unmarshal(run.Input, &input)
	out := workflowGenerationOut{RunID: runID, Status: "proposed", BaseDigest: input.BaseDigest, Diagnostics: []compiler.Diagnostic{}}
	nodeRuns, err := s.runNodeRuns(r.Context(), runID)
	if err != nil {
		return internalError(err)
	}
	for _, nr := range nodeRuns {
		switch {
		case nr.NodeID == "generate" && nr.Outcome == "generation_exhausted":
			out.Status = "exhausted"
		case nr.NodeID == "confirm" && nr.Outcome == "approved":
			out.Status = "confirmed"
		case nr.NodeID == "confirm" && nr.Outcome == "rejected":
			out.Status = "rejected"
		}
	}
	raw, err := s.engineStore.NodeOutput(r.Context(), runID, "generate")
	if err == nil {
		var proposal workflowGenerationOutput
		if json.Unmarshal(raw, &proposal) == nil && proposal.Source != "" {
			out.Format, out.Source = proposal.Format, proposal.Source
			compiled, diags, compileErr := compiler.Compile([]byte(proposal.Source), compiler.Format(proposal.Format))
			if compileErr != nil {
				return internalError(compileErr)
			}
			out.Diagnostics = diags
			out.Valid = compiled != nil
			if compiled != nil {
				out.Digest = compiled.Digest
			}
			if input.BaseDigest != "" {
				base, getErr := s.workflowVersionByDigest(r.Context(), input.BaseDigest)
				if getErr != nil {
					return internalError(getErr)
				}
				out.Diff = sourceDiff(input.BaseDigest, base.Source, proposal.Source)
			}
		}
	} else if !errors.Is(err, postgres.ErrNotFound) {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// sourceDiff is a deterministic line-oriented replacement diff. It keeps the
// shared prefix/suffix and marks only the changed middle, so an edit can never
// be presented as a silent replacement of its pinned base.
func sourceDiff(baseDigest, before, after string) string {
	a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ proposed\n", baseDigest)
	for _, line := range a[prefix : len(a)-suffix] {
		fmt.Fprintf(&out, "-%s\n", line)
	}
	for _, line := range b[prefix : len(b)-suffix] {
		fmt.Fprintf(&out, "+%s\n", line)
	}
	return out.String()
}
