package declengine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
)

func ordinaryTestDecl(name string) decl.Declaration {
	return decl.Declaration{
		Name:        name,
		Trigger:     decl.Trigger{Kind: "pr-upkeep.pr", ReentryLimit: 3, HopLimit: 20, RateCeiling: "30/h"},
		Condition:   "true",
		Action:      decl.Action{Kind: "agent.work", With: json.RawMessage(`{"uses":"actor://test","input":{}}`)},
		StartNode:   decl.Node{Name: "ready", Deadline: "none"},
		LandingNode: decl.Node{Name: "waiting", Deadline: "1h"},
	}
}

func activationTestDecl(name, targetWith string) decl.Declaration {
	return decl.Declaration{
		Name:        name,
		Trigger:     decl.Trigger{Kind: "declaration.proposed", ReentryLimit: 3, HopLimit: 20, RateCeiling: "30/h"},
		Condition:   "true",
		Action:      decl.Action{Kind: ActionKindActivate, With: json.RawMessage(targetWith)},
		StartNode:   decl.Node{Name: "root", Deadline: "none"},
		LandingNode: decl.Node{Name: "activated", Deadline: "none"},
	}
}

func TestIsActivationDeclaration(t *testing.T) {
	if IsActivationDeclaration(ordinaryTestDecl("x")) {
		t.Fatal("an agent.work declaration must not read as an activation declaration")
	}
	if !IsActivationDeclaration(activationTestDecl("rule", `{"declaration":"x"}`)) {
		t.Fatal("an action.kind=activate declaration must read as an activation declaration")
	}
}

// fakeLookup answers ActivationLookup from a fixed map, for tests that do
// not need Postgres.
type fakeLookup map[string]bool

func (f fakeLookup) IsActivationDeclaration(_ context.Context, _, name string) (bool, error) {
	return f[name], nil
}

func TestValidatePublishRefusesFixedActivationTarget(t *testing.T) {
	lookup := fakeLookup{"rule-a": true, "ordinary": false}
	// c33/h64 adversarial case: an activation declaration whose action
	// targets another activation declaration is refused at publish.
	err := ValidatePublish(context.Background(), "ns", activationTestDecl("rule-b", `{"declaration":"rule-a"}`), lookup)
	if err == nil {
		t.Fatal("expected refusal: activation declaration targets another activation declaration")
	}
	if !strings.Contains(err.Error(), "c33") {
		t.Errorf("error should cite the rule: %v", err)
	}
}

func TestValidatePublishAllowsActivationTargetingOrdinary(t *testing.T) {
	lookup := fakeLookup{"rule-a": true, "ordinary": false}
	if err := ValidatePublish(context.Background(), "ns", activationTestDecl("rule-a", `{"declaration":"ordinary"}`), lookup); err != nil {
		t.Fatalf("activation targeting an ordinary declaration must publish: %v", err)
	}
}

func TestValidatePublishSkipsOrdinaryDeclarations(t *testing.T) {
	// An ordinary (non-activation) declaration is never subject to this
	// gate, and needs no lookup at all.
	if err := ValidatePublish(context.Background(), "ns", ordinaryTestDecl("ordinary"), nil); err != nil {
		t.Fatalf("ordinary declaration must publish without a lookup: %v", err)
	}
}

func TestValidatePublishSkipsDynamicTarget(t *testing.T) {
	// An "activate everything" rule's target is resolved from the firing
	// event, not fixed at publish time — ValidatePublish cannot check what
	// it cannot resolve; AuthorizeActivation is the backstop for this case.
	lookup := fakeLookup{}
	d := activationTestDecl("activate-everything", `{"declaration":"{declaration}"}`)
	if err := ValidatePublish(context.Background(), "ns", d, lookup); err != nil {
		t.Fatalf("a template-named target must not be statically refused: %v", err)
	}
	d2 := activationTestDecl("activate-anything", `{}`)
	if err := ValidatePublish(context.Background(), "ns", d2, lookup); err != nil {
		t.Fatalf("an activation declaration with no fixed target must not be statically refused: %v", err)
	}
}

func TestValidatePublishRefusesSelfTarget(t *testing.T) {
	lookup := fakeLookup{}
	err := ValidatePublish(context.Background(), "ns", activationTestDecl("rule", `{"declaration":"rule"}`), lookup)
	if err == nil {
		t.Fatal("an activation declaration must not be able to target itself")
	}
}

func TestValidatePublishRequiresLookupForActivationDeclaration(t *testing.T) {
	err := ValidatePublish(context.Background(), "ns", activationTestDecl("rule", `{"declaration":"ordinary"}`), nil)
	if err == nil {
		t.Fatal("a nil lookup must refuse rather than silently allow an activation declaration to publish")
	}
}

func TestAuthorizeActivationOneLevelDeep(t *testing.T) {
	tests := []struct {
		name          string
		principal     ActivationPrincipal
		targetIsActiv bool
		wantErr       bool
	}{
		{"human activates activation declaration", ActivationPrincipal{Kind: PrincipalHuman, Author: "human:alice"}, true, false},
		{"human activates ordinary declaration", ActivationPrincipal{Kind: PrincipalHuman, Author: "human:alice"}, false, false},
		{"agent activates ordinary declaration (self-activation allowed)", ActivationPrincipal{Kind: PrincipalAgent, Author: "agent:codex-thor"}, false, false},
		{"agent activates activation declaration (refused)", ActivationPrincipal{Kind: PrincipalAgent, Author: "agent:codex-thor"}, true, true},
		{"missing author refused", ActivationPrincipal{Kind: PrincipalHuman, Author: ""}, false, true},
		{"unrecognized kind refused", ActivationPrincipal{Kind: "root", Author: "x"}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := AuthorizeActivation(tt.principal, tt.targetIsActiv)
			if (err != nil) != tt.wantErr {
				t.Fatalf("AuthorizeActivation() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestResolveAuthorIgnoresAnyClaimedValue(t *testing.T) {
	// ResolveAuthor takes no claimed-author input at all: there is nothing
	// for a forged body field to override (c89, h62). This is exercised
	// end-to-end (with an actual forged field alongside the principal) in
	// TestPublishIgnoresForgedAuthorClaim (activation_postgres_test.go).
	author := ResolveAuthor(ActivationPrincipal{Kind: PrincipalHuman, Author: "human:real"})
	if author != "human:real" {
		t.Fatalf("ResolveAuthor() = %q, want the principal's own author", author)
	}
}
