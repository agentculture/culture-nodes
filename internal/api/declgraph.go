// The declaration graph API (task t20, #328): two read-only routes layered
// on top of t19's declaration routes (declarations.go) and t4's
// internal/store/postgres/declstore.go primitives.
//
//   - GET /v1alpha1/declarations/{name}/focus (spec c25/c62/c63, h19): a
//     distance-N neighborhood of the ONE global declaration graph. Distance
//     0 is only the named declaration; distance 1 adds its direct
//     neighbours across declaration_links; distance 2 the next ring, and
//     so on. Distance counts hops across BOTH directions and BOTH must and
//     can links by default, and is filterable by direction (both|up|down)
//     and link kind (both|must|can). Declarations count as hops; the
//     start/landing waiting-state nodes carried alongside them never do
//     (c63) -- the BFS below only ever walks declaration_links, never a
//     node.
//   - GET /v1alpha1/declarations/{name}/suggestions (spec c41, h29): lists
//     candidate predecessor/successor declarations whose action "produces"
//     artifact type matches a trigger "consumes" artifact type
//     (internal/decl/kinds). It is read-only -- calling it creates no
//     link; linking is always an explicit POST .../links call
//     (declarations.go's handleLinkDeclaration).
//
// Both routes are unauthenticated GETs, matching the existing declaration
// list/show routes -- this API's phase-1 default (declarations.go's
// unauthorized doc comment, spec decision c45).
//
// declarationGraphOut is deliberately the SAME response shape for both the
// single-declaration view (distance=0, no links, no neighbours) and the
// chain/focus view (distance>0): c24 asks for one payload shape so the web
// view (t22) and the CLI (t21) can both render from it.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/decl/kinds"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// declarationGraphDeclarationOut is one declaration in a focus payload,
// carrying its distance from the center (0 for the center itself).
type declarationGraphDeclarationOut struct {
	Name          string `json:"name"`
	DeclarationID string `json:"declaration_id"`
	Version       int    `json:"version"`
	Digest        string `json:"digest"`
	Distance      int    `json:"distance"`
	TriggerKind   string `json:"trigger_kind"`
	ActionKind    string `json:"action_kind"`
	StartNode     string `json:"start_node"`
	LandingNode   string `json:"landing_node"`
}

// declarationGraphNodeOut is a drawn waiting-state node -- a declaration's
// start_node or landing_node -- included so a graph can be drawn (c63).
// These are never visited by the focus BFS and never count as a hop.
type declarationGraphNodeOut struct {
	Declaration string `json:"declaration"`
	Role        string `json:"role"` // "start" or "landing"
	Name        string `json:"name"`
	Deadline    string `json:"deadline"`
}

// declarationGraphLinkOut is one must/can edge between two declarations
// that are both present in Declarations.
type declarationGraphLinkOut struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// declarationGraphOut is components.schemas.DeclarationFocus -- the one
// shape (c24) GET .../focus renders, at any distance.
type declarationGraphOut struct {
	Center       string                           `json:"center"`
	Distance     int                              `json:"distance"`
	Direction    string                           `json:"direction"`
	Link         string                           `json:"link"`
	Declarations []declarationGraphDeclarationOut `json:"declarations"`
	Nodes        []declarationGraphNodeOut        `json:"nodes"`
	Links        []declarationGraphLinkOut        `json:"links"`
}

func parseFocusDistance(raw string) (int, *apiError) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, badRequest("distance must be a non-negative integer", "invalid distance %q", raw)
	}
	return n, nil
}

func parseFocusEnum(raw, field string, allowed ...string) (string, *apiError) {
	if raw == "" {
		return allowed[0], nil
	}
	for _, a := range allowed {
		if raw == a {
			return raw, nil
		}
	}
	return "", badRequest(fmt.Sprintf("%s must be one of %v", field, allowed), "invalid %s %q", field, raw)
}

// neighborIDs returns the declaration ids directly reachable from id, in
// the requested direction. "up" walks id's own outgoing links (toward the
// declarations it names as a dependency via POST .../links {to: ...}) --
// its predecessors. "down" walks the links that name id as their "to"
// (toward the declarations that depend on it) -- its successors.
func neighborIDs(id, direction string, byFrom, byTo map[string][]postgres.DeclarationLink) []string {
	var out []string
	if direction == "both" || direction == "up" {
		for _, l := range byFrom[id] {
			out = append(out, l.ToDeclarationID)
		}
	}
	if direction == "both" || direction == "down" {
		for _, l := range byTo[id] {
			out = append(out, l.FromDeclarationID)
		}
	}
	return out
}

