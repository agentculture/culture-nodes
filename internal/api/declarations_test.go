package api_test

// Task t19 (#328; spec c27, honesty h21) declaration API route tests, plus
// the security gate closing what t15 left open. Every request body shape
// mirrors api/openapi/openapi.yaml the way a real client would (see
// api_test.go's package doc) rather than importing internal/api's private
// wire types.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/auth"
	"github.com/agentculture/culture-nodes/internal/store"
	pgtest "github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// --- wire shapes (mirroring the OpenAPI contract) --------------------------

type declTrigger struct {
	Kind string          `json:"kind"`
	With json.RawMessage `json:"with,omitempty"`
}

type declAction struct {
	Kind string          `json:"kind"`
	With json.RawMessage `json:"with,omitempty"`
}

type declNode struct {
	Name     string `json:"name"`
	Deadline string `json:"deadline"`
}

type declBody struct {
	Name        string      `json:"name"`
	Trigger     declTrigger `json:"trigger"`
	Action      declAction  `json:"action"`
	StartNode   declNode    `json:"start_node"`
	LandingNode declNode    `json:"landing_node"`
}

type declarationSourceReq struct {
	Format string `json:"format"`
	Source string `json:"source"`
	Author string `json:"author,omitempty"`
}

type declarationVersionResp struct {
	ID            string   `json:"id"`
	DeclarationID string   `json:"declaration_id"`
	Name          string   `json:"name"`
	Version       int      `json:"version"`
	Digest        string   `json:"digest"`
	Author        string   `json:"author"`
	Warnings      []string `json:"warnings"`
}

type declarationValidationResp struct {
	Valid       bool     `json:"valid"`
	Name        string   `json:"name"`
	Digest      string   `json:"digest"`
	Diagnostics []string `json:"diagnostics"`
	Warnings    []string `json:"warnings"`
}

