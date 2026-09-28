package api_test

// Task t38 (#328): the engine switch route and the declaration engine's
// seat on POST /v1alpha1/events, end to end through the Access handler.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/auth"
	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/store"
	pgtest "github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

type declSwitchResp struct {
	NamespaceID   string `json:"namespace_id"`
	Mode          string `json:"mode"`
	Previous      string `json:"previous"`
	EngineEnabled bool   `json:"engine_enabled"`
	ReplayError   string `json:"replay_error"`
}

// artifactDispatcher answers as a stamping bridge would: its artifact id
// binds the firing's marker, so a reaction can carry verified lineage.
type artifactDispatcher struct{}

func (artifactDispatcher) Dispatch(_ context.Context, r declengine.DispatchRequest) (declengine.DispatchResult, error) {
	return declengine.DispatchResult{ArtifactID: "artifact-" + r.Firing.ID}, nil
}

func chainDecl(name, start, landing string) decl.Declaration {
	return decl.Declaration{Name: name, Condition: "true",
		Trigger:     decl.Trigger{Kind: "pr-upkeep.pr", ReentryLimit: 3, HopLimit: 20, RateCeiling: "30/h"},
		Action:      decl.Action{Kind: "agent.work", With: json.RawMessage(`{"uses":"actor://test"}`)},
		StartNode:   decl.Node{Name: start, Deadline: "none"},
		LandingNode: decl.Node{Name: landing, Deadline: "1h"}}
}

