// Task t20 (#328) declaration graph API route tests: GET .../focus (spec
// c25/c62/c63, honesty h19) and GET .../suggestions (spec c41, honesty
// h29). Wire shapes mirror api/openapi/openapi.yaml, matching
// declarations_test.go's convention; the fixture helpers
// (newDeclarationHumanFixture, doAccess, ordinaryDeclSource) live there.
package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	api "github.com/agentculture/culture-nodes/internal/api"
)

type declGraphDeclarationResp struct {
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

type declGraphNodeResp struct {
	Declaration string `json:"declaration"`
	Role        string `json:"role"`
	Name        string `json:"name"`
	Deadline    string `json:"deadline"`
}

type declGraphLinkResp struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type declGraphFocusResp struct {
	Center       string                     `json:"center"`
	Distance     int                        `json:"distance"`
	Direction    string                     `json:"direction"`
	Link         string                     `json:"link"`
	Declarations []declGraphDeclarationResp `json:"declarations"`
	Nodes        []declGraphNodeResp        `json:"nodes"`
	Links        []declGraphLinkResp        `json:"links"`
}

type declGraphSuggestionResp struct {
	Name         string `json:"name"`
	ArtifactType string `json:"artifact_type"`
}

type declGraphSuggestionsResp struct {
	Center       string                    `json:"center"`
	Predecessors []declGraphSuggestionResp `json:"predecessors"`
	Successors   []declGraphSuggestionResp `json:"successors"`
}

// declSourceOfKind renders a minimal declaration with an explicit
// trigger/action kind pair, so a test can pick kinds whose produces/consumes
// signature (internal/decl/kinds) is known.
func declSourceOfKind(name, triggerKind, actionKind string) string {
	body := declBody{
		Name:        name,
		Trigger:     declTrigger{Kind: triggerKind},
		Action:      declAction{Kind: actionKind, With: json.RawMessage(`{}`)},
		StartNode:   declNode{Name: "ready", Deadline: "none"},
		LandingNode: declNode{Name: "waiting", Deadline: "1h"},
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// --- focus fixture ----------------------------------------------------

// declFocusFixture builds the graph:
//
//	pred-pred --must--> pred-must --must--> center --can--> pred-can
//	                                          ^
//	                                          |must/can
//	                                    succ-must, succ-can
//	                                          ^
//	                                          |must
//	                                     succ-succ
//
// Edges are recorded the way handleLinkDeclaration always records them:
// POST /declarations/{from}/links {to, kind} inserts (from, to, kind). So
// "center"'s own predecessors (declarations it depends on) are recorded as
// center -> pred-*, and its successors (declarations that depend on it) are
// recorded as succ-* -> center. isolated has no links at all.
func declFocusFixture(t *testing.T) (srv *api.Server, token string) {
	t.Helper()
	s, _, tok, _ := newDeclarationHumanFixture(t)

	for _, name := range []string{"center", "pred-must", "pred-can", "succ-must", "succ-can", "pred-pred", "succ-succ", "isolated"} {
		rr := doAccess(t, s, http.MethodPost, "/v1alpha1/declarations", tok,
			declarationSourceReq{Format: "json", Source: ordinaryDeclSource(name, "")}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("publish %s: status = %d: %s", name, rr.Code, rr.Body.String())
		}
	}

	link := func(from, to, kind string) {
		t.Helper()
		rr := doAccess(t, s, http.MethodPost, "/v1alpha1/declarations/"+from+"/links", tok,
			map[string]string{"to": to, "kind": kind}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("link %s->%s: status = %d: %s", from, to, rr.Code, rr.Body.String())
		}
	}
	link("center", "pred-must", "must")
	link("center", "pred-can", "can")
	link("succ-must", "center", "must")
	link("succ-can", "center", "can")
	link("pred-must", "pred-pred", "must")
	link("succ-succ", "succ-must", "must")

	return s, tok
}

func names(decls []declGraphDeclarationResp) map[string]int {
	out := map[string]int{}
	for _, d := range decls {
		out[d.Name] = d.Distance
	}
	return out
}

func focus(t *testing.T, srv *api.Server, token, name, query string) declGraphFocusResp {
	t.Helper()
	var out declGraphFocusResp
	rr := doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/"+name+"/focus"+query, "", nil, &out)
	if rr.Code != http.StatusOK {
		t.Fatalf("focus %s%s: status = %d: %s", name, query, rr.Code, rr.Body.String())
	}
	return out
}

// TestDeclarationFocusDistanceZeroIsOnlyCenter is c25's first claim:
// distance 0 is only the named declaration, regardless of any links.
func TestDeclarationFocusDistanceZeroIsOnlyCenter(t *testing.T) {
	srv, token := declFocusFixture(t)
	out := focus(t, srv, token, "center", "")
	got := names(out.Declarations)
	if len(got) != 1 || got["center"] != 0 {
		t.Fatalf("distance 0 declarations = %+v, want only center at distance 0", got)
	}
	if len(out.Links) != 0 {
		t.Fatalf("distance 0 links = %+v, want none (nothing else is in view)", out.Links)
	}
	// Nodes (start/landing waiting states) are still carried for the center
	// itself, so a single-declaration view can still draw a graph (c24).
	if len(out.Nodes) != 2 {
		t.Fatalf("distance 0 nodes = %+v, want the center's own start+landing nodes", out.Nodes)
	}
}

// TestDeclarationFocusDistanceOneBothBothIsDirectNeighbours is c25's
// second claim: distance 1 adds exactly the directly connected
// declarations, in both directions, across both link kinds.
func TestDeclarationFocusDistanceOneBothBothIsDirectNeighbours(t *testing.T) {
	srv, token := declFocusFixture(t)
	out := focus(t, srv, token, "center", "?distance=1")
	got := names(out.Declarations)
	want := map[string]int{"center": 0, "pred-must": 1, "pred-can": 1, "succ-must": 1, "succ-can": 1}
	if len(got) != len(want) {
		t.Fatalf("distance 1 declarations = %+v, want %+v", got, want)
	}
	for name, dist := range want {
		if got[name] != dist {
			t.Fatalf("distance 1 declarations = %+v, want %+v", got, want)
		}
	}
	if _, present := got["pred-pred"]; present {
		t.Fatalf("distance 1 must not reach the second ring: %+v", got)
	}
	if _, present := got["isolated"]; present {
		t.Fatalf("distance 1 must not include an unlinked declaration: %+v", got)
	}
}

// TestDeclarationFocusDistanceTwoBothBothAddsSecondRing is c25's "and so on"
// claim (h19: exactly the declarations within N hops).
func TestDeclarationFocusDistanceTwoBothBothAddsSecondRing(t *testing.T) {
	srv, token := declFocusFixture(t)
	out := focus(t, srv, token, "center", "?distance=2")
	got := names(out.Declarations)
	want := map[string]int{
		"center":    0,
		"pred-must": 1, "pred-can": 1, "succ-must": 1, "succ-can": 1,
		"pred-pred": 2, "succ-succ": 2,
	}
	if len(got) != len(want) {
		t.Fatalf("distance 2 declarations = %+v, want %+v", got, want)
	}
	for name, dist := range want {
		if got[name] != dist {
			t.Fatalf("distance 2 declarations = %+v, want %+v", got, want)
		}
	}
	if _, present := got["isolated"]; present {
		t.Fatalf("distance 2 must never include an unlinked declaration: %+v", got)
	}
}

// TestDeclarationFocusDirectionFilter is h19's direction filter: "up"
// (toward what the center depends on) and "down" (toward what depends on
// the center) each see only their own side.
func TestDeclarationFocusDirectionFilter(t *testing.T) {
	srv, token := declFocusFixture(t)

	up := focus(t, srv, token, "center", "?distance=2&direction=up")
	gotUp := names(up.Declarations)
	wantUp := map[string]int{"center": 0, "pred-must": 1, "pred-can": 1, "pred-pred": 2}
	if len(gotUp) != len(wantUp) {
		t.Fatalf("direction=up declarations = %+v, want %+v", gotUp, wantUp)
	}
	for name, dist := range wantUp {
		if gotUp[name] != dist {
			t.Fatalf("direction=up declarations = %+v, want %+v", gotUp, wantUp)
		}
	}
	if _, present := gotUp["succ-must"]; present {
		t.Fatalf("direction=up must not cross to a successor: %+v", gotUp)
	}

	down := focus(t, srv, token, "center", "?distance=2&direction=down")
	gotDown := names(down.Declarations)
	wantDown := map[string]int{"center": 0, "succ-must": 1, "succ-can": 1, "succ-succ": 2}
	if len(gotDown) != len(wantDown) {
		t.Fatalf("direction=down declarations = %+v, want %+v", gotDown, wantDown)
	}
	for name, dist := range wantDown {
		if gotDown[name] != dist {
			t.Fatalf("direction=down declarations = %+v, want %+v", gotDown, wantDown)
		}
	}
	if _, present := gotDown["pred-must"]; present {
		t.Fatalf("direction=down must not cross to a predecessor: %+v", gotDown)
	}
}

// TestDeclarationFocusLinkFilter is h19's link-kind filter: "must" and
// "can" each traverse only their own kind of edge, at every hop.
func TestDeclarationFocusLinkFilter(t *testing.T) {
	srv, token := declFocusFixture(t)

	must := focus(t, srv, token, "center", "?distance=2&link=must")
	gotMust := names(must.Declarations)
	wantMust := map[string]int{"center": 0, "pred-must": 1, "succ-must": 1, "pred-pred": 2, "succ-succ": 2}
	if len(gotMust) != len(wantMust) {
		t.Fatalf("link=must declarations = %+v, want %+v", gotMust, wantMust)
	}
	for name, dist := range wantMust {
		if gotMust[name] != dist {
			t.Fatalf("link=must declarations = %+v, want %+v", gotMust, wantMust)
		}
	}
	if _, present := gotMust["pred-can"]; present {
		t.Fatalf("link=must must not cross a 'can' edge: %+v", gotMust)
	}

	can := focus(t, srv, token, "center", "?distance=2&link=can")
	gotCan := names(can.Declarations)
	// pred-can and succ-can have no further outgoing "can" edges of their
	// own, so the second ring is empty under this filter -- proving the
	// filter is applied at every hop, not just the first.
	wantCan := map[string]int{"center": 0, "pred-can": 1, "succ-can": 1}
	if len(gotCan) != len(wantCan) {
		t.Fatalf("link=can declarations = %+v, want %+v", gotCan, wantCan)
	}
	for name, dist := range wantCan {
		if gotCan[name] != dist {
			t.Fatalf("link=can declarations = %+v, want %+v", gotCan, wantCan)
		}
	}

	// Combine direction and link filters: only the must-linked predecessor
	// ring.
	upMust := focus(t, srv, token, "center", "?distance=2&direction=up&link=must")
	gotUpMust := names(upMust.Declarations)
	wantUpMust := map[string]int{"center": 0, "pred-must": 1, "pred-pred": 2}
	if len(gotUpMust) != len(wantUpMust) {
		t.Fatalf("direction=up&link=must declarations = %+v, want %+v", gotUpMust, wantUpMust)
	}
	for name, dist := range wantUpMust {
		if gotUpMust[name] != dist {
			t.Fatalf("direction=up&link=must declarations = %+v, want %+v", gotUpMust, wantUpMust)
		}
	}
}

// TestDeclarationFocusNodesCarriedButNotHops is c63: nodes are drawn
// alongside the returned declarations, but the distance count only ever
// reflects declaration hops.
func TestDeclarationFocusNodesCarriedButNotHops(t *testing.T) {
	srv, token := declFocusFixture(t)
	out := focus(t, srv, token, "center", "?distance=1")
	if len(out.Nodes) != 2*len(out.Declarations) {
		t.Fatalf("nodes = %d, declarations = %d, want exactly a start+landing pair per declaration",
			len(out.Nodes), len(out.Declarations))
	}
	sawStart, sawLanding := false, false
	for _, n := range out.Nodes {
		if n.Declaration == "center" && n.Role == "start" && n.Name == "ready" {
			sawStart = true
		}
		if n.Declaration == "center" && n.Role == "landing" && n.Name == "waiting" {
			sawLanding = true
		}
	}
	if !sawStart || !sawLanding {
		t.Fatalf("nodes = %+v, want center's own start+landing nodes present", out.Nodes)
	}
}

// TestDeclarationFocusUnknownDeclaration404s and
// TestDeclarationFocusRejectsInvalidQuery round out the route's error
// shape (matching declarations.go's classifyDeclarationError posture).
func TestDeclarationFocusUnknownDeclaration404s(t *testing.T) {
	srv, _, _, _ := newDeclarationHumanFixture(t)
	rr := doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/does-not-exist/focus", "", nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("focus of unknown declaration: status = %d, want 404", rr.Code)
	}
}

// TestDeclarationFocusOnAliasName is task t21b (#328), spec c31/h23: "a
// chain alias resolves by name in every verb that accepts a chain" --
// GET .../focus on an alias name that is NOT itself a declaration must not
// 404. A parent alias directly names one declaration and nests a child
// alias that names a second; focusing on the PARENT alias's name at
// distance 0 must return both member declarations (nested child included),
// each at distance 0, with no center-vs-alias artifact leaking into the
// response shape.
func TestDeclarationFocusOnAliasName(t *testing.T) {
	srv, token := declFocusFixture(t)

	for _, name := range []string{"chain-parent-member", "chain-child-member"} {
		rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations", token,
			declarationSourceReq{Format: "json", Source: ordinaryDeclSource(name, "")}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("publish %s: status = %d: %s", name, rr.Code, rr.Body.String())
		}
	}
	rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/aliases", token,
		map[string]any{"name": "focus-parent", "declarations": []string{"chain-parent-member"}}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create parent alias: status = %d: %s", rr.Code, rr.Body.String())
	}
	rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/aliases", token,
		map[string]any{"name": "focus-child", "declarations": []string{"chain-child-member"}}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create child alias: status = %d: %s", rr.Code, rr.Body.String())
	}
	var childMemberShown declarationShowResp
	doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/chain-child-member", "", nil, &childMemberShown)
	var moved map[string]any
	rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/aliases/focus-child/move", token,
		map[string]string{"parent": "focus-parent", "declaration_version_id": childMemberShown.ID}, &moved)
	if rr.Code != http.StatusOK {
		t.Fatalf("nest child alias: status = %d: %s", rr.Code, rr.Body.String())
	}

	out := focus(t, srv, token, "focus-parent", "")
	got := names(out.Declarations)
	want := map[string]int{"chain-parent-member": 0, "chain-child-member": 0}
	if len(got) != len(want) {
		t.Fatalf("focus on parent alias declarations = %+v, want %+v", got, want)
	}
	for name, dist := range want {
		if got[name] != dist {
			t.Fatalf("focus on parent alias declarations = %+v, want %+v", got, want)
		}
	}
	if out.Center != "focus-parent" {
		t.Fatalf("focus center = %q, want the alias name echoed back", out.Center)
	}

	// An unknown name -- neither a declaration nor an alias -- still 404s.
	rr = doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/no-such-name-or-alias/focus", "", nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("focus of unknown name: status = %d, want 404: %s", rr.Code, rr.Body.String())
	}
}

