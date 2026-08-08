package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// workflowSourceRequest is components.schemas.WorkflowSource.
type workflowSourceRequest struct {
	Format string `json:"format"`
	Source string `json:"source"`
}

// workflowValidationOut is components.schemas.WorkflowValidation.
// compiler.Diagnostic already carries the documented json tags, so it is
// used directly rather than re-declared here.
type workflowValidationOut struct {
	Valid       bool                  `json:"valid"`
	Digest      string                `json:"digest"`
	Diagnostics []compiler.Diagnostic `json:"diagnostics"`
}

// decodeWorkflowSource reads and validates a WorkflowSource request body,
// defaulting format to yaml (PRD §7: YAML is authoring sugar over JSON).
func decodeWorkflowSource(r *http.Request) (workflowSourceRequest, *apiError) {
	var req workflowSourceRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return workflowSourceRequest{}, badRequest("send a JSON body matching WorkflowSource: {format, source}", "decode request body: %v", err)
	}
	if req.Source == "" {
		return workflowSourceRequest{}, badRequest("source must not be empty", "workflow source is required")
	}
	if req.Format == "" {
		req.Format = string(compiler.FormatYAML)
	}
	if req.Format != string(compiler.FormatYAML) && req.Format != string(compiler.FormatJSON) {
		return workflowSourceRequest{}, badRequest(
			fmt.Sprintf("format must be %q or %q", compiler.FormatYAML, compiler.FormatJSON),
			"unknown format %q", req.Format)
	}
	return req, nil
}

// handleValidateWorkflow is POST /v1alpha1/workflows/validate: compiles the
// submitted source and reports every diagnostic. A document with error
// diagnostics is a documented domain outcome (valid: false), not an HTTP
// error — see the package doc's discussion of PRD §3.4.
func (s *Server) handleValidateWorkflow(w http.ResponseWriter, r *http.Request) error {
	req, apiErr := decodeWorkflowSource(r)
	if apiErr != nil {
		return apiErr
	}

	compiled, diagnostics, err := compiler.Compile([]byte(req.Source), compiler.Format(req.Format))
	if err != nil {
		return internalError(fmt.Errorf("compiler: %w", err))
	}
	if diagnostics == nil {
		diagnostics = []compiler.Diagnostic{}
	}

	out := workflowValidationOut{Valid: compiled != nil, Diagnostics: diagnostics}
	if compiled != nil {
		out.Digest = compiled.Digest
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// handlePublishWorkflow is POST /v1alpha1/workflows: compiles the submitted
// source and, if it compiles, stores it as an immutable version addressed
// by its content digest. Publishing the same content twice returns the
// existing version with 200 rather than erroring (PRD §11.3: an identical
// definition always resolves to the same immutable version).
func (s *Server) handlePublishWorkflow(w http.ResponseWriter, r *http.Request) error {
	req, apiErr := decodeWorkflowSource(r)
	if apiErr != nil {
		return apiErr
	}
	ctx := r.Context()

	compiled, diagnostics, err := compiler.Compile([]byte(req.Source), compiler.Format(req.Format))
	if err != nil {
		return internalError(fmt.Errorf("compiler: %w", err))
	}
	if compiled == nil {
		errCount, _ := compiler.CountByLevel(diagnostics)
		return unprocessable(
			"call POST /v1alpha1/workflows/validate for the full diagnostic list",
			"workflow does not compile: %d error diagnostic(s)", errCount)
	}

	// Idempotent-by-digest: a pre-check keeps a repeat publish a 200, not a
	// 201 — EnsureWorkflowVersion below is itself idempotent and would
	// return the same version either way, but only this check lets the
	// response say which happened. A publish racing another publish of the
	// identical content may still report 201 here even though
	// EnsureWorkflowVersion's own advisory lock made only one of them the
	// true creator; both responses describe the same, now-durable version,
	// so that race is benign.
	if existing, err := s.workflowVersionByDigest(ctx, compiled.Digest); err == nil {
		writeJSON(w, http.StatusOK, workflowVersionOut(existing))
		return nil
	} else if !errors.Is(err, postgres.ErrNotFound) {
		return internalError(err)
	}

	versionID, err := s.engineStore.EnsureWorkflowVersion(ctx, engine.WorkflowVersionInput{
		WorkflowKey:   compiled.Name,
		SourceFormat:  string(compiled.Format),
		Source:        string(compiled.Source),
		NormalizedIR:  compiled.Normalized,
		ContentDigest: compiled.Digest,
	})
	if err != nil {
		return internalError(fmt.Errorf("publish workflow: %w", err))
	}

	published, err := s.Store.GetWorkflowVersion(ctx, versionID)
	if err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusCreated, workflowVersionOut(published))
	return nil
}

// handleListWorkflows is GET /v1alpha1/workflows.
func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) error {
	limit := parseLimit(r, 50, 500)
	versions, err := s.listWorkflowVersions(r.Context(), r.URL.Query().Get("workflow_key"), limit)
	if err != nil {
		return internalError(err)
	}
	out := make([]WorkflowVersionOut, len(versions))
	for i, v := range versions {
		out[i] = workflowVersionOut(v)
	}
	writeJSON(w, http.StatusOK, WorkflowVersionListOut{Items: out})
	return nil
}

// handleGetWorkflow is GET /v1alpha1/workflows/{digest}.
func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) error {
	digest := r.PathValue("digest")
	v, err := s.workflowVersionByDigest(r.Context(), digest)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return notFound("check the digest, or POST /v1alpha1/workflows to publish it", "no workflow version with digest %s", digest)
		}
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, workflowVersionOut(v))
	return nil
}
