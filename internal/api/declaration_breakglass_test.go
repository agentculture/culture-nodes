package api_test

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/store"
)

// The inventory comes from server.go, so adding a declaration write route
// forces this test to account for its break-glass authorization explicitly.
func declarationWriteRoutes(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`mux\.HandleFunc\("POST (/v1alpha1/(?:declarations(?:/[^" ]*)?|declaration-engine/switch|sensitivity-approvals/[^" ]*|repository-visibility|destination-audiences))"`)
	matches := re.FindAllStringSubmatch(string(source), -1)
	var routes []string
	for _, match := range matches {
		routes = append(routes, match[1])
	}
	return routes
}

func TestDeclarationWriteRouteInventory(t *testing.T) {
	// These are all the POST declaration, sensitivity and repository-visibility
	// routes in server.go.
	// The explicit inventory makes a new route a test failure until reviewed.
	want := map[string]bool{
		"/v1alpha1/declarations/validate":               true,
		"/v1alpha1/declarations":                        true,
		"/v1alpha1/declarations/aliases":                true,
		"/v1alpha1/declarations/aliases/{name}/move":    true,
		"/v1alpha1/declarations/{name}/links":           true,
		"/v1alpha1/declarations/{name}/activate":        true,
		"/v1alpha1/declarations/{name}/deactivate":      true,
		"/v1alpha1/declaration-engine/switch":           true,
		"/v1alpha1/sensitivity-approvals/{id}/decision": true,
		"/v1alpha1/repository-visibility":               true,
		"/v1alpha1/destination-audiences":               true,
	}
	routes := declarationWriteRoutes(t)
	if len(routes) != len(want) {
		t.Fatalf("server.go declaration write routes = %v; expected inventory has %d entries", routes, len(want))
	}
	for _, route := range routes {
		if !want[route] {
			t.Fatalf("unreviewed declaration write route in server.go: %s", route)
		}
	}
}

func TestBreakGlassDeclarationWriteBoundary(t *testing.T) {
	routes := declarationWriteRoutes(t)
	gated, _ := breakGlassFixtures(t)
	partyKey := "company/operator-" + strings.ToLower(store.NewULID())
	insertActorRevision(t, gated.store, gated.nsID, partyKey, "human", 1)
	credential := issueBreakGlassCredential(t, gated.store, partyKey)
	for _, route := range routes {
		if route == "/v1alpha1/declarations/{name}/deactivate" || route == "/v1alpha1/declaration-engine/switch" {
			continue
		}
		t.Run(route, func(t *testing.T) {
			path := strings.NewReplacer("{name}", "sample", "{id}", store.NewULID()).Replace(route)
			var out map[string]any
			resp, body := doJSONBearer(t, gated.client, http.MethodPost, gated.url(path), credential, map[string]any{}, &out)
			requireStatus(t, resp, body, http.StatusForbidden)
			if out["reason"] != "forbidden_role" {
				t.Fatalf("%s: reason = %v, want forbidden_role: %s", route, out["reason"], body)
			}
		})
	}
}

func TestHumanBreakGlassDeactivatesDeclarationWithBoundAuthor(t *testing.T) {
	gated, _ := breakGlassFixtures(t)
	partyKey := "company/operator-" + strings.ToLower(store.NewULID())
	actorID := insertActorRevision(t, gated.store, gated.nsID, partyKey, "human", 1)
	credential := issueBreakGlassCredential(t, gated.store, partyKey)
	v := publishActiveDecl(t, gated.store, gated.nsID, chainDecl("emergency-stop", "start", "end"))
	var out map[string]any
	resp, body := doJSONBearer(t, gated.client, http.MethodPost, gated.url("/v1alpha1/declarations/emergency-stop/deactivate"), credential,
		map[string]string{"version_id": v.ID, "author": "forged"}, &out)
	requireStatus(t, resp, body, http.StatusOK)
	if out["active"] != false {
		t.Fatalf("deactivation response = %s", body)
	}
	var actor string
	if err := gated.store.Pool().QueryRow(context.Background(),
		`SELECT actor FROM declaration_history WHERE namespace_id=$1 AND target_version_id=$2 ORDER BY seq DESC LIMIT 1`, gated.nsID, v.ID).Scan(&actor); err != nil || actor != actorID {
		t.Fatalf("deactivation actor = %q, err=%v; want %q", actor, err, actorID)
	}
}

