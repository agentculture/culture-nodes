package declengine

import (
	"context"
	"os"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

var markerTestStore *postgres.Store

func TestMain(m *testing.M) {
	os.Exit(pgtest.Run(m, func(s *postgres.Store) { markerTestStore = s }))
}

func TestPostgresMarkerBindingAndRejection(t *testing.T) {
	pgtest.RequireStore(t, markerTestStore)
	ctx := context.Background()
	ns, err := markerTestStore.CreateNamespace(ctx, "marker-"+store.NewULID(), "Marker test")
	if err != nil {
		t.Fatal(err)
	}
	event, err := markerTestStore.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{NamespaceID: ns.ID, Name: "test.marker", Emitter: "test"})
	if err != nil {
		t.Fatal(err)
	}
	// A firing must name a published version of a catalog declaration (0064).
	first := active("first").Declaration
	body, err := first.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	version, err := markerTestStore.PublishDeclaration(ctx, postgres.PublishDeclarationInput{NamespaceID: ns.ID, Name: "first", Body: body, Author: "test"})
	if err != nil {
		t.Fatal(err)
	}
	firing, _, err := markerTestStore.RecordDeclarationFiring(ctx, postgres.DeclarationFiringInput{
		NamespaceID: ns.ID, EventID: event.Event.ID, DeclarationID: version.DeclarationID, DeclarationVersion: version.ID,
		TriggerDigest: "trigger", ConditionDigest: "condition", ActionDigest: "action",
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewMarkerService([]byte("test control plane secret with sufficient length"), PostgresMarkerStore{markerTestStore})
	if err != nil {
		t.Fatal(err)
	}
	marker, err := m.Mint(ctx, ns.ID, firing.ID, "github.pr")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RecordArtifact(ctx, ns.ID, marker, "42"); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordArtifact(ctx, ns.ID, marker, "43"); err == nil {
		t.Fatal("artifact binding was overwritten")
	}
	origin := OriginEvent{NamespaceID: ns.ID, EventID: event.Event.ID, Marker: marker, ArtifactKind: "github.pr", ArtifactID: "42", Author: "bridge", BridgeAccount: "bridge"}
	parent, err := m.Resolve(ctx, origin)
	if err != nil || parent != firing.ID {
		t.Fatalf("parent=%q err=%v", parent, err)
	}
	origin.ArtifactID = "43"
	parent, err = m.Resolve(ctx, origin)
	if err != nil || parent != "" {
		t.Fatalf("copied marker inherited parent=%q err=%v", parent, err)
	}
	var outcome string
	if err := markerTestStore.Pool().QueryRow(ctx, `SELECT outcome FROM declaration_evaluations WHERE namespace_id=$1 AND event_id=$2 AND declaration_id='origin-marker'`, ns.ID, event.Event.ID).Scan(&outcome); err != nil || outcome != "marker rejected" {
		t.Fatalf("rejection outcome=%q err=%v", outcome, err)
	}
}
