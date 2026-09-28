package api

// Workflow generation is deliberately implemented as a normal workflow run.
// The control plane renders and compiles the fixed orchestration graph below;
// the registered fleet actor named by the caller is the only component that
// turns prose into workflow source.  In particular, this package imports no
// model SDK and holds no model credential.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// Output selects what the actor drafts (task t35, spec c86): "workflow"
	// (the default, today's behaviour) or "declarations".
	Output string `json:"output,omitempty"`
}

// generationOutput is the form a generation lane drafts. The workflow form
// stays the default until t37 retires the graph engine; the declaration
// form is added beside it, never in place of it.
type generationOutput string

const (
	generationOutputWorkflow     generationOutput = "workflow"
	generationOutputDeclarations generationOutput = "declarations"
)

func parseGenerationOutput(raw string) (generationOutput, bool) {
	switch raw {
	case "", string(generationOutputWorkflow):
		return generationOutputWorkflow, true
	case string(generationOutputDeclarations):
		return generationOutputDeclarations, true
	}
	return "", false
}

// declarationGenerationOutput is what the actor returns in declaration
// mode: a declaration set in one format plus the links between members.
type declarationGenerationOutput struct {
	Format       string               `json:"format"`
	Declarations []declarationSetItem `json:"declarations"`
	Links        []declarationSetLink `json:"links"`
}

type workflowGenerationOutput struct {
	Format string `json:"format"`
	Source string `json:"source"`
}

type workflowGenerationOut struct {
	RunID       string                `json:"run_id"`
	Status      string                `json:"status"`
	Output      generationOutput      `json:"output"`
	BaseDigest  string                `json:"base_digest,omitempty"`
	Format      string                `json:"format,omitempty"`
	Source      string                `json:"source,omitempty"`
	Diff        string                `json:"diff,omitempty"`
	Valid       bool                  `json:"valid"`
	Digest      string                `json:"digest,omitempty"`
	Diagnostics []compiler.Diagnostic `json:"diagnostics"`
	// Declarations is the drafted declaration set (output=declarations
	// only), validated -- or, from the publish route, published.
	Declarations *declarationSetOut `json:"declarations,omitempty"`
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

// renderGeneration renders the orchestration for the selected output. The
// workflow form is byte-identical to renderWorkflowGeneration; the
// declaration form differs only in the outcome it demands from the actor
// and in the orchestration's name.
func renderGeneration(actorRef string, output generationOutput) string {
	source := renderWorkflowGeneration(actorRef)
	if output != generationOutputDeclarations {
		return source
	}
	return strings.NewReplacer(
		"name: workflow-generator-", "name: declaration-generator-",
		workflowOutcomeSchema, declarationOutcomeSchema,
	).Replace(source)
}

const workflowOutcomeSchema = `              required: [format, source]
              properties:
                format: {type: string, enum: [yaml, json]}
                source: {type: string, minLength: 1}
`

const declarationOutcomeSchema = `              required: [format, declarations]
              properties:
                format: {type: string, enum: [yaml, json]}
                declarations:
                  type: array
                  minItems: 1
                  items:
                    type: object
                    required: [source]
                    properties:
                      source: {type: string, minLength: 1}
                links:
                  type: array
                  items:
                    type: object
                    required: [from, to, kind]
                    properties:
                      from: {type: string, minLength: 1}
                      to: {type: string, minLength: 1}
                      kind: {type: string, enum: [must, can]}
`

// declarationGenerationInstruction is generationInstruction's declaration
// counterpart. With a base digest, the actor translates (or edits) that
// pinned workflow into declarations.
func declarationGenerationInstruction(description, baseDigest, baseSource string) string {
	var b strings.Builder
	b.WriteString("Author Culture Nodes trigger-condition-action declarations (one trigger, one CEL condition, one action, a start node and a landing node each) from the following plain-text description. ")
	b.WriteString("Return outcome generated with JSON output {format, declarations: [{source}], links: [{from, to, kind}]}, where a link kind is must (from fires only after to) or can (from may fire either way); add a link wherever one declaration follows another. ")
	b.WriteString("Before returning, call POST /v1alpha1/declarations/validate with each exact source and continue until every one reports valid=true with no diagnostics. Do not publish. Do not activate: a human activates.\n\nDescription:\n")
	b.WriteString(description)
	if baseDigest != "" {
		fmt.Fprintf(&b, "\n\nExpress the pinned workflow %s as declarations. Preserve intent outside the requested change. Base source:\n%s", baseDigest, baseSource)
	}
	return b.String()
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
		return badRequest("send {description, actor_ref, base_digest?, output?}", "decode request body: %v", err)
	}
	output, ok := parseGenerationOutput(req.Output)
	if !ok {
		return badRequest(`output must be "workflow" (the default) or "declarations"`, "unknown output %q", req.Output)
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
	source := renderGeneration(req.ActorRef, output)
	compiled, diagnostics, err := compiler.Compile([]byte(source), compiler.FormatYAML)
	if err != nil {
		return internalError(fmt.Errorf("compile generation workflow: %w", err))
	}
	if compiled == nil {
		return internalError(fmt.Errorf("generation workflow template does not compile: %+v", diagnostics))
	}
	instruction := generationInstruction(req.Description, req.BaseDigest, baseSource)
	title := "Generate workflow"
	if output == generationOutputDeclarations {
		instruction = declarationGenerationInstruction(req.Description, req.BaseDigest, baseSource)
		title = "Generate declarations"
	}
	input, err := json.Marshal(map[string]string{
		"instruction": instruction,
		"base_digest": req.BaseDigest,
		"output":      string(output),
	})
	if err != nil {
		return internalError(err)
	}
	run, err := s.Engine.CreateRun(r.Context(), compiled, input,
		engine.WithRunMetadata(title, req.Description, "workflow-generation"))
	if err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusAccepted, workflowGenerationOut{
		RunID: run.ID, Status: "proposed", Output: output, BaseDigest: req.BaseDigest,
		Diagnostics: []compiler.Diagnostic{},
	})
	return nil
}