type declarationLinkResp struct {
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type declarationShowResp struct {
	declarationVersionResp
	Active          bool                  `json:"active"`
	ActiveVersionID string                `json:"active_version_id"`
	Links           []declarationLinkResp `json:"links"`
}

type declarationActivationResp struct {
	Name      string `json:"name"`
	VersionID string `json:"version_id"`
	Active    bool   `json:"active"`
}

// ordinaryDeclSource renders a minimal, valid ordinary (agent.work)
// declaration as JSON source, with an optional action.with note carrying
// template references.
func ordinaryDeclSource(name, note string) string {
	var with json.RawMessage
	if note != "" {
		b, _ := json.Marshal(map[string]string{"note": note})
		with = b
	} else {
		with = json.RawMessage(`{}`)
	}
	body := declBody{
		Name:        name,
		Trigger:     declTrigger{Kind: "timer"},
		Action:      declAction{Kind: "agent.work", With: with},
		StartNode:   declNode{Name: "ready", Deadline: "none"},
		LandingNode: declNode{Name: "waiting", Deadline: "1h"},
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// activationDeclSource renders a minimal activation declaration (action
// kind "activate") targeting the named declaration.
func activationDeclSource(name, target string) string {
	with, _ := json.Marshal(map[string]string{"declaration": target})
	body := declBody{
		Name:        name,
		Trigger:     declTrigger{Kind: "declaration.proposed"},
		Action:      declAction{Kind: "activate", With: with},
		StartNode:   declNode{Name: "root", Deadline: "none"},
		LandingNode: declNode{Name: "activated", Deadline: "none"},
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// --- human (Cloudflare Access) principal fixture ---------------------------

// newDeclarationHumanFixture builds a server with a principal verifier
// configured, and a human identity bound with the namespace_administrator
// role -- the role declaration writes require by default (principal.go's
// principalPolicy, no special case beyond agents:true). Requests are sent
// through srv.AccessHandler(), the same way principal_test.go exercises the
// Access-era gate.
func newDeclarationHumanFixture(t *testing.T) (srv *api.Server, nsID, token, actorID string) {
	t.Helper()
	s := requireStore(t)
	nsID = pgtest.MustNamespace(t, s, "decl-human").ID
	actorID = store.NewULID()
	if _, err := s.Pool().Exec(context.Background(),
		`INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol) VALUES ($1,$2,$3,1,'human','http')`,
		actorID, nsID, "decl-human-"+actorID); err != nil {
		t.Fatal(err)
	}
	token = "decl-human-sub"
	if _, err := s.BindIdentity(context.Background(), nsID, "cloudflare-access", token, actorID, []string{string(auth.RoleNamespaceAdministrator)}); err != nil {
		t.Fatal(err)
	}
	srv, err := api.NewServer(s, nsID, api.WithPrincipalVerifier(verifierFunc(func(_ context.Context, tok string) (auth.Principal, error) {
		return auth.Principal{Subject: tok, Email: tok + "@example.test", Kind: auth.PrincipalInteractive}, nil
	})))
	if err != nil {
		t.Fatal(err)
	}
	return srv, nsID, token, actorID
}

// doAccess sends a request through srv.AccessHandler() with the given
// Access token (empty means no header, i.e. unauthenticated), decoding a
// JSON response into out when non-nil.
func doAccess(t *testing.T, srv *api.Server, method, path, token string, body, out any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(b))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Cf-Access-Jwt-Assertion", token)
	}
	rr := httptest.NewRecorder()
	srv.AccessHandler().ServeHTTP(rr, req)
	if out != nil && rr.Body.Len() > 0 {
		if err := json.Unmarshal(rr.Body.Bytes(), out); err != nil {
			t.Fatalf("decode response %s: %v", rr.Body.String(), err)
		}
	}
	return rr
}

// --- happy path --------------------------------------------------------

func TestDeclarationValidatePublishShowList(t *testing.T) {
	srv, _, token, humanActorID := newDeclarationHumanFixture(t)

	var validation declarationValidationResp
	rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/validate", token,
		declarationSourceReq{Format: "json", Source: ordinaryDeclSource("leaf", "")}, &validation)
	if rr.Code != http.StatusOK {
		t.Fatalf("validate: status = %d: %s", rr.Code, rr.Body.String())
	}
	if !validation.Valid || validation.Name != "leaf" || len(validation.Warnings) != 0 {
		t.Fatalf("validate: %+v", validation)
	}

	var published declarationVersionResp
	rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations", token,
		declarationSourceReq{Format: "json", Source: ordinaryDeclSource("leaf", "")}, &published)
	if rr.Code != http.StatusCreated {
		t.Fatalf("publish: status = %d: %s", rr.Code, rr.Body.String())
	}
	if published.Name != "leaf" || published.Author != humanActorID || published.Version != 1 {
		t.Fatalf("publish: %+v", published)
	}
	if len(published.Warnings) != 0 {
		t.Fatalf("publish warnings: %+v, want none for a declaration with no template references", published.Warnings)
	}

	var shown declarationShowResp
	rr = doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/leaf", "", nil, &shown)
	if rr.Code != http.StatusOK {
		t.Fatalf("show: status = %d: %s", rr.Code, rr.Body.String())
	}
	if shown.Name != "leaf" || shown.Active {
		t.Fatalf("show: %+v", shown)
	}

	var list struct {
		Items []declarationVersionResp `json:"items"`
	}
	rr = doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations", "", nil, &list)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: status = %d: %s", rr.Code, rr.Body.String())
	}
	found := false
	for _, v := range list.Items {
		if v.Name == "leaf" {
			found = true
		}
	}
	if !found {
		t.Fatalf("list did not include leaf: %+v", list.Items)
	}
}

// TestPublishWarnsOnMissingReferencesWithNoDefault is the core acceptance
// criterion (c27/h21): a template reference that crosses only a 'can'
// link, or a numeric step beyond the declaration's guaranteed 'must'
// count, warns; one with a default never does; and linking the
// declaration to a 'must' predecessor makes that reference stop warning.
func TestPublishWarnsOnMissingReferencesWithNoDefault(t *testing.T) {
	srv, _, token, _ := newDeclarationHumanFixture(t)

	for _, name := range []string{"must-pred", "can-pred"} {
		var v declarationVersionResp
		rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations", token,
			declarationSourceReq{Format: "json", Source: ordinaryDeclSource(name, "")}, &v)
		if rr.Code != http.StatusCreated {
			t.Fatalf("publish %s: status = %d: %s", name, rr.Code, rr.Body.String())
		}
	}

	note := "Hi {must-pred:owner}, {can-pred:owner}, {2:extra}, {1:safe:fallback}"
	source := ordinaryDeclSource("consumer", note)

	// Before any link exists: every non-default, non-"0" reference warns.
	var beforeLink declarationVersionResp
	rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations", token,
		declarationSourceReq{Format: "json", Source: source}, &beforeLink)
	if rr.Code != http.StatusCreated {
		t.Fatalf("publish consumer: status = %d: %s", rr.Code, rr.Body.String())
	}
	wantSubstrings := []string{"must-pred", "can-pred", "step 2"}
	for _, want := range wantSubstrings {
		found := false
		for _, w := range beforeLink.Warnings {
			if strings.Contains(w, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("warnings before linking = %+v, want one containing %q", beforeLink.Warnings, want)
		}
	}
	for _, w := range beforeLink.Warnings {
		if strings.Contains(w, "{1:safe") {
			t.Errorf("a reference with a default must never warn: %+v", beforeLink.Warnings)
		}
	}

	// Link consumer -> must-pred (must) and consumer -> can-pred (can).
	for _, l := range []struct{ to, kind string }{{"must-pred", "must"}, {"can-pred", "can"}} {
		rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/consumer/links", token,
			map[string]string{"to": l.to, "kind": l.kind}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("link consumer->%s: status = %d: %s", l.to, rr.Code, rr.Body.String())
		}
	}

	// Re-validate the same source: the 'must' reference stops warning, the
	// 'can' reference and the out-of-range numeric step still do.
	var afterLink declarationValidationResp
	rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/validate", token,
		declarationSourceReq{Format: "json", Source: source}, &afterLink)
	if rr.Code != http.StatusOK {
		t.Fatalf("validate after link: status = %d: %s", rr.Code, rr.Body.String())
	}
	for _, w := range afterLink.Warnings {
		if strings.Contains(w, "must-pred") {
			t.Errorf("a reference crossing a 'must' link must not warn: %+v", afterLink.Warnings)
		}
	}
	sawCan, sawStep := false, false
	for _, w := range afterLink.Warnings {
		if strings.Contains(w, "can-pred") {
			sawCan = true
		}
		if strings.Contains(w, "step 2") {
			sawStep = true
		}
	}
	if !sawCan {
		t.Errorf("expected a warning naming the 'can'-linked reference: %+v", afterLink.Warnings)
	}
	if !sawStep {
		t.Errorf("expected a warning for the numeric step beyond the guaranteed 'must' count: %+v", afterLink.Warnings)
	}
}

