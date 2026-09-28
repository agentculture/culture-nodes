package api_test

// Task t38d (#328, owner decision d6): an event delivered through the
// authenticated POST /v1alpha1/events arrives at root unless its origin
// marker verifies -- whatever its payload's `node` names. A verified
// reaction arrives at the landing node its parent firing opened.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	api "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/auth"
	"github.com/agentculture/culture-nodes/internal/declengine"
	pgtest "github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

func TestPostedEventWithoutAVerifiedMarkerArrivesAtRoot(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	nsID := pgtest.MustNamespace(t, s, "decl-start-root").ID
	t.Setenv("TCA_API_START_KEY", strings.Repeat("r", 32))
	sw := declengine.PostgresSwitchStore{Store: s}
	eng, err := declengine.New(declengine.Config{MarkerKeyEnv: "TCA_API_START_KEY"}, declengine.PostgresBackend{Store: s}, declengine.PostgresMarkerStore{Store: s},
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
	if _, err := sw.Flip(ctx, nsID, declengine.ModeAfter, "human:ops", "t38d"); err != nil {
		t.Fatal(err)
	}
	va := publishActiveDecl(t, s, nsID, chainDecl("st-a", declengine.RootNode, "waiting"))
	vb := publishActiveDecl(t, s, nsID, chainDecl("st-b", "waiting", "done"))

	count := func(q, eventID, declarationID string) int {
		t.Helper()
		var n int
		if err := s.Pool().QueryRow(ctx, q, nsID, eventID, declarationID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	firings := func(eventID, declarationID string) int {
		return count(`SELECT count(*) FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3`, eventID, declarationID)
	}
	evaluations := func(eventID, declarationID string) int {
		return count(`SELECT count(*) FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3`, eventID, declarationID)
	}

	first := postEventAccess(t, srv, eventTokenSecret, map[string]any{"name": "pr-upkeep.pr", "payload": map[string]any{}})
	var firingID string
	if err := s.Pool().QueryRow(ctx, `SELECT id FROM declaration_firings WHERE namespace_id=$1 AND event_id=$2 AND declaration_id=$3`, nsID, first.Event.ID, va.DeclarationID).Scan(&firingID); err != nil {
		t.Fatalf("the root event did not fire st-a: %v", err)
	}

	// No marker: the payload names st-b's start node, which is open, and
	// the event still arrives at root -- st-a (root) matches it, st-b does
	// not even see it.
	unmarked := postEventAccess(t, srv, eventTokenSecret, map[string]any{"name": "pr-upkeep.pr", "payload": map[string]any{"node": "waiting"}})
	if n := evaluations(unmarked.Event.ID, vb.DeclarationID); n != 0 {
		t.Fatalf("an unmarked event naming node 'waiting' was evaluated %d times by st-b, want 0", n)
	}
	if n := firings(unmarked.Event.ID, va.DeclarationID); n != 1 {
		t.Fatalf("the unmarked event fired st-a %d times, want 1 (it arrives at root)", n)
	}

	var nonce, mac string
	if err := s.Pool().QueryRow(ctx, `SELECT nonce,mac FROM declaration_minted_markers WHERE namespace_id=$1 AND firing_id=$2`, nsID, firingID).Scan(&nonce, &mac); err != nil {
		t.Fatal(err)
	}
	origin := func(m string) map[string]string {
		return map[string]string{"marker": "cn1:" + firingID + ":github.pr:" + nonce + ":" + m, "artifact_kind": "github.pr", "artifact_id": "artifact-" + firingID}
	}

	// A rejected marker (the MAC altered) is no better than none.
	forged := postEventAccess(t, srv, eventTokenSecret, map[string]any{"name": "pr-upkeep.pr", "payload": map[string]any{"node": "waiting", "origin": origin(strings.Repeat("0", 64))}})
	if n := evaluations(forged.Event.ID, vb.DeclarationID); n != 0 {
		t.Fatalf("an event with a rejected marker naming node 'waiting' was evaluated %d times by st-b, want 0", n)
	}
	if n := evaluations(forged.Event.ID, "origin-marker"); n != 1 {
		t.Fatalf("the rejected marker left %d rejection records, want 1", n)
	}

	// The verified marker from the firing that landed on 'waiting' fires
	// st-b -- from the landing node, not from what its payload names.
	verified := postEventAccess(t, srv, eventTokenSecret, map[string]any{"name": "pr-upkeep.pr", "payload": map[string]any{"node": "somewhere-else", "origin": origin(mac)}})
	if n := firings(verified.Event.ID, vb.DeclarationID); n != 1 {
		t.Fatalf("the verified reaction fired st-b %d times, want 1", n)
	}
	if n := firings(verified.Event.ID, va.DeclarationID); n != 0 {
		t.Fatalf("the verified reaction fired st-a %d times, want 0 (it arrives at 'waiting', not root)", n)
	}
}

// The declaration show and focus routes report start_from (task t38d): the
// CLI's `decl show` and the web's focus view read it from here. A body with
// an unknown start_from key or a kind outside the closed set is refused at
// publish.
func TestDeclarationRoutesReportStartFrom(t *testing.T) {
	srv, _, token, _ := newDeclarationHumanFixture(t)
	withStart := func(name, value string) string {
		src := ordinaryDeclSource(name, "")
		return strings.Replace(src, `"start_node":`, `"start_from":`+value+`,"start_node":`, 1)
	}
	for name, value := range map[string]string{"sf-typed": `{"host":"thor","actor_kind":"codex"}`, "sf-any": `"any"`} {
		if rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations", token, declarationSourceReq{Format: "json", Source: withStart(name, value)}, nil); rr.Code != http.StatusCreated {
			t.Fatalf("publish %s: %d %s", name, rr.Code, rr.Body.String())
		}
	}
	var show struct {
		StartNode string          `json:"start_node"`
		StartFrom json.RawMessage `json:"start_from"`
	}
	if rr := doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/sf-typed", "", nil, &show); rr.Code != http.StatusOK {
		t.Fatalf("show: %d %s", rr.Code, rr.Body.String())
	}
	if show.StartNode != "ready" || string(show.StartFrom) != `{"host":"thor","actor_kind":"codex"}` {
		t.Fatalf("show start = %q / %s, want ready / {host: thor, actor_kind: codex}", show.StartNode, show.StartFrom)
	}
	var focus struct {
		Declarations []struct {
			Name      string          `json:"name"`
			StartFrom json.RawMessage `json:"start_from"`
		} `json:"declarations"`
	}
	if rr := doAccess(t, srv, http.MethodGet, "/v1alpha1/declarations/sf-any/focus", "", nil, &focus); rr.Code != http.StatusOK || len(focus.Declarations) != 1 {
		t.Fatalf("focus: %d %s", rr.Code, rr.Body.String())
	}
	if string(focus.Declarations[0].StartFrom) != `"any"` {
		t.Fatalf("focus start_from = %s, want \"any\"", focus.Declarations[0].StartFrom)
	}
	for _, bad := range []string{`{"host":"thor","label":"x"}`, `{"actor_kind":"gpt"}`, `{"host":""}`, `{}`} {
		if rr := doAccess(t, srv, http.MethodPost, "/v1alpha1/declarations", token, declarationSourceReq{Format: "json", Source: withStart("sf-bad", bad)}, nil); rr.Code == http.StatusCreated {
			t.Fatalf("publish with start_from %s was accepted", bad)
		}
	}
}
