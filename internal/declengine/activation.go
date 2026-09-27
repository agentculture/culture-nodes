// Activation by declaration, with a human root of trust (ADR 0014 §3, spec
// claims c33/h64/c89/h62). An activation declaration is a declaration whose
// action is "activate": when it fires, it names another declaration to make
// active. That authority has to stop somewhere, or it compounds: an agent
// could propose "activate everything" and have it activated by a broader
// human-activated rule, or two agents could activate each other's rules,
// holding standing authority no human granted. The chain is kept exactly
// one level deep:
//
//   - only a human principal's activate call can make an activation
//     declaration active — never a firing dispatched by another declaration,
//     however it was authored or what it matches;
//   - an activation declaration's action may only target an ordinary
//     declaration, refused at publish whenever the target is known statically;
//   - the actor recorded for every activation, deactivation and publish is
//     the authenticated request principal, resolved here and only here —
//     never a value read from a request or declaration body.
//
// Activate and Publish are the two choke points: every path that can make a
// declaration active, or attribute a declaration to an author, is expected
// to go through them rather than calling postgres.Store directly, so the
// root of trust cannot be bypassed by a caller that forgets to check.
package declengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/decl/template"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// ActionKindActivate is the registered action kind (internal/decl/kinds)
// that marks a declaration as an activation declaration.
const ActionKindActivate = "activate"

// PrincipalKind distinguishes a human principal from an agent principal, the
// same distinction internal/api resolves a request to (an Access identity or
// the synthetic administrator credential is human; a registered actor's own
// bearer, api.Principal.isActorBearer, is agent). declengine does not import
// internal/api — that would cycle once a later task wires the API through
// this package — so callers resolve the request principal on their side of
// the API boundary and hand the classification in here already made.
type PrincipalKind string

const (
	PrincipalHuman PrincipalKind = "human"
	PrincipalAgent PrincipalKind = "agent"
)

// ActivationPrincipal is the authenticated caller attempting to activate a
// declaration, or to publish one. Author is what gets recorded — always this
// field, never a value the caller read out of a request or declaration body.
type ActivationPrincipal struct {
	Kind   PrincipalKind
	Author string
}

// ResolveAuthor returns the recorded author/actor for a publish or
// activation: always the authenticated principal (c89, h62). It takes no
// "claimed author" argument on purpose — there is nothing here for a body's
// claim to override, which is the point: PublishInput.ClaimedAuthor and
// similar fields exist only to prove, in tests, that such a claim never
// reaches the record.
func ResolveAuthor(principal ActivationPrincipal) string {
	return principal.Author
}

// IsActivationDeclaration reports whether d's action is "activate": the
// action kind that makes d an activation declaration rather than an
// ordinary one.
func IsActivationDeclaration(d decl.Declaration) bool {
	return d.Action.Kind == ActionKindActivate
}

// activationTargetName is what an activation declaration's action names as
// the declaration to activate. Dynamic is true when the name is rendered
// from a template reference (e.g. "{declaration}") rather than fixed at
// publish time — the case the ADR calls out as the reason a static publish
// check is not enough by itself: an "activate everything" rule's target is
// only known at firing time, so AuthorizeActivation (not ValidatePublish)
// is what has to catch it.
type activationTargetName struct {
	Name    string
	Dynamic bool
}

func activationTarget(d decl.Declaration) (activationTargetName, error) {
	if !IsActivationDeclaration(d) {
		return activationTargetName{}, fmt.Errorf("declengine: declaration %q is not an activation declaration", d.Name)
	}
	var with struct {
		Declaration string `json:"declaration"`
	}
	if len(d.Action.With) > 0 {
		if err := json.Unmarshal(d.Action.With, &with); err != nil {
			return activationTargetName{}, fmt.Errorf("declengine: activation action.with: %w", err)
		}
	}
	if with.Declaration == "" {
		// No fixed name at all: the target is resolved entirely at firing
		// time (e.g. from the triggering declaration.proposed event).
		return activationTargetName{Dynamic: true}, nil
	}
	tpl, err := template.Parse(with.Declaration)
	if err != nil {
		return activationTargetName{}, fmt.Errorf("declengine: activation target template: %w", err)
	}
	if len(tpl.References()) > 0 {
		return activationTargetName{Name: with.Declaration, Dynamic: true}, nil
	}
	return activationTargetName{Name: with.Declaration}, nil
}

// ActivationLookup resolves whether a named declaration's current version is
// itself an activation declaration. ValidatePublish uses it for the static
// half of the one-level-deep check.
type ActivationLookup interface {
	IsActivationDeclaration(ctx context.Context, namespaceID, name string) (bool, error)
}

// StoreActivationLookup adapts a postgres.Store to ActivationLookup by
// reading the candidate target's newest published version. A target with no
// published version yet is reported as "not an activation declaration" —
// there is nothing to refuse on that ground; an unknown-declaration refusal,
// if any, belongs to whatever enforces that the target exists at all.
type StoreActivationLookup struct{ Store *postgres.Store }

func (l StoreActivationLookup) IsActivationDeclaration(ctx context.Context, namespaceID, name string) (bool, error) {
	v, err := l.Store.LatestDeclarationVersion(ctx, namespaceID, name)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	d, err := decl.Parse(v.Body, decl.FormatJSON)
	if err != nil {
		return false, fmt.Errorf("declengine: target declaration %q version %s: %w", name, v.ID, err)
	}
	return IsActivationDeclaration(*d), nil
}

