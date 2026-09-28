package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Sensitivity approval inbox (task t30, #328; spec q22; reworked by t30b,
// owner decision d4): a firing that would render a variable into a wider
// audience than it came from is blocked; when the declaration lists that
// variable in `exposes`, the block opens one approval task per (declaration
// name, entry, owner), addressed to the variable's owner
// (internal/declengine/sensitivity.go). These two routes are that owner's
// inbox: list the tasks, and append an approve/refuse decision. The decider
// is always the authenticated principal, resolved by declarationPrincipal's
// allow-list; declengine.DecideSensitivityApproval refuses an agent and any
// human who is not the owner.
//
// The repository-visibility routes (t30b, d4) record whether a GitHub
// repository is public or private, which decides GitHub's audience rank. An
// operator sets it, never an agent bearer (see principal.go); the record is
// append-only and names who set it.

// repositoryVisibilityPath is the one route for listing and setting
// repository visibility; principal.go and actorbearer.go name it too.
const repositoryVisibilityPath = "/v1alpha1/repository-visibility"

type sensitivityApprovalList struct {
	Items []declengine.SensitivityApproval `json:"items"`
}

type sensitivityDecisionRequest struct {
	Decision string `json:"decision"`
	Note     string `json:"note,omitempty"`
}

// handleListSensitivityApprovals is GET /v1alpha1/sensitivity-approvals
// ?owner=&status=.
func (s *Server) handleListSensitivityApprovals(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	status := q.Get("status")
	switch status {
	case "", declengine.SensitivityPending, declengine.SensitivityApproved, declengine.SensitivityRefused, declengine.SensitivityWithdrawn:
	default:
		return badRequest("use status=pending, approved, refused or withdrawn", "unknown status %q", status)
	}
	items, err := declengine.ListSensitivityApprovals(r.Context(), s.Store, s.NamespaceID, q.Get("owner"), status)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, sensitivityApprovalList{Items: items})
	return nil
}

// handleDecideSensitivityApproval is POST
// /v1alpha1/sensitivity-approvals/{id}/decision.
func (s *Server) handleDecideSensitivityApproval(w http.ResponseWriter, r *http.Request) error {
	principal, apiErr := declarationPrincipal(r)
	if apiErr != nil {
		return apiErr
	}
	var req sensitivityDecisionRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && err != io.EOF {
		return badRequest(`send {"decision": "approved"|"refused", "note"?}`, "decode request body: %v", err)
	}
	if req.Decision != declengine.SensitivityApproved && req.Decision != declengine.SensitivityRefused {
		return badRequest(`send {"decision": "approved"|"refused", "note"?}`, "decision must be approved or refused, got %q", req.Decision)
	}
	d, err := declengine.DecideSensitivityApproval(r.Context(), s.Store, s.NamespaceID, r.PathValue("id"), principal, req.Decision, req.Note)
	switch {
	case err == nil:
	case errors.Is(err, postgres.ErrNotFound):
		return notFound("list GET /v1alpha1/sensitivity-approvals for task ids", "%v", err)
	case errors.Is(err, declengine.ErrSensitivityNotHuman), errors.Is(err, declengine.ErrSensitivityNotOwner):
		return forbidden("only the variable's owner, authenticated as a person, may decide this task", err.Error())
	default:
		return internalError(err)
	}
	writeJSON(w, http.StatusCreated, d)
	return nil
}

type repositoryVisibilityList struct {
	Items []declengine.RepositoryVisibilityRecord `json:"items"`
}

type repositoryVisibilityRequest struct {
	Repository string `json:"repository"`
	Visibility string `json:"visibility"`
	Note       string `json:"note,omitempty"`
}

// handleListRepositoryVisibility is GET /v1alpha1/repository-visibility: each
// recorded repository's current visibility. A repository absent from the
// list is unknown: public as an action's target, org as a source.
func (s *Server) handleListRepositoryVisibility(w http.ResponseWriter, r *http.Request) error {
	items, err := declengine.ListRepositoryVisibility(r.Context(), s.Store, s.NamespaceID)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, repositoryVisibilityList{Items: items})
	return nil
}

// handleSetRepositoryVisibility is POST /v1alpha1/repository-visibility.
func (s *Server) handleSetRepositoryVisibility(w http.ResponseWriter, r *http.Request) error {
	principal, apiErr := declarationPrincipal(r)
	if apiErr != nil {
		return apiErr
	}
	const hint = `send {"repository": "owner/name", "visibility": "public"|"private", "note"?}`
	var req repositoryVisibilityRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && err != io.EOF {
		return badRequest(hint, "decode request body: %v", err)
	}
	rec, err := declengine.SetRepositoryVisibility(r.Context(), s.Store, s.NamespaceID, req.Repository, req.Visibility, declengine.ResolveAuthor(principal), req.Note)
	switch {
	case err == nil:
	case errors.Is(err, declengine.ErrRepositoryVisibilityInvalid):
		return badRequest(hint, "%v", err)
	default:
		return internalError(err)
	}
	writeJSON(w, http.StatusCreated, rec)
	return nil
}
