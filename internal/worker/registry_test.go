package worker_test

import (
	"context"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// TestDBRegistryActorRowID proves the attribution resolver returns the
// actors-table row id of the highest revision — the identity
// attempts.actor_id records — and refuses unknown references.
func TestDBRegistryActorRowID(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "registry-rowid")
	ctx := context.Background()

	for rev, id := range map[int]string{1: "actor_rowid_rev1", 2: "actor_rowid_rev2"} {
		if _, err := s.Pool().Exec(ctx, `
			INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol, endpoint_ref)
			VALUES ($1, $2, 'company/rowid', $3, 'agent', 'nodes.actor/v1alpha1', 'http://127.0.0.1:1')
		`, id, ns.ID, rev); err != nil {
			t.Fatalf("insert actor rev %d: %v", rev, err)
		}
	}

	r, err := worker.NewDBRegistry(s, ns.ID)
	if err != nil {
		t.Fatalf("NewDBRegistry: %v", err)
	}

	id, err := r.ActorRowID(ctx, "actor://company/rowid")
	if err != nil {
		t.Fatalf("ActorRowID: %v", err)
	}
	if id != "actor_rowid_rev2" {
		t.Fatalf("ActorRowID = %q, want the highest revision's row id", id)
	}

	if _, err := r.ActorRowID(ctx, "actor://company/absent"); err == nil {
		t.Fatal("ActorRowID(absent) = nil error, want ErrUnknownActor")
	}
}

// TestDBRegistryResolvesCurrentDialInWithoutAddress is the database half of
// t23's defining property. It requires PostgreSQL and is skipped by pgtest in
// socket-denied sandboxes.
func TestDBRegistryResolvesCurrentDialInWithoutAddress(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "registry-dialin")
	ctx := context.Background()
	if _, err := s.Pool().Exec(ctx, `INSERT INTO actors
		(id,namespace_id,actor_key,revision,kind,protocol,endpoint_ref)
		VALUES('actor_dialin_no_address',$1,'company/dialin',1,'agent','nodes.actor/v1alpha1',NULL)`, ns.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchInboundActor(ctx, ns.ID, "company/dialin"); err != nil {
		t.Fatal(err)
	}
	r, err := worker.NewDBRegistry(s, ns.ID)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := r.Resolve(ctx, "actor://company/dialin")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.URL != "" || endpoint.DialIn == nil || endpoint.DialInActorKey != "company/dialin" {
		t.Fatalf("endpoint = %+v, want addressless dial-in", endpoint)
	}
}