func (s *Server) handleGetWorkflowGeneration(w http.ResponseWriter, r *http.Request) error {
	out, _, err := s.workflowGeneration(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// workflowGeneration reads one generation run's state and validates its
// proposal in the form the run was created for. In declaration mode it also
// returns the drafted set, for the publish route.
func (s *Server) workflowGeneration(ctx context.Context, runID string) (workflowGenerationOut, *declarationGenerationOutput, error) {
	run, err := s.engineStore.Run(ctx, runID)
	if err != nil {
		return workflowGenerationOut{}, nil, classify(err)
	}
	var input struct {
		BaseDigest string `json:"base_digest"`
		Output     string `json:"output"`
	}
	_ = json.Unmarshal(run.Input, &input)
	output, ok := parseGenerationOutput(input.Output)
	if !ok {
		output = generationOutputWorkflow
	}
	out := workflowGenerationOut{RunID: runID, Status: "proposed", Output: output, BaseDigest: input.BaseDigest, Diagnostics: []compiler.Diagnostic{}}
	nodeRuns, err := s.runNodeRuns(ctx, runID)
	if err != nil {
		return out, nil, internalError(err)
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
	raw, err := s.engineStore.NodeOutput(ctx, runID, "generate")
	if err != nil && !errors.Is(err, postgres.ErrNotFound) {
		return out, nil, internalError(err)
	}
	if err != nil || len(raw) == 0 {
		return out, nil, nil
	}
	if output == generationOutputDeclarations {
		var proposal declarationGenerationOutput
		if json.Unmarshal(raw, &proposal) != nil || len(proposal.Declarations) == 0 {
			return out, nil, nil
		}
		for i := range proposal.Declarations {
			if proposal.Declarations[i].Format == "" {
				proposal.Declarations[i].Format = proposal.Format
			}
		}
		set, _, setErr := s.validateDeclarationSet(ctx, proposal.Declarations, proposal.Links)
		if setErr != nil {
			return out, nil, internalError(setErr)
		}
		out.Format, out.Valid, out.Declarations = proposal.Format, set.Valid, &set
		return out, &proposal, nil
	}
	var proposal workflowGenerationOutput
	if json.Unmarshal(raw, &proposal) == nil && proposal.Source != "" {
		out.Format, out.Source = proposal.Format, proposal.Source
		compiled, diags, compileErr := compiler.Compile([]byte(proposal.Source), compiler.Format(proposal.Format))
		if compileErr != nil {
			return out, nil, internalError(compileErr)
		}
		out.Diagnostics = diags
		out.Valid = compiled != nil
		if compiled != nil {
			out.Digest = compiled.Digest
		}
		if input.BaseDigest != "" {
			base, getErr := s.workflowVersionByDigest(ctx, input.BaseDigest)
			if getErr != nil {
				return out, nil, internalError(getErr)
			}
			out.Diff = sourceDiff(input.BaseDigest, base.Source, proposal.Source)
		}
	}
	return out, nil, nil
}

// publishGenerationRequest is components.schemas.PublishWorkflowGeneration.
type publishGenerationRequest struct {
	// Author is decoded and NEVER read (h62): the recorded author is always
	// the authenticated principal.
	Author string `json:"author,omitempty"`
}

// handlePublishWorkflowGeneration is POST
// /v1alpha1/workflow-generations/{id}/publish (task t35): publishes a
// CONFIRMED declaration-output generation's set through declengine.Publish
// under the authenticated principal, with its links. It never activates
// (ADR 0014 §3). Workflow-output generations keep today's path -- the
// confirmed source is published through POST /v1alpha1/workflows -- and are
// refused here.
func (s *Server) handlePublishWorkflowGeneration(w http.ResponseWriter, r *http.Request) error {
	principal, apiErr := declarationPrincipal(r)
	if apiErr != nil {
		return apiErr
	}
	var req publishGenerationRequest
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && err != io.EOF {
			return badRequest("send no body, or an empty JSON object", "decode request body: %v", err)
		}
	}
	ctx := r.Context()
	out, proposal, err := s.workflowGeneration(ctx, r.PathValue("id"))
	if err != nil {
		return err
	}
	if out.Output != generationOutputDeclarations {
		return conflict("publish workflow source through POST /v1alpha1/workflows",
			"generation %s drafted a workflow, not declarations", out.RunID)
	}
	if out.Status != "confirmed" {
		return conflict("a human confirms the proposal on its approval task first",
			"generation %s is %s, not confirmed", out.RunID, out.Status)
	}
	if proposal == nil {
		return conflict("wait for the actor's proposal", "generation %s has no declaration proposal", out.RunID)
	}
	set, err := s.publishDeclarationSet(ctx, principal, proposal.Declarations, proposal.Links)
	if err != nil {
		return err
	}
	out.Declarations, out.Valid = &set, set.Valid
	writeJSON(w, http.StatusCreated, out)
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
