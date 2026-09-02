package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

func TestBindAndLookupIdentity(t *testing.T) {
	s := requireStore(t)
	ns := mustNamespace(t, s, "identity-bind")
	actorID := mustActorRow(t, s, ns.ID)
	ctx := context.Background()

	bound, err := s.BindIdentity(ctx, ns.ID, "cloudflare-access", "user-123", actorID, []string{"viewer", "approver"})
	if err != nil {
		t.Fatalf("BindIdentity: %v", err)
	}
	got, err := s.LookupIdentity(ctx, ns.ID, "cloudflare-access", "user-123")
	if err != nil {
		t.Fatalf("LookupIdentity: %v", err)
	}
	if got.ID != bound.ID || got.NamespaceID != ns.ID || got.Provider != "cloudflare-access" || got.Subject != "user-123" || got.ActorID != actorID {
		t.Errorf("LookupIdentity = %#v, want binding %#v", got, bound)
	}
	if len(got.Roles) != 2 || got.Roles[0] != "viewer" || got.Roles[1] != "approver" {
		t.Errorf("roles = %#v, want [viewer approver]", got.Roles)
	}
}

func TestBindIdentityRejectsDuplicateLiveBinding(t *testing.T) {
	s := requireStore(t)
	ns := mustNamespace(t, s, "identity-duplicate")
	actorID := mustActorRow(t, s, ns.ID)
	ctx := context.Background()

	if _, err := s.BindIdentity(ctx, ns.ID, "cloudflare-access", "user-123", actorID, nil); err != nil {
		t.Fatalf("first BindIdentity: %v", err)
	}
	if _, err := s.BindIdentity(ctx, ns.ID, "cloudflare-access", "user-123", actorID, nil); !errors.Is(err, postgres.ErrIdentityAlreadyBound) {
		t.Fatalf("second BindIdentity error = %v, want ErrIdentityAlreadyBound", err)
	}
}

func TestRevokeIdentityHidesBinding(t *testing.T) {
	s := requireStore(t)
	ns := mustNamespace(t, s, "identity-revoke")
	actorID := mustActorRow(t, s, ns.ID)
	ctx := context.Background()

	bound, err := s.BindIdentity(ctx, ns.ID, "cloudflare-service-token", "service-123", actorID, []string{"viewer"})
	if err != nil {
		t.Fatalf("BindIdentity: %v", err)
	}
	if err := s.RevokeIdentity(ctx, bound.ID); err != nil {
		t.Fatalf("RevokeIdentity: %v", err)
	}
	if _, err := s.LookupIdentity(ctx, ns.ID, bound.Provider, bound.Subject); !errors.Is(err, postgres.ErrIdentityNotFound) {
		t.Fatalf("LookupIdentity after revoke error = %v, want ErrIdentityNotFound", err)
	}
}

func TestBindIdentityAppendsAfterRevoke(t *testing.T) {
	s := requireStore(t)
	ns := mustNamespace(t, s, "identity-rebind")
	firstActor := mustActorRow(t, s, ns.ID)
	secondActor := mustActorRow(t, s, ns.ID)
	ctx := context.Background()

	first, err := s.BindIdentity(ctx, ns.ID, "cloudflare-access", "user-123", firstActor, []string{"viewer"})
	if err != nil {
		t.Fatalf("first BindIdentity: %v", err)
	}
	if err := s.RevokeIdentity(ctx, first.ID); err != nil {
		t.Fatalf("RevokeIdentity: %v", err)
	}
	second, err := s.BindIdentity(ctx, ns.ID, "cloudflare-access", "user-123", secondActor, []string{"approver"})
	if err != nil {
		t.Fatalf("second BindIdentity: %v", err)
	}
	if second.ID == first.ID {
		t.Error("rebind reused the revoked row; want an appended binding")
	}
	if second.ActorID != secondActor {
		t.Errorf("rebound actor = %q, want %q", second.ActorID, secondActor)
	}
}

func TestBindIdentityRejectsInvalidRole(t *testing.T) {
	s := requireStore(t)
	ns := mustNamespace(t, s, "identity-role")
	actorID := mustActorRow(t, s, ns.ID)

	if _, err := s.BindIdentity(context.Background(), ns.ID, "cloudflare-access", "user-123", actorID, []string{"administrator"}); err == nil {
		t.Fatal("BindIdentity with an invalid role succeeded")
	}
}