func TestHumanBreakGlassSwitchOnlyBeforeWithBoundAuthor(t *testing.T) {
	gated, _ := breakGlassFixtures(t)
	partyKey := "company/operator-" + strings.ToLower(store.NewULID())
	actorID := insertActorRevision(t, gated.store, gated.nsID, partyKey, "human", 1)
	credential := issueBreakGlassCredential(t, gated.store, partyKey)
	for _, mode := range []string{declengine.ModeShadow, declengine.ModeAfter} {
		t.Run(mode, func(t *testing.T) {
			var out map[string]any
			resp, body := doJSONBearer(t, gated.client, http.MethodPost, gated.url("/v1alpha1/declaration-engine/switch"), credential,
				map[string]string{"mode": mode}, &out)
			requireStatus(t, resp, body, http.StatusForbidden)
			if !strings.Contains(string(body), "Cloudflare Access") {
				t.Fatalf("%s refusal lacks Access remediation: %s", mode, body)
			}
		})
	}
	var out map[string]any
	resp, body := doJSONBearer(t, gated.client, http.MethodPost, gated.url("/v1alpha1/declaration-engine/switch"), credential,
		map[string]string{"mode": declengine.ModeBefore, "reason": "emergency stop"}, &out)
	requireStatus(t, resp, body, http.StatusOK)
	if out["mode"] != declengine.ModeBefore {
		t.Fatalf("switch response = %s", body)
	}
	var actor string
	if err := gated.store.Pool().QueryRow(context.Background(),
		`SELECT actor FROM engine_switch_history WHERE namespace_id=$1 ORDER BY seq DESC LIMIT 1`, gated.nsID).Scan(&actor); err != nil || actor != actorID {
		t.Fatalf("switch actor = %q, err=%v; want %q", actor, err, actorID)
	}
}

func TestNonHumanBreakGlassCannotStopDeclarationOrSwitch(t *testing.T) {
	gated, _ := breakGlassFixtures(t)
	partyKey := "company/agent-" + strings.ToLower(store.NewULID())
	insertActorRevision(t, gated.store, gated.nsID, partyKey, "agent", 1)
	credential := issueBreakGlassCredential(t, gated.store, partyKey)
	for _, path := range []string{"/v1alpha1/declarations/sample/deactivate", "/v1alpha1/declaration-engine/switch"} {
		var out map[string]any
		resp, body := doJSONBearer(t, gated.client, http.MethodPost, gated.url(path), credential,
			map[string]string{"mode": declengine.ModeBefore}, &out)
		requireStatus(t, resp, body, http.StatusForbidden)
		if out["reason"] != "forbidden_role" {
			t.Fatalf("%s: reason = %v, want forbidden_role: %s", path, out["reason"], body)
		}
	}
}

// Access administrators retain the existing route role; the role gate must
// let each route reach its handler, even when the sample body is invalid.
func TestAccessAdminStillReachesEveryDeclarationWrite(t *testing.T) {
	srv, _, token, _ := newDeclarationHumanFixture(t)
	for _, route := range declarationWriteRoutes(t) {
		path := strings.NewReplacer("{name}", "sample", "{id}", store.NewULID()).Replace(route)
		rr := doAccess(t, srv, http.MethodPost, path, token, map[string]any{}, nil)
		if rr.Code == http.StatusForbidden || rr.Code == http.StatusUnauthorized {
			t.Errorf("Access admin refused on %s: %d %s", route, rr.Code, rr.Body.String())
		}
	}
}
