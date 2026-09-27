package declengine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// publishOrdinary publishes an ordinary declaration under a human author and
// returns its version.
func publishOrdinary(t *testing.T, db *postgres.Store, ns, name string) postgres.DeclarationVersion {
	t.Helper()
	v, err := Publish(context.Background(), db, StoreActivationLookup{Store: db}, PublishInput{
		NamespaceID: ns, Body: mustCanonical(t, ordinaryTestDecl(name)), Format: decl.FormatJSON,
		Principal: ActivationPrincipal{Kind: PrincipalHuman, Author: "human:publisher"},
	})
	if err != nil {
		t.Fatalf("publish ordinary %q: %v", name, err)
	}
	return v
}

// publishActivationRule publishes an activation declaration whose action
// targets the named declaration (fixed, not templated).
func publishActivationRule(t *testing.T, db *postgres.Store, ns, name, target string, principal ActivationPrincipal) (postgres.DeclarationVersion, error) {
	t.Helper()
	with, err := json.Marshal(map[string]string{"declaration": target})
	if err != nil {
		t.Fatal(err)
	}
	return Publish(context.Background(), db, StoreActivationLookup{Store: db}, PublishInput{
		NamespaceID: ns, Body: mustCanonical(t, activationTestDecl(name, string(with))), Format: decl.FormatJSON,
		Principal: principal,
	})
}

func mustCanonical(t *testing.T, d decl.Declaration) []byte {
	t.Helper()
	body, err := d.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestPublishRefusesActivationDeclarationTargetingActivationDeclaration(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "activation-publish-refuse").ID

	publishOrdinary(t, db, ns, "ordinary")
	ruleA, err := publishActivationRule(t, db, ns, "rule-a", "ordinary", ActivationPrincipal{Kind: PrincipalHuman, Author: "human:alice"})
	if err != nil {
		t.Fatalf("rule-a targeting an ordinary declaration must publish: %v", err)
	}

	// Adversarial case: an activation declaration whose action targets
	// another activation declaration is refused at publish (c33, h64).
	_, err = publishActivationRule(t, db, ns, "rule-b", "rule-a", ActivationPrincipal{Kind: PrincipalHuman, Author: "human:alice"})
	if err == nil {
		t.Fatal("expected publish to refuse an activation declaration targeting another activation declaration")
	}
	if !strings.Contains(err.Error(), "c33") {
		t.Errorf("error should cite the rule: %v", err)
	}

	// The refused publish must never have reached the store: rule-b has no
	// published version at all.
	if _, err := db.LatestDeclarationVersion(ctx, ns, "rule-b"); err != postgres.ErrNotFound {
		t.Fatalf("rule-b must not exist after a refused publish, got err=%v", err)
	}
	_ = ruleA
}

func TestActivateOnlyHumanActivatesActivationDeclaration(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "activation-human-root").ID

	publishOrdinary(t, db, ns, "ordinary")
	rule, err := publishActivationRule(t, db, ns, "rule", "ordinary", ActivationPrincipal{Kind: PrincipalHuman, Author: "human:alice"})
	if err != nil {
		t.Fatal(err)
	}

	// Adversarial case: an agent-authored (here, agent-CALLED) activation
	// declaration stays inactive even though the call is otherwise
	// well-formed ("activate everything" shape: the agent just asks).
	if err := Activate(ctx, db, ns, rule.ID, ActivationPrincipal{Kind: PrincipalAgent, Author: "agent:codex-thor"}, ""); err == nil {
		t.Fatal("expected an agent principal to be refused activating an activation declaration")
	}
	assertInactive(t, ctx, db, ns, "rule")

	// A human's activate call succeeds.
	if err := Activate(ctx, db, ns, rule.ID, ActivationPrincipal{Kind: PrincipalHuman, Author: "human:alice"}, ""); err != nil {
		t.Fatalf("human activation of an activation declaration must succeed: %v", err)
	}
	assertActive(t, ctx, db, ns, "rule", rule.ID)
}

