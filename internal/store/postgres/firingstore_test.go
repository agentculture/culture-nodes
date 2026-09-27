package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

func TestDeclarationFiringDeduplicatesEventAndPreservesRemintLineage(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-declaration-firing")
	event, err := s.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{
		NamespaceID: ns.ID, Name: "test.firing", Emitter: "test", SourceKey: "test-firing-source", Watermark: []byte(`{"cursor":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, created, err := s.RecordDeclarationFiring(ctx, postgres.DeclarationFiringInput{
		NamespaceID: ns.ID, EventID: event.Event.ID, DeclarationID: "declaration-a", DeclarationVersion: "version-a",
		TriggerDigest: "trigger-v1", ConditionDigest: "condition-v1", ActionDigest: "action-v1",
	})
	if err != nil || !created {
		t.Fatalf("first firing: created=%v err=%v", created, err)
	}
	if first.LineageID == "" || first.ID == "" {
		t.Fatalf("missing firing identity: %+v", first)
	}
	redelivery, err := s.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{
		NamespaceID: ns.ID, Name: "test.firing", Emitter: "test", SourceKey: "test-firing-source", Watermark: []byte(`{"cursor":1}`),
	})
	if err != nil || !redelivery.Duplicate || redelivery.Event.ID != event.Event.ID {
		t.Fatalf("redelivery: %+v err=%v", redelivery, err)
	}
	second, created, err := s.RecordDeclarationFiring(ctx, postgres.DeclarationFiringInput{
		NamespaceID: ns.ID, EventID: redelivery.Event.ID, DeclarationID: "declaration-a", DeclarationVersion: "version-a",
		TriggerDigest: "trigger-v1", ConditionDigest: "condition-v1", ActionDigest: "action-v1",
	})
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("duplicate firing: %+v created=%v err=%v", second, created, err)
	}
	remint, created, err := s.RecordDeclarationFiring(ctx, postgres.DeclarationFiringInput{
		NamespaceID: ns.ID, EventID: event.Event.ID, DeclarationID: "declaration-a", DeclarationVersion: "version-a",
		TriggerDigest: "trigger-v1", ConditionDigest: "condition-v1", ActionDigest: "action-v1", RemintOfID: first.ID,
	})
	if err != nil || !created || remint.ID == first.ID || remint.LineageID != first.LineageID || remint.CanonicalFiringID != first.ID {
		t.Fatalf("remint: %+v created=%v err=%v", remint, created, err)
	}
	childEvent, err := s.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{NamespaceID: ns.ID, Name: "test.child", Emitter: "test"})
	if err != nil {
		t.Fatal(err)
	}
	child, created, err := s.RecordDeclarationFiring(ctx, postgres.DeclarationFiringInput{
		NamespaceID: ns.ID, EventID: childEvent.Event.ID, DeclarationID: "declaration-b", DeclarationVersion: "version-b",
		TriggerDigest: "trigger-v2", ConditionDigest: "condition-v2", ActionDigest: "action-v2", ParentFiringID: remint.ID,
	})
	if err != nil || !created || child.LineageID != first.LineageID {
		t.Fatalf("child: %+v created=%v err=%v", child, created, err)
	}
	var edgeParent string
	if err := s.Pool().QueryRow(ctx, `SELECT parent_firing_id FROM declaration_lineage_edges WHERE child_firing_id=$1`, child.ID).Scan(&edgeParent); err != nil || edgeParent != first.ID {
		t.Fatalf("parent edge=%q err=%v, want canonical %q", edgeParent, err, first.ID)
	}
	var entries int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_firings WHERE namespace_id=$1 AND canonical_firing_id=id`, ns.ID).Scan(&entries); err != nil || entries != 2 {
		t.Fatalf("lineage entries=%d err=%v, want two", entries, err)
	}
}

func TestDeclarationFiringStorageSurfaces(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-declaration-surfaces")
	event, err := s.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{NamespaceID: ns.ID, Name: "test.surfaces", Emitter: "test"})
	if err != nil {
		t.Fatal(err)
	}
	f, _, err := s.RecordDeclarationFiring(ctx, postgres.DeclarationFiringInput{
		NamespaceID: ns.ID, EventID: event.Event.ID, DeclarationID: "declaration-a", DeclarationVersion: "version-a",
		TriggerDigest: "trigger-v1", ConditionDigest: "condition-v1", ActionDigest: "action-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
	_, err = s.Pool().Exec(ctx, `INSERT INTO declaration_nodes(id,namespace_id,opening_firing_id,node_name,state,deadline) VALUES('node-a',$1,$2,'wait','open',$3)`, ns.ID, f.ID, deadline)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Pool().Exec(ctx, `INSERT INTO declaration_node_frozen_events(namespace_id,node_id,event_id,arrival_order) VALUES($1,'node-a',$2,1)`, ns.ID, event.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Pool().Exec(ctx, `INSERT INTO declaration_lineage_edges(namespace_id,parent_firing_id,child_firing_id) VALUES($1,$2,$3)`, ns.ID, f.ID, f.ID)
	if err == nil {
		t.Fatal("self lineage edge accepted")
	}
	_, err = s.Pool().Exec(ctx, `INSERT INTO declaration_minted_markers(id,namespace_id,firing_id,artifact_kind,nonce,mac,artifact_id) VALUES('marker-a',$1,$2,'github.pr','nonce','mac','123')`, ns.ID, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Pool().Exec(ctx, `INSERT INTO declaration_evaluations(id,namespace_id,event_id,declaration_id,declaration_version,outcome,reason,firing_id) VALUES('eval-a',$1,$2,'declaration-a','version-a','fired','matched',$3)`, ns.ID, event.Event.ID, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	var trigger, condition, action, artifact, state, outcome, reason string
	var storedDeadline time.Time
	var frozen int
	err = s.Pool().QueryRow(ctx, `SELECT trigger_digest,condition_digest,action_digest FROM declaration_firings WHERE id=$1`, f.ID).Scan(&trigger, &condition, &action)
	if err != nil || trigger != "trigger-v1" || condition != "condition-v1" || action != "action-v1" {
		t.Fatalf("digests %q %q %q: %v", trigger, condition, action, err)
	}
	err = s.Pool().QueryRow(ctx, `SELECT artifact_id FROM declaration_minted_markers WHERE id='marker-a'`).Scan(&artifact)
	if err != nil || artifact != "123" {
		t.Fatalf("artifact %q: %v", artifact, err)
	}
	err = s.Pool().QueryRow(ctx, `SELECT state,deadline FROM declaration_nodes WHERE id='node-a'`).Scan(&state, &storedDeadline)
	if err != nil || state != "open" || !storedDeadline.Equal(deadline) {
		t.Fatalf("node state %q deadline %v: %v", state, storedDeadline, err)
	}
	err = s.Pool().QueryRow(ctx, `SELECT count(*) FROM declaration_node_frozen_events WHERE node_id='node-a'`).Scan(&frozen)
	if err != nil || frozen != 1 {
		t.Fatalf("frozen events %d: %v", frozen, err)
	}
	err = s.Pool().QueryRow(ctx, `SELECT outcome,reason FROM declaration_evaluations WHERE id='eval-a'`).Scan(&outcome, &reason)
	if err != nil || outcome != "fired" || reason != "matched" {
		t.Fatalf("evaluation %q %q: %v", outcome, reason, err)
	}
}