// ValidatePublish is the static half of the one-level-deep root of trust
// (c33, h64): an activation declaration whose action names a FIXED target
// that is itself an activation declaration is refused outright, before any
// version is written. A dynamic target (named at firing time, e.g. an
// "activate everything" rule) cannot be resolved here; AuthorizeActivation
// enforces the same rule when the activation is actually attempted.
func ValidatePublish(ctx context.Context, namespaceID string, d decl.Declaration, lookup ActivationLookup) error {
	if !IsActivationDeclaration(d) {
		return nil
	}
	if lookup == nil {
		return errors.New("declengine: activation lookup is required to validate an activation declaration at publish")
	}
	target, err := activationTarget(d)
	if err != nil {
		return err
	}
	if target.Dynamic || target.Name == "" {
		return nil
	}
	if target.Name == d.Name {
		return fmt.Errorf("declengine: activation declaration %q may not target itself (c33, h64)", d.Name)
	}
	isActivation, err := lookup.IsActivationDeclaration(ctx, namespaceID, target.Name)
	if err != nil {
		return err
	}
	if isActivation {
		return fmt.Errorf("declengine: activation declaration %q may not target %q: an activation declaration may only activate ordinary declarations, never another activation declaration (c33, h64)", d.Name, target.Name)
	}
	return nil
}

// AuthorizeActivation is the runtime half of the one-level-deep root of
// trust (c33, h64), and the single check every activation attempt has to
// pass, whichever path reached it:
//
//   - a direct activate call, human or agent;
//   - a firing dispatched by an already-active activation declaration,
//     which is always treated as an agent principal here — a declaration
//     firing autonomously is never a live human clicking activate, no
//     matter who authored the rule or how broadly its condition matches.
//
// A target that is itself an activation declaration is refused unless the
// principal is human. An agent principal may activate an ordinary
// declaration (the spec: "an agent may self-activate when such a
// declaration allows it") — just never another activation declaration.
func AuthorizeActivation(principal ActivationPrincipal, targetIsActivationDeclaration bool) error {
	if principal.Author == "" {
		return errors.New("declengine: activation principal is required")
	}
	switch principal.Kind {
	case PrincipalHuman, PrincipalAgent:
	default:
		return fmt.Errorf("declengine: unrecognized activation principal kind %q", principal.Kind)
	}
	if targetIsActivationDeclaration && principal.Kind != PrincipalHuman {
		return fmt.Errorf("declengine: only a human principal may activate an activation declaration; refused for %s principal %q (c33, h64: the chain is one level deep)", principal.Kind, principal.Author)
	}
	return nil
}

// Activate is the single choke point for making a declaration version
// active. It enforces AuthorizeActivation against the target's ACTUAL
// action kind — read fresh from the target version's own body, never
// assumed from whoever is calling — before writing anything, and records
// the append-only history row under the resolved principal, never a
// caller-supplied actor string.
func Activate(ctx context.Context, store *postgres.Store, namespaceID, targetVersionID string, principal ActivationPrincipal, supersedesID string) error {
	target, err := store.GetDeclarationVersion(ctx, targetVersionID)
	if err != nil {
		return err
	}
	d, err := decl.Parse(target.Body, decl.FormatJSON)
	if err != nil {
		return fmt.Errorf("declengine: activation target version %s: %w", targetVersionID, err)
	}
	if err := AuthorizeActivation(principal, IsActivationDeclaration(*d)); err != nil {
		return err
	}
	return store.RecordDeclarationActivation(ctx, namespaceID, targetVersionID, "activate", ResolveAuthor(principal), supersedesID)
}

// PublishInput is what Publish needs to parse, validate and store one
// declaration version. ClaimedAuthor is never read by Publish; it exists so
// a test can hand in a forged/body-supplied author and confirm the recorded
// author is Principal.Author regardless (c89, h62).
type PublishInput struct {
	NamespaceID   string
	Body          []byte
	Format        decl.Format
	Principal     ActivationPrincipal
	ClaimedAuthor string
}

// Publish parses and validates a declaration, applies the one-level-deep
// publish-time activation gate (ValidatePublish), and stores the new
// version under the authenticated principal's author — never
// PublishInput.ClaimedAuthor or anything else a request body might have
// claimed (c89, h62).
func Publish(ctx context.Context, store *postgres.Store, lookup ActivationLookup, in PublishInput) (postgres.DeclarationVersion, error) {
	d, err := decl.Parse(in.Body, in.Format)
	if err != nil {
		return postgres.DeclarationVersion{}, err
	}
	if err := ValidatePublish(ctx, in.NamespaceID, *d, lookup); err != nil {
		return postgres.DeclarationVersion{}, err
	}
	canonical, err := d.CanonicalJSON()
	if err != nil {
		return postgres.DeclarationVersion{}, err
	}
	return store.PublishDeclaration(ctx, postgres.PublishDeclarationInput{
		NamespaceID: in.NamespaceID,
		Name:        d.Name,
		Body:        canonical,
		Author:      ResolveAuthor(in.Principal),
	})
}