func TestActivateAgentSelfActivatesOrdinaryDeclaration(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "activation-agent-self").ID

	ordinary := publishOrdinary(t, db, ns, "ordinary")

	// The spec explicitly allows this: "an agent may self-activate when
	// such a declaration allows it" — only an ACTIVATION declaration
	// requires a human.
	if err := Activate(ctx, db, ns, ordinary.ID, ActivationPrincipal{Kind: PrincipalAgent, Author: "agent:codex-thor"}, ""); err != nil {
		t.Fatalf("agent self-activation of an ordinary declaration must succeed: %v", err)
	}
	assertActive(t, ctx, db, ns, "ordinary", ordinary.ID)

	history, err := db.ListDeclarationHistory(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range history {
		if h.TargetVersionID == ordinary.ID && h.Kind == "activate" {
			found = true
			if h.Actor != "agent:codex-thor" {
				t.Errorf("recorded actor = %q, want the resolved agent principal", h.Actor)
			}
		}
	}
	if !found {
		t.Fatal("no activate history row recorded for the ordinary declaration")
	}
}

func TestActivateAgentSelfActivation(t *testing.T) {
	// Adversarial case named explicitly in the brief: an agent tries to
	// activate its OWN activation declaration.
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "activation-self").ID

	publishOrdinary(t, db, ns, "ordinary")
	rule, err := publishActivationRule(t, db, ns, "own-rule", "ordinary", ActivationPrincipal{Kind: PrincipalAgent, Author: "agent:codex-orin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Activate(ctx, db, ns, rule.ID, ActivationPrincipal{Kind: PrincipalAgent, Author: "agent:codex-orin"}, ""); err == nil {
		t.Fatal("an agent must not be able to self-activate its own activation declaration")
	}
	assertInactive(t, ctx, db, ns, "own-rule")
}

func TestActivateTwoAgentsActivatingEachOthersRules(t *testing.T) {
	// Adversarial case named explicitly in the brief and in the spec's own
	// rationale for c33: "two agents could activate each other's rules".
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "activation-mutual").ID

	publishOrdinary(t, db, ns, "ord1")
	publishOrdinary(t, db, ns, "ord2")
	ruleA, err := publishActivationRule(t, db, ns, "rule-agent-a", "ord1", ActivationPrincipal{Kind: PrincipalAgent, Author: "agent:one"})
	if err != nil {
		t.Fatal(err)
	}
	ruleB, err := publishActivationRule(t, db, ns, "rule-agent-b", "ord2", ActivationPrincipal{Kind: PrincipalAgent, Author: "agent:two"})
	if err != nil {
		t.Fatal(err)
	}

	if err := Activate(ctx, db, ns, ruleB.ID, ActivationPrincipal{Kind: PrincipalAgent, Author: "agent:one"}, ""); err == nil {
		t.Fatal("agent:one must not be able to activate agent:two's activation declaration")
	}
	if err := Activate(ctx, db, ns, ruleA.ID, ActivationPrincipal{Kind: PrincipalAgent, Author: "agent:two"}, ""); err == nil {
		t.Fatal("agent:two must not be able to activate agent:one's activation declaration")
	}
	assertInactive(t, ctx, db, ns, "rule-agent-a")
	assertInactive(t, ctx, db, ns, "rule-agent-b")
}

func TestPublishIgnoresForgedAuthorClaim(t *testing.T) {
	db := pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, db, "activation-forged-author").ID

	v, err := Publish(ctx, db, StoreActivationLookup{Store: db}, PublishInput{
		NamespaceID: ns,
		Body:        mustCanonical(t, ordinaryTestDecl("forged-author-target")),
		Format:      decl.FormatJSON,
		Principal:   ActivationPrincipal{Kind: PrincipalHuman, Author: "human:real-alice"},
		// ClaimedAuthor simulates a forged field a request body might carry
		// (h62: "a declaration body claiming a different author is
		// ignored"). Publish never reads it.
		ClaimedAuthor: "human:forged-mallory",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Author != "human:real-alice" {
		t.Fatalf("recorded author = %q, want the authenticated principal, not the claimed one", v.Author)
	}
	stored, err := db.GetDeclarationVersion(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Author != "human:real-alice" {
		t.Fatalf("stored author = %q, want the authenticated principal", stored.Author)
	}
}

func assertInactive(t *testing.T, ctx context.Context, db *postgres.Store, ns, name string) {
	t.Helper()
	active, err := (PostgresBackend{Store: db}).Active(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range active {
		if a.Declaration.Name == name {
			t.Fatalf("declaration %q must not be active", name)
		}
	}
}

func assertActive(t *testing.T, ctx context.Context, db *postgres.Store, ns, name, versionID string) {
	t.Helper()
	active, err := (PostgresBackend{Store: db}).Active(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range active {
		if a.Declaration.Name == name {
			if a.VersionID != versionID {
				t.Fatalf("active version = %q, want %q", a.VersionID, versionID)
			}
			return
		}
	}
	t.Fatalf("declaration %q must be active", name)
}
