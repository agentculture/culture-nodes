package repair_test

import (
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/repair"
)

// thorSurface is the shape a codex bridge really advertises, trimmed to the
// keys a routing decision reads. The values are the ones
// docs/baselines/dispatch-posture.md measured on thor.
const thorSurface = `{
  "preflight": {
    "protocol_version": "1.0",
    "host": {
      "hostname": "thor",
      "sandbox_modes": ["workspace-write", "danger-full-access"],
      "default_sandbox_mode": "workspace-write",
      "dispatch_grants": {
        "workspace-write": ["workspace-write", "tmp-write"],
        "danger-full-access": ["workspace-write", "tmp-write", "home-write", "network-egress"]
      },
      "toolchains": [
        {"name": "go", "state": "present", "usable_in": ["workspace-write"],
         "unusable_in": {"read-only": "nothing is writable in this mode"}},
        {"name": "gh", "state": "present", "usable_in": [],
         "unusable_in": {"workspace-write": "a dispatched session has no network egress in this mode"}}
      ]
    }
  }
}`

func TestLaneReadsThePostureAndItsGrants(t *testing.T) {
	lane := repair.LaneFromCapabilities("act_1", "company/codex-thor", json.RawMessage(thorSurface))

	if !lane.SurfaceAdvertised {
		t.Fatal("SurfaceAdvertised = false, want true")
	}
	if lane.Posture != "workspace-write" {
		t.Fatalf("posture = %q, want the advertised default_sandbox_mode", lane.Posture)
	}
	if len(lane.Grants) != 2 {
		t.Fatalf("grants = %v, want the two the default posture grants", lane.Grants)
	}
	if len(lane.Toolchains) != 2 {
		t.Fatalf("toolchains = %v, want both advertised tools", lane.Toolchains)
	}
	if lane.ActorID != "act_1" || lane.ActorKey != "company/codex-thor" {
		t.Fatalf("lane identity = %+v", lane)
	}
}

// The two facts together: `go` runs on thor under workspace-write, and `gh`
// does not — so a gate spelled with each routes differently off one surface.
func TestLaneFromARealSurfaceRoutesGoAndGhDifferently(t *testing.T) {
	lane := repair.LaneFromCapabilities("act_1", "company/codex-thor", json.RawMessage(thorSurface))
	in := baseInput(baseNow())
	in.Lane = lane

	in.Gate.Command = []string{"go", "test", "./..."}
	if got := repair.Decide(in); got.Destination != repair.DestinationRepair {
		t.Fatalf("go: destination = %q, want %q (%s)", got.Destination, repair.DestinationRepair, got.Narrative)
	}

	in.Gate.Command = []string{"gh", "pr", "checks"}
	got := repair.Decide(in)
	if got.Reason != repair.ReasonLaneCannotVerify {
		t.Fatalf("gh: reason = %q, want %q", got.Reason, repair.ReasonLaneCannotVerify)
	}
}

func TestLaneFromNoCapabilitiesAdvertisesNothing(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":            ``,
		"empty object":     `{}`,
		"no preflight key": `{"something_else": {}}`,
		"wrong version":    `{"preflight":{"protocol_version":"0.9","host":{"hostname":"thor"}}}`,
		"malformed":        `{"preflight": "not an object"}`,
	} {
		lane := repair.LaneFromCapabilities("act_1", "k", json.RawMessage(raw))
		if lane.SurfaceAdvertised {
			t.Errorf("%s: SurfaceAdvertised = true, want false", name)
		}
		if lane.ActorID != "act_1" {
			t.Errorf("%s: the lane still names its actor even with no surface", name)
		}
	}
}

// A surface that advertises a default posture its own dispatch_grants block
// says nothing about grants nothing. Reading that as "unconstrained" is the
// #18/#63 failure — a config echoed back as a capability.
func TestAPostureWithNoAdvertisedGrantsGrantsNothing(t *testing.T) {
	raw := `{"preflight":{"protocol_version":"1.0","host":{
		"hostname":"orin","default_sandbox_mode":"read-only",
		"dispatch_grants":{"workspace-write":["workspace-write"]}}}}`

	lane := repair.LaneFromCapabilities("act_1", "company/codex-orin", json.RawMessage(raw))
	if !lane.SurfaceAdvertised {
		t.Fatal("SurfaceAdvertised = false, want true — a surface was advertised")
	}
	if len(lane.Grants) != 0 {
		t.Fatalf("grants = %v, want none: the default posture has no dispatch_grants entry", lane.Grants)
	}

	in := baseInput(baseNow())
	in.Lane = lane
	if got := repair.Decide(in); got.Reason != repair.ReasonLaneCannotWrite {
		t.Fatalf("reason = %q, want %q", got.Reason, repair.ReasonLaneCannotWrite)
	}
}
