// The declaration API (task t19, #328; spec c27, honesty h21). This is
// where declare/publish, validate, link (must/can), alias (create, move,
// nest), activate, deactivate, show and list become HTTP routes over the
// primitives t6 (internal/decl), t4 (internal/store/postgres/declstore.go)
// and t15 (internal/declengine/activation.go) already built.
//
// SECURITY (closing the trust boundary t15 left open): every route below
// resolves the request principal from the AUTHENTICATED identity the
// server's existing principal machinery already verified
// (principal.go's principalMiddleware -> PrincipalFromContext), classified
// human/agent exactly the way ADR 0014's one-level-deep activation root of
// trust already does (isActorBearer). No handler ever reads an author,
// principal or actor claim out of the request body -- every request
// struct below carries an Author field purely so a test can send a forged
// value and prove it changes nothing (mirrors
// declengine.PublishInput.ClaimedAuthor). Publish and activate go through
// declengine.Publish and declengine.Activate, the two choke points t15
// built for exactly this reason; deactivate goes through the new,
// symmetric declengine.Deactivate. No handler here writes a declaration
// version or an activation_history row any other way.
//
// declarationPrincipal is the one place a route may resolve without a
// legacy bearer-secret fallback: unlike requireDecisionAuth's human
// decision routes, there is no NODES_DECLARATION_TOKEN_SECRET. An
// unauthenticated write is refused outright -- closed by default, the same
// posture TestAdhocRefusedWhenNoSecretConfigured pins for the ad-hoc lane.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/decl/template"
	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// declarationPrincipal resolves the authenticated caller into
// declengine.ActivationPrincipal. An agent actor's own bearer
// (actorbearer.go, isActorBearer) classifies as declengine.PrincipalAgent;
// every other authenticated principal -- Cloudflare Access, the LAN
// break-glass credential, or the transition bearer -- classifies as
// declengine.PrincipalHuman, the same line ADR 0014 draws for the
// activation root of trust. No principal in context is refused outright:
// this route family has no legacy secret fallback.
func declarationPrincipal(r *http.Request) (declengine.ActivationPrincipal, *apiError) {
	p, ok := PrincipalFromContext(r.Context())
	if !ok {
		return declengine.ActivationPrincipal{}, unauthorized(
			"authenticate as a human (Cloudflare Access) or a registered agent actor's own bearer",
			"declarations require an authenticated principal")
	}
	// Classification is an ALLOW-list, because it feeds the activation root
	// of trust (ADR 0014 §3): only a Cloudflare Access identity, or a LAN
	// break-glass credential whose registered actor is a person (breakglass.go
	// downgrades every non-human holder to the agent provider), counts as
	// human. A synthetic transition-secret principal is refused outright --
	// holding a shared secret proves nothing about being a person -- and any
	// provider not named here is treated as an agent, failing closed.
	if p.Synthetic {
		return declengine.ActivationPrincipal{}, unauthorized(
			"use a Cloudflare Access identity or a registered actor's own bearer",
			"a shared transition secret cannot author or activate declarations")
	}
	author := p.ActorID
	if !declarationHumanProvider(p.Provider) {
		if author == "" {
			author = p.Subject
		}
		if author == "" {
			return declengine.ActivationPrincipal{}, unauthorized(
				"authenticate with an identifiable principal",
				"the authenticated principal carries no identifiable author")
		}
		return declengine.ActivationPrincipal{Kind: declengine.PrincipalAgent, Author: author}, nil
	}
	if author == "" {
		author = p.Email
	}
	if author == "" {
		author = p.CommonName
	}
	if author == "" {
		author = p.Subject
	}
	if author == "" {
		return declengine.ActivationPrincipal{}, unauthorized(
			"authenticate with an identifiable principal",
			"the authenticated principal carries no identifiable author")
	}
	return declengine.ActivationPrincipal{Kind: declengine.PrincipalHuman, Author: author}, nil
}

