package api

import (
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/mesh"
)

func TestBuildMeshPreservesProbeClasses(t *testing.T) {
	now := time.Now().UTC()
	observations := map[string]mesh.Observation{
		"unsupported": {ObservedAt: now, Class: "unsupported", Reason: "GET capabilities: 404 Not Found", Error: "GET capabilities: 404 Not Found"},
		"failed":      {ObservedAt: now, Class: "failed", Error: "connection refused", FailureCount: 3},
	}
	got := buildMesh([]meshActorRow{{id: "a1", key: "unobserved"}, {id: "a2", key: "unsupported"}, {id: "a3", key: "failed"}}, nil, "v", observations)
	if got.Actors[0].Bridge.Class != "unobserved" || got.Actors[0].Bridge.Error != "not observed by the bridge collector" {
		t.Fatalf("unobserved actor = %#v", got.Actors[0])
	}
	if got.Actors[1].Bridge.Reason != "GET capabilities: 404 Not Found" || got.Actors[2].Bridge.FailureCount != 3 {
		t.Fatalf("unsupported/failed actors = %#v / %#v", got.Actors[1], got.Actors[2])
	}
	if got.Actors[0].ID != "a1" {
		t.Fatalf("actor row id = %q, want the actors-table id run attribution joins on", got.Actors[0].ID)
	}
}