// resolveFocusCenters resolves {name} to the distance-0 declaration id set
// GET .../focus starts its BFS from (task t21b, #328; spec c31/h23: "a
// chain alias resolves by name in every verb that accepts a chain"). A
// declaration named {name} always wins over an alias of the same name
// (documented behavior, not an ambiguity): only when no declaration named
// {name} exists is {name} looked up as an alias, whose distance-0 set is
// every declaration that is a direct member of it OR a member of any alias
// nested under it (DeclarationAliasMemberIDsRecursive). Neither existing
// names {name} at all is a 404 naming both possibilities.
func (s *Server) resolveFocusCenters(ctx context.Context, name string) ([]string, *apiError) {
	if v, err := s.Store.LatestDeclarationVersion(ctx, s.NamespaceID, name); err == nil {
		return []string{v.DeclarationID}, nil
	} else if !errors.Is(err, postgres.ErrNotFound) {
		return nil, internalError(err)
	}
	ids, err := s.Store.DeclarationAliasMemberIDsRecursive(ctx, s.NamespaceID, name)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return nil, notFound("check the declaration or alias name", "no declaration or alias named %q", name)
		}
		return nil, internalError(err)
	}
	return ids, nil
}

// handleDeclarationFocus is GET /v1alpha1/declarations/{name}/focus.
func (s *Server) handleDeclarationFocus(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	ctx := r.Context()

	distance, apiErr := parseFocusDistance(r.URL.Query().Get("distance"))
	if apiErr != nil {
		return apiErr
	}
	direction, apiErr := parseFocusEnum(r.URL.Query().Get("direction"), "direction", "both", "up", "down")
	if apiErr != nil {
		return apiErr
	}
	linkFilter, apiErr := parseFocusEnum(r.URL.Query().Get("link"), "link", "both", "must", "can")
	if apiErr != nil {
		return apiErr
	}

	centerIDs, apiErr := s.resolveFocusCenters(ctx, name)
	if apiErr != nil {
		return apiErr
	}

	links, err := s.Store.ListNamespaceDeclarationLinks(ctx, s.NamespaceID)
	if err != nil {
		return internalError(err)
	}
	versions, err := s.Store.ListDeclarations(ctx, s.NamespaceID)
	if err != nil {
		return internalError(err)
	}
	byID := make(map[string]postgres.DeclarationVersion, len(versions))
	for _, v := range versions {
		byID[v.DeclarationID] = v
	}

	byFrom := map[string][]postgres.DeclarationLink{}
	byTo := map[string][]postgres.DeclarationLink{}
	for _, l := range links {
		if linkFilter != "both" && l.Kind != linkFilter {
			continue
		}
		byFrom[l.FromDeclarationID] = append(byFrom[l.FromDeclarationID], l)
		byTo[l.ToDeclarationID] = append(byTo[l.ToDeclarationID], l)
	}

	// Declarations count as hops; nodes never do (c63) -- this BFS walks
	// declaration_links only. Distance 0 may hold several ids at once (an
	// alias center's member declarations, spec c31/h23) instead of the
	// single declaration a plain declaration name resolves to.
	distanceOf := map[string]int{}
	frontier := make([]string, 0, len(centerIDs))
	for _, id := range centerIDs {
		if _, seen := distanceOf[id]; seen {
			continue
		}
		distanceOf[id] = 0
		frontier = append(frontier, id)
	}
	for hop := 1; hop <= distance && len(frontier) > 0; hop++ {
		var next []string
		for _, id := range frontier {
			for _, n := range neighborIDs(id, direction, byFrom, byTo) {
				if _, seen := distanceOf[n]; seen {
					continue
				}
				distanceOf[n] = hop
				next = append(next, n)
			}
		}
		frontier = next
	}

	ids := make([]string, 0, len(distanceOf))
	for id := range distanceOf {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if distanceOf[ids[i]] != distanceOf[ids[j]] {
			return distanceOf[ids[i]] < distanceOf[ids[j]]
		}
		return byID[ids[i]].Name < byID[ids[j]].Name
	})

	out := declarationGraphOut{
		Center: name, Distance: distance, Direction: direction, Link: linkFilter,
		Declarations: []declarationGraphDeclarationOut{},
		Nodes:        []declarationGraphNodeOut{},
		Links:        []declarationGraphLinkOut{},
	}
	for _, id := range ids {
		v, ok := byID[id]
		if !ok {
			// A link named a declaration id with no current version --
			// should not happen (declarations are never deleted), but
			// skip defensively rather than 500 the whole focus view.
			continue
		}
		var d decl.Declaration
		if err := json.Unmarshal(v.Body, &d); err != nil {
			return internalError(fmt.Errorf("decode stored declaration %q: %w", v.Name, err))
		}
		out.Declarations = append(out.Declarations, declarationGraphDeclarationOut{
			Name: v.Name, DeclarationID: v.DeclarationID, Version: v.Version, Digest: v.Digest,
			Distance: distanceOf[id], TriggerKind: d.Trigger.Kind, ActionKind: d.Action.Kind,
			StartNode: d.StartNode.Name, LandingNode: d.LandingNode.Name,
		})
		out.Nodes = append(out.Nodes,
			declarationGraphNodeOut{Declaration: v.Name, Role: "start", Name: d.StartNode.Name, Deadline: d.StartNode.Deadline},
			declarationGraphNodeOut{Declaration: v.Name, Role: "landing", Name: d.LandingNode.Name, Deadline: d.LandingNode.Deadline},
		)
	}
	for _, l := range links {
		if linkFilter != "both" && l.Kind != linkFilter {
			continue
		}
		from, okFrom := byID[l.FromDeclarationID]
		to, okTo := byID[l.ToDeclarationID]
		if !okFrom || !okTo {
			continue
		}
		if _, inFrom := distanceOf[l.FromDeclarationID]; !inFrom {
			continue
		}
		if _, inTo := distanceOf[l.ToDeclarationID]; !inTo {
			continue
		}
		out.Links = append(out.Links, declarationGraphLinkOut{From: from.Name, To: to.Name, Kind: l.Kind})
	}

	writeJSON(w, http.StatusOK, out)
	return nil
}