// classifyDeclarationError maps a declengine/decl domain error to an
// apiError. decl.Parse failures (schema, kind vocabulary) and
// declengine.ValidatePublish's one-level-deep refusal (c33/h64) are both
// domain rejections the caller can fix by changing the request: 422, with
// the same "call validate first" remediation workflows.go's publish uses.
func classifyDeclarationError(err error) *apiError {
	if err == nil {
		return nil
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	if errors.Is(err, postgres.ErrNotFound) {
		return notFound("check the declaration name or version id", "%v", err)
	}
	return unprocessable("call POST /v1alpha1/declarations/validate for the full diagnostic", "%v", err)
}

// declarationSourceRequest is components.schemas.DeclarationSource.
type declarationSourceRequest struct {
	Format string `json:"format"`
	Source string `json:"source"`
	// Author is decoded and NEVER read by any handler below (h62): it
	// exists only so a test can send a forged author and confirm the
	// recorded author is always the authenticated principal.
	Author string `json:"author,omitempty"`
}

func decodeDeclarationSource(r *http.Request) (declarationSourceRequest, *apiError) {
	var req declarationSourceRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return declarationSourceRequest{}, badRequest("send a JSON body matching DeclarationSource: {format, source}", "decode request body: %v", err)
	}
	if req.Source == "" {
		return declarationSourceRequest{}, badRequest("source must not be empty", "declaration source is required")
	}
	if req.Format == "" {
		req.Format = string(decl.FormatYAML)
	}
	if req.Format != string(decl.FormatYAML) && req.Format != string(decl.FormatJSON) {
		return declarationSourceRequest{}, badRequest(
			fmt.Sprintf("format must be %q or %q", decl.FormatYAML, decl.FormatJSON),
			"unknown format %q", req.Format)
	}
	return req, nil
}

// declarationValidationOut is components.schemas.DeclarationValidation.
type declarationValidationOut struct {
	Valid       bool     `json:"valid"`
	Name        string   `json:"name,omitempty"`
	Digest      string   `json:"digest,omitempty"`
	Diagnostics []string `json:"diagnostics"`
	// Warnings never makes valid false (c27/h21): every template reference
	// that may be missing and has no default is reported here, not as a
	// diagnostic.
	Warnings []string `json:"warnings"`
}

// handleValidateDeclaration is POST /v1alpha1/declarations/validate: parses
// and schema-checks the submitted source without writing anything. A
// document that fails to parse is a documented domain outcome (valid:
// false), not an HTTP error -- matching handleValidateWorkflow's posture
// (PRD §3.4: a domain outcome is not a technical failure).
func (s *Server) handleValidateDeclaration(w http.ResponseWriter, r *http.Request) error {
	req, apiErr := decodeDeclarationSource(r)
	if apiErr != nil {
		return apiErr
	}

	d, err := decl.Parse([]byte(req.Source), decl.Format(req.Format))
	if err != nil {
		writeJSON(w, http.StatusOK, declarationValidationOut{Diagnostics: []string{err.Error()}, Warnings: []string{}})
		return nil
	}
	digest, err := d.Digest()
	if err != nil {
		return internalError(err)
	}
	warnings, err := s.declarationReferenceWarnings(r.Context(), d.Name, *d)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusOK, declarationValidationOut{Valid: true, Name: d.Name, Digest: digest, Diagnostics: []string{}, Warnings: warnings})
	return nil
}

// declarationVersionOut is components.schemas.DeclarationVersion.
type declarationVersionOut struct {
	ID            string    `json:"id"`
	DeclarationID string    `json:"declaration_id"`
	Name          string    `json:"name"`
	Version       int       `json:"version"`
	Digest        string    `json:"digest"`
	Author        string    `json:"author"`
	CreatedAt     time.Time `json:"created_at"`
	// Warnings carries every template reference this publish found that may
	// be missing and has no default (c27/h21) -- never an error, and never
	// a reason the publish itself is refused.
	Warnings []string `json:"warnings"`
}

func declarationVersionOutOf(v postgres.DeclarationVersion) declarationVersionOut {
	return declarationVersionOut{
		ID: v.ID, DeclarationID: v.DeclarationID, Name: v.Name, Version: v.Version,
		Digest: v.Digest, Author: v.Author, CreatedAt: v.CreatedAt, Warnings: []string{},
	}
}

