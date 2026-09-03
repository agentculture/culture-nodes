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
	got := buildMesh([]meshActorRow{{key: "unobserved"}, {key: "unsupported"}, {key: "failed"}}, nil, "v", observations)
	if got.Actors[0].Bridge.Class != "unobserved" || got.Actors[0].Bridge.Error != "not observed by the bridge collector" {
		t.Fatalf("unobserved actor = %#v", got.Actors[0])
	}
	if got.Actors[1].Bridge.Reason != "GET capabilities: 404 Not Found" || got.Actors[2].Bridge.FailureCount != 3 {
		t.Fatalf("unsupported/failed actors = %#v / %#v", got.Actors[1], got.Actors[2])
	}
}