func TestDeclarationLinkAndAliasCreateMove(t *testing.T) {
	srv, _, token, _ := newDeclarationHumanFixture(t)

	for _, name := range []string{"one", "two"} {
		rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations", token,
			declarationSourceReq{Format: "json", Source: ordinaryDeclSource(name, "")}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("publish %s: status = %d: %s", name, rr.Code, rr.Body.String())
		}
	}

	var link declarationLinkResp
	rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/two/links", token,
		map[string]string{"to": "one", "kind": "must"}, &link)
	if rr.Code != http.StatusCreated || link.To != "one" || link.Kind != "must" {
		t.Fatalf("link: status = %d, body = %+v", rr.Code, link)
	}

	var shown declarationShowResp
	doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/two", "", nil, &shown)
	if len(shown.Links) != 1 || shown.Links[0].To != "one" || shown.Links[0].Kind != "must" {
		t.Fatalf("show links: %+v", shown.Links)
	}

	// Alias: create with an initial member, then move (nest) it under a
	// second, newly created alias.
	var alias struct {
		ID           string   `json:"id"`
		Name         string   `json:"name"`
		Declarations []string `json:"declarations"`
	}
	rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/aliases", token,
		map[string]any{"name": "chain-a", "declarations": []string{"one"}}, &alias)
	if rr.Code != http.StatusCreated || alias.Name != "chain-a" {
		t.Fatalf("create alias: status = %d, body = %+v", rr.Code, alias)
	}

	rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/aliases", token,
		map[string]any{"name": "chain-parent"}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create parent alias: status = %d", rr.Code)
	}

	var one declarationShowResp
	doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/one", "", nil, &one)

	var moved map[string]any
	rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/aliases/chain-a/move", token,
		map[string]string{"parent": "chain-parent", "declaration_version_id": one.ID}, &moved)
	if rr.Code != http.StatusOK {
		t.Fatalf("move alias: status = %d: %s", rr.Code, rr.Body.String())
	}
	if moved["parent"] != "chain-parent" {
		t.Fatalf("move alias result: %+v", moved)
	}
}

func TestDeclarationActivateAndDeactivate(t *testing.T) {
	srv, _, token, _ := newDeclarationHumanFixture(t)

	var published declarationVersionResp
	rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations", token,
		declarationSourceReq{Format: "json", Source: ordinaryDeclSource("toggle", "")}, &published)
	if rr.Code != http.StatusCreated {
		t.Fatalf("publish: status = %d: %s", rr.Code, rr.Body.String())
	}

	var activation declarationActivationResp
	rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/toggle/activate", token, map[string]string{}, &activation)
	if rr.Code != http.StatusOK || !activation.Active || activation.VersionID != published.ID {
		t.Fatalf("activate: status = %d, body = %+v", rr.Code, activation)
	}

	var shown declarationShowResp
	doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/toggle", "", nil, &shown)
	if !shown.Active || shown.ActiveVersionID != published.ID {
		t.Fatalf("show after activate: %+v", shown)
	}

	rr = doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations/toggle/deactivate", token, map[string]string{}, &activation)
	if rr.Code != http.StatusOK || activation.Active {
		t.Fatalf("deactivate: status = %d, body = %+v", rr.Code, activation)
	}
	doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/toggle", "", nil, &shown)
	if shown.Active {
		t.Fatalf("show after deactivate: %+v", shown)
	}
}