// handlePublishDeclaration is POST /v1alpha1/declarations: parses and
// validates the submitted source, applies the one-level-deep activation
// gate, and stores it as a new (or repeat, by digest) version -- entirely
// through declengine.Publish, the choke point t15 built. The recorded
// author is always the authenticated principal declarationPrincipal
// resolves; req.Author is decoded and never read.
func (s *Server) handlePublishDeclaration(w http.ResponseWriter, r *http.Request) error {
	req, apiErr := decodeDeclarationSource(r)
	if apiErr != nil {
		return apiErr
	}
	principal, apiErr := declarationPrincipal(r)
	if apiErr != nil {
		return apiErr
	}
	ctx := r.Context()

	v, err := declengine.Publish(ctx, s.Store, declengine.StoreActivationLookup{Store: s.Store}, declengine.PublishInput{
		NamespaceID:   s.NamespaceID,
		Body:          []byte(req.Source),
		Format:        decl.Format(req.Format),
		Principal:     principal,
		ClaimedAuthor: req.Author,
	})
	var overlapErr error
	if err != nil {
		if errors.Is(err, declengine.ErrOverlapReportFailed) {
			// The write SUCCEEDED; only the overlap report failed. Respond
			// with success plus a warning, never a failure.
			overlapErr = err
		} else {
			return classifyDeclarationError(err)
		}
	}

	out := declarationVersionOutOf(v)
	if d, parseErr := decl.Parse([]byte(req.Source), decl.Format(req.Format)); parseErr == nil {
		// The version is already durably written: a failure computing the
		// advisory reference warnings must not turn that into a 500.
		warnings, warnErr := s.declarationReferenceWarningsForID(ctx, v.DeclarationID, *d)
		if warnErr != nil {
			s.log.Warn("declaration published; reference warnings unavailable", "declaration_id", v.DeclarationID, "error", warnErr)
			warnings = append(warnings, "reference warnings could not be computed: "+warnErr.Error())
		}
		out.Warnings = warnings
	}

	if overlapErr != nil {
		writeJSONWithWarning(w, http.StatusOK, out, "declaration published; the overlap report failed: "+overlapErr.Error()) // warning responses are 200 by convention
		return nil
	}
	writeJSON(w, http.StatusCreated, out)
	return nil
}

// handleListDeclarations is GET /v1alpha1/declarations.
func (s *Server) handleListDeclarations(w http.ResponseWriter, r *http.Request) error {
	versions, err := s.Store.ListDeclarations(r.Context(), s.NamespaceID)
	if err != nil {
		return internalError(err)
	}
	out := make([]declarationVersionOut, len(versions))
	for i, v := range versions {
		out[i] = declarationVersionOutOf(v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
	return nil
}

// declarationShowOut is components.schemas.DeclarationShow: a declaration's
// newest version plus its outbound links and current activation status.
type declarationShowOut struct {
	declarationVersionOut
	Active          bool                 `json:"active"`
	ActiveVersionID string               `json:"active_version_id,omitempty"`
	Links           []declarationLinkOut `json:"links"`
}

type declarationLinkOut struct {
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// handleGetDeclaration is GET /v1alpha1/declarations/{name}.
func (s *Server) handleGetDeclaration(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	ctx := r.Context()
	v, err := s.Store.LatestDeclarationVersion(ctx, s.NamespaceID, name)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return notFound("check the declaration name", "no declaration named %q", name)
		}
		return internalError(err)
	}
	links, err := s.Store.ListDeclarationLinks(ctx, s.NamespaceID, v.DeclarationID)
	if err != nil {
		return internalError(err)
	}
	linkOut := make([]declarationLinkOut, 0, len(links))
	for _, l := range links {
		toName, err := s.Store.DeclarationName(ctx, l.ToDeclarationID)
		if err != nil && !errors.Is(err, postgres.ErrNotFound) {
			return internalError(err)
		}
		linkOut = append(linkOut, declarationLinkOut{To: toName, Kind: l.Kind})
	}
	active, activeVersionID, _, err := s.Store.DeclarationActivationStatus(ctx, s.NamespaceID, v.DeclarationID)
	if err != nil {
		return internalError(err)
	}
	out := declarationShowOut{declarationVersionOut: declarationVersionOutOf(v), Active: active, Links: linkOut}
	if active {
		out.ActiveVersionID = activeVersionID
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// declarationLinkRequest is components.schemas.DeclarationLinkRequest.
type declarationLinkRequest struct {
	To     string `json:"to"`
	Kind   string `json:"kind"`
	Author string `json:"author,omitempty"` // decoded, never read
}

// handleLinkDeclaration is POST /v1alpha1/declarations/{name}/links: records
// a must/can ordering relation from the named declaration to another
// already-published declaration (internal/store/postgres/declstore.go's
// LinkDeclarations).
func (s *Server) handleLinkDeclaration(w http.ResponseWriter, r *http.Request) error {
	fromName := r.PathValue("name")
	if _, apiErr := declarationPrincipal(r); apiErr != nil {
		return apiErr
	}
	var req declarationLinkRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return badRequest("send a JSON body matching DeclarationLinkRequest: {to, kind}", "decode request body: %v", err)
	}
	if req.To == "" {
		return badRequest("to names the declaration this one depends on", "to is required")
	}
	if req.Kind != "must" && req.Kind != "can" {
		return badRequest(`kind must be "must" or "can"`, "invalid link kind %q", req.Kind)
	}
	ctx := r.Context()
	from, err := s.Store.LatestDeclarationVersion(ctx, s.NamespaceID, fromName)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return notFound("publish the declaration first", "no declaration named %q", fromName)
		}
		return internalError(err)
	}
	to, err := s.Store.LatestDeclarationVersion(ctx, s.NamespaceID, req.To)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return notFound("publish the target declaration first", "no declaration named %q", req.To)
		}
		return internalError(err)
	}
	if err := s.Store.LinkDeclarations(ctx, s.NamespaceID, from.DeclarationID, to.DeclarationID, req.Kind); err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusCreated, declarationLinkOut{To: req.To, Kind: req.Kind})
	return nil
}