func TestDeclarationFocusRejectsInvalidQuery(t *testing.T) {
	srv, token := declFocusFixture(t)
	cases := []string{"?distance=-1", "?distance=banana", "?direction=sideways", "?link=maybe"}
	for _, q := range cases {
		rr := doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/center/focus"+q, token, nil, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("focus%s: status = %d, want 400: %s", q, rr.Code, rr.Body.String())
		}
	}
}

// --- suggestions (c41/h29) ----------------------------------------------

// TestDeclarationSuggestionsMatchProducesConsumesAndCreateNoLink is the
// core acceptance criterion for GET .../suggestions: "producer" (action
// agent.work, which internal/decl/kinds registers as producing
// github.pr) is suggested as a PREDECESSOR of "consumer" (trigger
// github.pr.approved, which consumes github.pr); "consumer" is
// symmetrically suggested as a SUCCESSOR of "producer". An unrelated
// declaration whose signature matches neither never appears. Calling the
// route twice, for both declarations, creates no link -- proven by
// re-showing each declaration afterward and finding its links list still
// empty.
func TestDeclarationSuggestionsMatchProducesConsumesAndCreateNoLink(t *testing.T) {
	srv, _, token, _ := newDeclarationHumanFixture(t)

	for _, tc := range []struct{ name, trigger, action string }{
		{"producer", "timer", "agent.work"},                    // produces github.pr
		{"consumer", "github.pr.approved", "discord.post"},     // consumes github.pr, produces discord.message
		{"unrelated", "jira.issue.created", "jira.transition"}, // consumes/produces jira.issue only
	} {
		rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations", token,
			declarationSourceReq{Format: "json", Source: declSourceOfKind(tc.name, tc.trigger, tc.action)}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("publish %s: status = %d: %s", tc.name, rr.Code, rr.Body.String())
		}
	}

	var producerSug declGraphSuggestionsResp
	rr := doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/producer/suggestions", "", nil, &producerSug)
	if rr.Code != http.StatusOK {
		t.Fatalf("suggestions producer: status = %d: %s", rr.Code, rr.Body.String())
	}
	if len(producerSug.Predecessors) != 0 {
		t.Fatalf("producer predecessors = %+v, want none (nothing produces what timer consumes)", producerSug.Predecessors)
	}
	if !containsSuggestion(producerSug.Successors, "consumer") {
		t.Fatalf("producer successors = %+v, want consumer (matches github.pr)", producerSug.Successors)
	}
	if containsSuggestion(producerSug.Successors, "unrelated") {
		t.Fatalf("producer successors = %+v, must not include unrelated", producerSug.Successors)
	}

	var consumerSug declGraphSuggestionsResp
	rr = doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/consumer/suggestions", "", nil, &consumerSug)
	if rr.Code != http.StatusOK {
		t.Fatalf("suggestions consumer: status = %d: %s", rr.Code, rr.Body.String())
	}
	if !containsSuggestion(consumerSug.Predecessors, "producer") {
		t.Fatalf("consumer predecessors = %+v, want producer (matches github.pr)", consumerSug.Predecessors)
	}
	if containsSuggestion(consumerSug.Predecessors, "unrelated") {
		t.Fatalf("consumer predecessors = %+v, must not include unrelated", consumerSug.Predecessors)
	}
	if len(consumerSug.Successors) != 0 {
		t.Fatalf("consumer successors = %+v, want none (nothing consumes discord.message)", consumerSug.Successors)
	}

	// No link call was ever made: both declarations must still show zero
	// links (h29's "linking is always the user's or agent's explicit
	// choice").
	for _, name := range []string{"producer", "consumer"} {
		var shown declarationShowResp
		doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/"+name, "", nil, &shown)
		if len(shown.Links) != 0 {
			t.Fatalf("%s links = %+v after calling suggestions, want none: suggestions must never create a link", name, shown.Links)
		}
	}
}

func TestDeclarationSuggestionsUnknownDeclaration404s(t *testing.T) {
	srv, _, _, _ := newDeclarationHumanFixture(t)
	rr := doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/does-not-exist/suggestions", "", nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("suggestions of unknown declaration: status = %d, want 404", rr.Code)
	}
}

func containsSuggestion(items []declGraphSuggestionResp, name string) bool {
	for _, it := range items {
		if it.Name == name {
			return true
		}
	}
	return false
}