// --- suggestions (c41, h29) -------------------------------------------------

// declarationSuggestionOut is one candidate predecessor or successor: the
// declaration whose produces/consumes signature matches, and which
// artifact type matched.
type declarationSuggestionOut struct {
	Name         string `json:"name"`
	ArtifactType string `json:"artifact_type"`
}

// declarationSuggestionsOut is components.schemas.DeclarationSuggestions.
type declarationSuggestionsOut struct {
	Center       string                     `json:"center"`
	Predecessors []declarationSuggestionOut `json:"predecessors"`
	Successors   []declarationSuggestionOut `json:"successors"`
}

func triggerConsumes(name string) []kinds.ArtifactType {
	k, ok := kinds.Trigger(name)
	if !ok {
		return nil
	}
	return k.Consumes
}

func actionProduces(name string) []kinds.ArtifactType {
	k, ok := kinds.Action(name)
	if !ok {
		return nil
	}
	return k.Produces
}

// matchingArtifactType returns the first artifact type common to both
// lists, ignoring ArtifactNone (which never denotes a real produced
// artifact -- matching on it would suggest every consumes-nothing trigger
// as a successor of every produces-nothing action).
func matchingArtifactType(a, b []kinds.ArtifactType) (kinds.ArtifactType, bool) {
	for _, x := range a {
		if x == kinds.ArtifactNone {
			continue
		}
		for _, y := range b {
			if x == y {
				return x, true
			}
		}
	}
	return "", false
}

// handleDeclarationSuggestions is GET /v1alpha1/declarations/{name}/suggestions.
// It never writes: no link is created here, ever (h29) -- the only writer
// of declaration_links is handleLinkDeclaration's explicit POST
// .../links call.
func (s *Server) handleDeclarationSuggestions(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	ctx := r.Context()

	center, err := s.Store.LatestDeclarationVersion(ctx, s.NamespaceID, name)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return notFound("check the declaration name", "no declaration named %q", name)
		}
		return internalError(err)
	}
	var centerDecl decl.Declaration
	if err := json.Unmarshal(center.Body, &centerDecl); err != nil {
		return internalError(fmt.Errorf("decode stored declaration %q: %w", name, err))
	}
	centerConsumes := triggerConsumes(centerDecl.Trigger.Kind)
	centerProduces := actionProduces(centerDecl.Action.Kind)

	versions, err := s.Store.ListDeclarations(ctx, s.NamespaceID)
	if err != nil {
		return internalError(err)
	}

	out := declarationSuggestionsOut{Center: name, Predecessors: []declarationSuggestionOut{}, Successors: []declarationSuggestionOut{}}
	for _, v := range versions {
		if v.DeclarationID == center.DeclarationID {
			continue
		}
		var d decl.Declaration
		if err := json.Unmarshal(v.Body, &d); err != nil {
			return internalError(fmt.Errorf("decode stored declaration %q: %w", v.Name, err))
		}
		// Predecessor candidate: this OTHER declaration's action produces
		// something the CENTER's trigger consumes.
		if t, ok := matchingArtifactType(actionProduces(d.Action.Kind), centerConsumes); ok {
			out.Predecessors = append(out.Predecessors, declarationSuggestionOut{Name: v.Name, ArtifactType: string(t)})
		}
		// Successor candidate: the CENTER's action produces something this
		// OTHER declaration's trigger consumes.
		if t, ok := matchingArtifactType(centerProduces, triggerConsumes(d.Trigger.Kind)); ok {
			out.Successors = append(out.Successors, declarationSuggestionOut{Name: v.Name, ArtifactType: string(t)})
		}
	}
	sort.Slice(out.Predecessors, func(i, j int) bool { return out.Predecessors[i].Name < out.Predecessors[j].Name })
	sort.Slice(out.Successors, func(i, j int) bool { return out.Successors[i].Name < out.Successors[j].Name })

	writeJSON(w, http.StatusOK, out)
	return nil
}