// createDeclarationAliasRequest is components.schemas.CreateDeclarationAliasRequest.
type createDeclarationAliasRequest struct {
	Name         string   `json:"name"`
	Declarations []string `json:"declarations,omitempty"`
	Author       string   `json:"author,omitempty"` // decoded, never read
}

type declarationAliasOut struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Declarations []string `json:"declarations"`
}

// handleCreateDeclarationAlias is POST /v1alpha1/declarations/aliases: names
// a chain, optionally attaching one or more already-published declarations
// as its initial members.
func (s *Server) handleCreateDeclarationAlias(w http.ResponseWriter, r *http.Request) error {
	if _, apiErr := declarationPrincipal(r); apiErr != nil {
		return apiErr
	}
	var req createDeclarationAliasRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return badRequest("send a JSON body matching CreateDeclarationAliasRequest: {name, declarations?}", "decode request body: %v", err)
	}
	if req.Name == "" {
		return badRequest("name identifies the alias", "name is required")
	}
	ctx := r.Context()
	// Resolve every member before writing anything, so an unknown member
	// refuses the whole request instead of leaving a half-built alias.
	memberIDs := make([]string, 0, len(req.Declarations))
	for _, declName := range req.Declarations {
		v, err := s.Store.LatestDeclarationVersion(ctx, s.NamespaceID, declName)
		if err != nil {
			if errors.Is(err, postgres.ErrNotFound) {
				return notFound("publish the declaration first", "no declaration named %q", declName)
			}
			return internalError(err)
		}
		memberIDs = append(memberIDs, v.DeclarationID)
	}
	alias, err := s.Store.CreateDeclarationAlias(ctx, s.NamespaceID, req.Name)
	if err != nil {
		return classify(err)
	}
	for _, id := range memberIDs {
		if err := s.Store.AddDeclarationToAlias(ctx, s.NamespaceID, req.Name, id); err != nil {
			return internalError(err)
		}
	}
	writeJSON(w, http.StatusCreated, declarationAliasOut{ID: alias.ID, Name: alias.Name, Declarations: req.Declarations})
	return nil
}

// declarationAliasDetailOut is components.schemas.DeclarationAliasDetail.
type declarationAliasDetailOut struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Parent       string   `json:"parent,omitempty"`
	Declarations []string `json:"declarations"`
	Aliases      []string `json:"aliases"`
}