func postEventAccess(t *testing.T, srv *api.Server, secret string, body any) api.EventDeliveryOut {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1alpha1/events", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	rr := httptest.NewRecorder()
	srv.AccessHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /v1alpha1/events: %d %s", rr.Code, rr.Body.String())
	}
	var out api.EventDeliveryOut
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Acceptances 1 and 3: POST /v1alpha1/events reaches the declaration engine;
// a human flips the switch; a flip to 'before' freezes the open node (the
// reaction that arrives meanwhile is stored, not evaluated); a flip to
// 'after' replays it once the flip commits, firing the reaction exactly once.
func TestDeclarationSwitchRouteFreezesAndReplaysThroughEventDelivery(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	nsID := pgtest.MustNamespace(t, s, "decl-switch").ID
	human := store.NewULID()
	if _, err := s.Pool().Exec(ctx, `INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol) VALUES ($1,$2,$3,1,'human','http')`, human, nsID, "switch-human-"+human); err != nil {
		t.Fatal(err)
	}
	const token = "switch-human-sub"
	if _, err := s.BindIdentity(ctx, nsID, "cloudflare-access", token, human, []string{string(auth.RoleNamespaceAdministrator)}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TCA_API_SWITCH_KEY", strings.Repeat("a", 32))
	sw := declengine.PostgresSwitchStore{Store: s}
	eng, err := declengine.New(declengine.Config{MarkerKeyEnv: "TCA_API_SWITCH_KEY"}, declengine.PostgresBackend{Store: s}, declengine.PostgresMarkerStore{Store: s},
		declengine.ShadowGate{Switch: sw, Underlying: artifactDispatcher{}})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := api.NewServer(s, nsID, api.WithDeclarationEngine(eng), api.WithEventTokenSecret(eventTokenSecret),
		api.WithPrincipalVerifier(verifierFunc(func(_ context.Context, tok string) (auth.Principal, error) {
			return auth.Principal{Subject: tok, Email: tok + "@example.test", Kind: auth.PrincipalInteractive}, nil
		})))
	if err != nil {
		t.Fatal(err)
	}
	va := publishActiveDecl(t, s, nsID, chainDecl("sw-a", declengine.RootNode, "waiting"))
	vb := publishActiveDecl(t, s, nsID, chainDecl("sw-b", "waiting", "done"))

	var got declSwitchResp
	if rr := doAccess(t, srv, http.MethodGet, "/v1alpha1/declaration-engine/switch", "", nil, &got); rr.Code != http.StatusOK || got.Mode != declengine.ModeBefore || !got.EngineEnabled {
		t.Fatalf("GET switch: %d %+v", rr.Code, got)
	}
	if rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declaration-engine/switch", token, map[string]string{"mode": "after", "reason": "t38"}, &got); rr.Code != http.StatusOK || got.Mode != declengine.ModeAfter || got.Previous != declengine.ModeBefore {
		t.Fatalf("flip to after: %d %+v %s", rr.Code, got, rr.Body.String())
	}
	var actor string
	if err := s.Pool().QueryRow(ctx, `SELECT actor FROM engine_switch_history WHERE namespace_id=$1 ORDER BY seq DESC LIMIT 1`, nsID).Scan(&actor); err != nil || actor != human {
		t.Fatalf("flip attributed to %q (err=%v), want the authenticated human %s", actor, err, human)
	}

	// The event route offers the delivery to the declaration engine.
	first := postEventAccess(t, srv, eventTokenSecret, map[string]any{"name": "pr-upkeep.pr", "payload": map[string]any{}})
	var firingID string
	if err := s.Pool().QueryRow(ctx, `SELECT id FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3`, nsID, first.Event.ID, va.DeclarationID).Scan(&firingID); err != nil {
		t.Fatalf("POST /v1alpha1/events did not reach the declaration engine: %v", err)
	}
	nodeState := func() string {
		var st string
		if err := s.Pool().QueryRow(ctx, `SELECT state FROM declaration_nodes WHERE namespace_id=$1 AND opening_firing_id=$2`, nsID, firingID).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := nodeState(); st != declengine.NodeStateOpen {
		t.Fatalf("landing node %q, want open", st)
	}

	// Flip back: FreezeHook runs in the flip's transaction.
	if rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declaration-engine/switch", token, map[string]string{"mode": "before"}, &got); rr.Code != http.StatusOK || got.Previous != declengine.ModeAfter {
		t.Fatalf("flip to before: %d %+v", rr.Code, got)
	}
	if st := nodeState(); st != declengine.NodeStateFrozen {
		t.Fatalf("landing node after the flip back = %q, want frozen", st)
	}

	// The reaction arrives while frozen: stored, not evaluated.
	var nonce, mac string
	if err := s.Pool().QueryRow(ctx, `SELECT nonce,mac FROM declaration_minted_markers WHERE namespace_id=$1 AND firing_id=$2`, nsID, firingID).Scan(&nonce, &mac); err != nil {
		t.Fatal(err)
	}
	reaction := postEventAccess(t, srv, eventTokenSecret, map[string]any{"name": "pr-upkeep.pr", "payload": map[string]any{"origin": map[string]string{
		"marker": "cn1:" + firingID + ":github.pr:" + nonce + ":" + mac, "artifact_kind": "github.pr", "artifact_id": "artifact-" + firingID}}})
	var evaluations, stored int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2`, nsID, reaction.Event.ID).Scan(&evaluations); err != nil || evaluations != 0 {
		t.Fatalf("'before' evaluated the reaction %d times (err=%v)", evaluations, err)
	}
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_node_frozen_events WHERE namespace_id=$1 AND event_id=$2`, nsID, reaction.Event.ID).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("reaction stored %d times against the frozen node (err=%v), want 1", stored, err)
	}

	// Forward again: ThawAndReplay runs after the flip commits.
	if rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declaration-engine/switch", token, map[string]string{"mode": "after"}, &got); rr.Code != http.StatusOK || got.ReplayError != "" {
		t.Fatalf("flip forward: %d %+v", rr.Code, got)
	}
	if st := nodeState(); st != declengine.NodeStateClosed {
		t.Fatalf("landing node after replay = %q, want closed (consumed)", st)
	}
	var reacted int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3`, nsID, reaction.Event.ID, vb.DeclarationID).Scan(&reacted); err != nil || reacted != 1 {
		t.Fatalf("replayed reaction fired %d times (err=%v), want exactly 1", reacted, err)
	}
	if rr := doAccess(t, srv, http.MethodGet, "/v1alpha1/declaration-engine/switch", "", nil, &got); rr.Code != http.StatusOK || got.Mode != declengine.ModeAfter {
		t.Fatalf("GET switch after the flips: %d %+v", rr.Code, got)
	}
}

// Acceptance 3's refusals: unauthenticated is 401; a bad mode is 400; a
// control plane that does not run the engine refuses 'shadow'/'after' (409)
// -- flipping to 'after' there would drain the graph engine with nothing
// evaluating declarations -- but still accepts 'before'.
func TestDeclarationSwitchRouteRefusals(t *testing.T) {
	srv, _, token, _ := newDeclarationHumanFixture(t)
	var got declSwitchResp
	if rr := doAccess(t, srv, http.MethodGet, "/v1alpha1/declaration-engine/switch", "", nil, &got); rr.Code != http.StatusOK || got.EngineEnabled || got.Mode != declengine.ModeBefore {
		t.Fatalf("GET without an engine: %d %+v", rr.Code, got)
	}
	if rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declaration-engine/switch", "", map[string]string{"mode": "before"}, nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated flip: %d, want 401", rr.Code)
	}
	if rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declaration-engine/switch", token, map[string]string{"mode": "sideways"}, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad mode: %d, want 400", rr.Code)
	}
	for _, mode := range []string{declengine.ModeShadow, declengine.ModeAfter} {
		if rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declaration-engine/switch", token, map[string]string{"mode": mode}, nil); rr.Code != http.StatusConflict {
			t.Fatalf("flip to %s without an engine: %d, want 409: %s", mode, rr.Code, rr.Body.String())
		}
	}
	if rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declaration-engine/switch", token, map[string]string{"mode": "before"}, &got); rr.Code != http.StatusOK || got.Mode != declengine.ModeBefore {
		t.Fatalf("flip to before without an engine: %d %+v", rr.Code, got)
	}
}