// --- adversarial / security (closing what t15 left open) -------------------

// TestDeclarationWritesRefuseUnauthenticated proves every mutating
// declaration route refuses a request that carries no authenticated
// principal at all -- closed by default, the same posture
// TestAdhocRefusedWhenNoSecretConfigured pins for the ad-hoc lane. This
// holds with NO principal verifier configured too (newFixture's default),
// because declarationPrincipal requires PrincipalFromContext regardless.
func TestDeclarationWritesRefuseUnauthenticated(t *testing.T) {
	f := newFixture(t)

	mutating := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/v1alpha1/declarations", `{"format":"json","source":"{}"}`},
		{http.MethodPost, "/v1alpha1/declarations/validate-target/links", `{"to":"x","kind":"must"}`},
		{http.MethodPost, "/v1alpha1/declarations/aliases", `{"name":"x"}`},
		{http.MethodPost, "/v1alpha1/declarations/aliases/x/move", `{"parent":"y","declaration_version_id":"z"}`},
		{http.MethodPost, "/v1alpha1/declarations/x/activate", `{}`},
		{http.MethodPost, "/v1alpha1/declarations/x/deactivate", `{}`},
	}
	for _, m := range mutating {
		t.Run(m.method+" "+m.path, func(t *testing.T) {
			resp, body := doJSON(t, f.client, m.method, f.url(m.path), json.RawMessage(m.body), nil)
			requireStatus(t, resp, body, http.StatusUnauthorized)
		})
	}
}

// TestDeclarationPublishIgnoresForgedAuthor proves a body-supplied author
// claim never reaches the recorded author -- the recorded author is always
// the authenticated principal (c89/h62).
func TestDeclarationPublishIgnoresForgedAuthor(t *testing.T) {
	srv, _, token, humanActorID := newDeclarationHumanFixture(t)

	var published declarationVersionResp
	rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations", token,
		declarationSourceReq{Format: "json", Source: ordinaryDeclSource("forged-author-target", ""), Author: "human:forged-mallory"},
		&published)
	if rr.Code != http.StatusCreated {
		t.Fatalf("publish: status = %d: %s", rr.Code, rr.Body.String())
	}
	if published.Author != humanActorID {
		t.Fatalf("recorded author = %q, want the authenticated principal, not the forged claim", published.Author)
	}
	if strings.Contains(published.Author, "mallory") {
		t.Fatalf("forged author leaked into the recorded author: %q", published.Author)
	}
}

// TestAgentActivatingActivationDeclarationIsRefused is the adversarial case
// the brief names explicitly: an agent credential may publish and
// self-activate an ORDINARY declaration, but activating an ACTIVATION
// declaration is refused regardless of role -- the one-level-deep root of
// trust (ADR 0014, c33/h64) enforced inside declengine.Activate itself, not
// merely by this route's policy.
func TestAgentActivatingActivationDeclarationIsRefused(t *testing.T) {
	f, agent := newAgentBearerFixture(t)

	var ordinary declarationVersionResp
	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/declarations"), mergeGateToken,
		declarationSourceReq{Format: "json", Source: ordinaryDeclSource("agent-target", "")}, &ordinary)
	requireStatus(t, resp, body, http.StatusCreated)

	var rule declarationVersionResp
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/declarations"), mergeGateToken,
		declarationSourceReq{Format: "json", Source: activationDeclSource("agent-rule", "agent-target")}, &rule)
	requireStatus(t, resp, body, http.StatusCreated)

	// An agent may self-activate the ordinary declaration.
	var activation declarationActivationResp
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/declarations/agent-target/activate"), mergeGateToken,
		map[string]string{}, &activation)
	requireStatus(t, resp, body, http.StatusOK)
	if !activation.Active {
		t.Fatalf("agent self-activation of an ordinary declaration should succeed: %+v", activation)
	}

	// The SAME agent credential activating the activation declaration is
	// refused -- the whole point of the one-level-deep gate.
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/declarations/agent-rule/activate"), mergeGateToken,
		map[string]string{}, nil)
	if resp.StatusCode < 400 {
		t.Fatalf("agent %s activating an activation declaration: status = %d, want a 4xx refusal: %s", agent, resp.StatusCode, body)
	}

	var shown declarationShowResp
	doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/declarations/agent-rule"), nil, &shown)
	if shown.Active {
		t.Fatalf("agent-rule must not be active after the refused activation attempt: %+v", shown)
	}
}