// handleGetDeclarationAlias is GET /v1alpha1/declaration-aliases/{name}
// (task t21b, #328; spec h23/c31: "a chain alias resolves by name in every
// verb that accepts a chain"). This is the missing read route: alias
// create/move already exist as writes, but nothing could previously read
// an alias back by name -- `nodes chain show` and `nodes decl focus` on a
// real alias name both 404'd on the declaration-only routes.
//
// The path is /v1alpha1/declaration-aliases/{name}, not the more obvious
// /v1alpha1/declarations/aliases/{name} nested under the existing
// POST .../declarations/aliases collection: net/http.ServeMux's pattern
// conflict check refuses that nesting outright at server startup (a
// literal "aliases" third path segment cannot coexist with the existing
// four-segment GET .../{name}/focus, .../{name}/suggestions and
// .../{name}/evaluations routes -- none of those patterns uniformly
// dominates the other for a hypothetical alias literally named "focus" et
// al., so ServeMux calls it an unresolvable conflict, not a routing
// preference, and panics registering it). A distinct second-level resource
// name sidesteps that ambiguity entirely; server.go's route registration
// carries the same note. Unauthenticated, matching the other GET
// declaration routes (declarations.go's unauthorized doc comment, spec
// decision c45).
func (s *Server) handleGetDeclarationAlias(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	detail, err := s.Store.GetDeclarationAliasDetail(r.Context(), s.NamespaceID, name)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return notFound("check the alias name", "no alias named %q", name)
		}
		return internalError(err)
	}
	out := declarationAliasDetailOut{
		ID: detail.ID, Name: detail.Name, Parent: detail.ParentName,
		Declarations: detail.Declarations, Aliases: detail.Children,
	}
	if out.Declarations == nil {
		out.Declarations = []string{}
	}
	if out.Aliases == nil {
		out.Aliases = []string{}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// moveDeclarationAliasRequest is components.schemas.MoveDeclarationAliasRequest.
// One route serves both "move" (change an existing parent) and "nest"
// (assign a parent for the first time) -- they are the same store
// operation (MoveDeclarationAliasWithSupersedes), and Parent="" detaches
// the alias to the root.
type moveDeclarationAliasRequest struct {
	Parent               string `json:"parent,omitempty"`
	DeclarationVersionID string `json:"declaration_version_id"`
	Supersedes           string `json:"supersedes,omitempty"`
	Author               string `json:"author,omitempty"` // decoded, never read
}

// handleMoveDeclarationAlias is POST /v1alpha1/declarations/aliases/{name}/move.
func (s *Server) handleMoveDeclarationAlias(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	principal, apiErr := declarationPrincipal(r)
	if apiErr != nil {
		return apiErr
	}
	var req moveDeclarationAliasRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return badRequest(
			"send a JSON body matching MoveDeclarationAliasRequest: {parent?, declaration_version_id, supersedes?}",
			"decode request body: %v", err)
	}
	if req.DeclarationVersionID == "" {
		return badRequest("declaration_version_id names the version this move concerns", "declaration_version_id is required")
	}
	if v, err := s.Store.GetDeclarationVersion(r.Context(), req.DeclarationVersionID); err != nil || v.NamespaceID != s.NamespaceID {
		if err != nil && !errors.Is(err, postgres.ErrNotFound) {
			return internalError(err)
		}
		return notFound("check the declaration version id", "no declaration version %q", req.DeclarationVersionID)
	}
	if err := s.Store.MoveDeclarationAliasWithSupersedes(r.Context(), s.NamespaceID, name, req.Parent, req.DeclarationVersionID, declengine.ResolveAuthor(principal), req.Supersedes); err != nil {
		return classify(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "parent": req.Parent})
	return nil
}

// activateDeclarationRequest is components.schemas.ActivateDeclarationRequest.
type activateDeclarationRequest struct {
	VersionID  string `json:"version_id,omitempty"`
	Supersedes string `json:"supersedes,omitempty"`
	Author     string `json:"author,omitempty"` // decoded, never read
}

type declarationActivationOut struct {
	Name      string `json:"name"`
	VersionID string `json:"version_id"`
	Active    bool   `json:"active"`
}

func decodeActivationBody(r *http.Request) (activateDeclarationRequest, *apiError) {
	var req activateDeclarationRequest
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && err != io.EOF {
			return activateDeclarationRequest{}, badRequest(
				"send no body, or a JSON body matching ActivateDeclarationRequest: {version_id?, supersedes?}",
				"decode request body: %v", err)
		}
	}
	return req, nil
}

// resolveTargetVersion resolves the version an activate/deactivate call
// names -- an explicit version_id, or (when omitted) the declaration's
// newest published version.
func (s *Server) resolveTargetVersion(ctx context.Context, name, versionID string) (postgres.DeclarationVersion, *apiError) {
	if versionID != "" {
		v, err := s.Store.GetDeclarationVersion(ctx, versionID)
		if err != nil {
			if errors.Is(err, postgres.ErrNotFound) {
				return postgres.DeclarationVersion{}, notFound("check the version id", "no declaration version %q", versionID)
			}
			return postgres.DeclarationVersion{}, internalError(err)
		}
		// The version id is a body field: it may only select a version of
		// the declaration the URL names, in this namespace. Anything else
		// reads as absent, so a caller cannot record history against another
		// namespace's (or another declaration's) version.
		if v.NamespaceID != s.NamespaceID || v.Name != name {
			return postgres.DeclarationVersion{}, notFound("check the version id", "no version %q of declaration %q", versionID, name)
		}
		return v, nil
	}
	v, err := s.Store.LatestDeclarationVersion(ctx, s.NamespaceID, name)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return postgres.DeclarationVersion{}, notFound("publish the declaration first", "no declaration named %q", name)
		}
		return postgres.DeclarationVersion{}, internalError(err)
	}
	return v, nil
}

// handleActivateDeclaration is POST /v1alpha1/declarations/{name}/activate.
// The one-level-deep root of trust is enforced entirely inside
// declengine.Activate: an agent principal activating an activation
// declaration is refused there, regardless of this route's own role check.
func (s *Server) handleActivateDeclaration(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	principal, apiErr := declarationPrincipal(r)
	if apiErr != nil {
		return apiErr
	}
	req, apiErr := decodeActivationBody(r)
	if apiErr != nil {
		return apiErr
	}
	ctx := r.Context()
	target, apiErr := s.resolveTargetVersion(ctx, name, req.VersionID)
	if apiErr != nil {
		return apiErr
	}

	err := declengine.Activate(ctx, s.Store, s.NamespaceID, target.ID, principal, req.Supersedes)
	if err != nil && !errors.Is(err, declengine.ErrOverlapReportFailed) {
		return classifyDeclarationError(err)
	}
	out := declarationActivationOut{Name: name, VersionID: target.ID, Active: true}
	if err != nil {
		writeJSONWithWarning(w, http.StatusOK, out, "declaration activated; the overlap report failed: "+err.Error())
		return nil
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// handleDeactivateDeclaration is POST /v1alpha1/declarations/{name}/deactivate.
func (s *Server) handleDeactivateDeclaration(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	principal, apiErr := declarationPrincipal(r)
	if apiErr != nil {
		return apiErr
	}
	req, apiErr := decodeActivationBody(r)
	if apiErr != nil {
		return apiErr
	}
	ctx := r.Context()
	target, apiErr := s.resolveTargetVersion(ctx, name, req.VersionID)
	if apiErr != nil {
		return apiErr
	}

	err := declengine.Deactivate(ctx, s.Store, s.NamespaceID, target.ID, principal, req.Supersedes)
	if err != nil && !errors.Is(err, declengine.ErrOverlapReportFailed) {
		return classifyDeclarationError(err)
	}
	out := declarationActivationOut{Name: name, VersionID: target.ID, Active: false}
	if err != nil {
		writeJSONWithWarning(w, http.StatusOK, out, "declaration deactivated; the overlap report failed: "+err.Error())
		return nil
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// --- template reference warnings (c27/h21) ---------------------------------

// actionTemplateReferences walks a decl.Action's `with` payload (the only
// place internal/declengine/match.go renders template.Parse references,
// see renderAction) and returns every {step:name} / {step:name:default}
// reference it contains, in no particular order.
func actionTemplateReferences(a decl.Action) ([]template.Reference, error) {
	if len(a.With) == 0 {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(a.With, &value); err != nil {
		return nil, err
	}
	var refs []template.Reference
	var walk func(any) error
	walk = func(v any) error {
		switch x := v.(type) {
		case string:
			t, err := template.Parse(x)
			if err != nil {
				return err
			}
			refs = append(refs, t.References()...)
		case []any:
			for _, e := range x {
				if err := walk(e); err != nil {
					return err
				}
			}
		case map[string]any:
			for _, e := range x {
				if err := walk(e); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(value); err != nil {
		return nil, err
	}
	return refs, nil
}

// declarationReferenceWarnings resolves the declaration's own existing
// entity by name (empty declarationID if it has never been published) and
// delegates to declarationReferenceWarningsForID.
func (s *Server) declarationReferenceWarnings(ctx context.Context, name string, d decl.Declaration) ([]string, error) {
	declarationID := ""
	if v, err := s.Store.LatestDeclarationVersion(ctx, s.NamespaceID, name); err == nil {
		declarationID = v.DeclarationID
	} else if !errors.Is(err, postgres.ErrNotFound) {
		return nil, err
	}
	return s.declarationReferenceWarningsForID(ctx, declarationID, d)
}

// declarationReferenceWarningsForID implements c27/h21: publish (and
// validate) warn -- never refuse -- on every action.with template
// reference that may be missing and carries no default:
//
//   - a reference naming a declaration this one has only a 'can' link to
//     (never a 'must' link) may not have fired before this one does;
//   - a reference naming a declaration this one has no link to at all can
//     never resolve;
//   - a numeric {N:...} reference walks N steps back along the causal
//     lineage at firing time; this route only knows the declaration's
//     DIRECT 'must' links, so N beyond that count is reported as "may not
//     exist" -- a conservative, documented approximation (the full
//     transitive lineage is a firing-time fact, not a publish-time one).
//
// A reference with an explicit default (ref.DefaultPresent) or step "0"
// (the current firing) never warns.
func (s *Server) declarationReferenceWarningsForID(ctx context.Context, declarationID string, d decl.Declaration) ([]string, error) {
	refs, err := actionTemplateReferences(d.Action)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return []string{}, nil
	}

	var links []postgres.DeclarationLink
	if declarationID != "" {
		links, err = s.Store.ListDeclarationLinks(ctx, s.NamespaceID, declarationID)
		if err != nil {
			return nil, err
		}
	}
	mustCount := 0
	linkKindTo := map[string]string{}
	for _, l := range links {
		if l.Kind == "must" {
			mustCount++
		}
		if linkKindTo[l.ToDeclarationID] != "must" {
			linkKindTo[l.ToDeclarationID] = l.Kind
		}
	}

	nameIDCache := map[string]string{}
	resolveID := func(name string) string {
		if id, ok := nameIDCache[name]; ok {
			return id
		}
		id := ""
		if v, err := s.Store.LatestDeclarationVersion(ctx, s.NamespaceID, name); err == nil {
			id = v.DeclarationID
		}
		nameIDCache[name] = id
		return id
	}

	seen := map[string]bool{}
	warnings := []string{}
	for _, ref := range refs {
		if ref.DefaultPresent || ref.Step == "0" {
			continue
		}
		var reason string
		if n, convErr := strconv.Atoi(ref.Step); convErr == nil {
			if n <= 0 {
				continue
			}
			if n > mustCount {
				reason = fmt.Sprintf("step %d back may not exist (this declaration has %d guaranteed 'must' predecessor(s))", n, mustCount)
			}
		} else {
			targetID := resolveID(ref.Step)
			switch {
			case targetID == "":
				reason = fmt.Sprintf("%q is not a linked predecessor of this declaration", ref.Step)
			case linkKindTo[targetID] == "must":
				// Guaranteed present at firing time; no warning.
			case linkKindTo[targetID] == "can":
				reason = fmt.Sprintf("crosses a 'can' link to %q, which may not have fired", ref.Step)
			default:
				reason = fmt.Sprintf("%q is not linked to this declaration", ref.Step)
			}
		}
		if reason == "" {
			continue
		}
		w := fmt.Sprintf("action.with reference {%s:%s} %s and has no default", ref.Step, ref.Name, reason)
		if !seen[w] {
			seen[w] = true
			warnings = append(warnings, w)
		}
	}
	return warnings, nil
}

// declarationHumanProvider names the principal providers that are people:
// a Cloudflare Access assertion, and a break-glass inbound credential
// bound to a human actor (breakglass.go rewrites any non-human holder's
// provider to principalProviderActorToken before it gets here).
func declarationHumanProvider(provider string) bool {
	switch provider {
	case "cloudflare-access", principalProviderInboundCredential:
		return true
	}
	return false
}
