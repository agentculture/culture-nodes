package repair

import (
	"encoding/json"

	"github.com/agentculture/culture-nodes/internal/preflight"
)

// hostBlock is the subset of one bridge's advertised host facts this package
// reads — three of the agreed `host` keys the bridges' shared preflight.py
// declares in HOST_KEYS (`default_sandbox_mode`, `dispatch_grants`,
// `toolchains`). Everything else in the block is carried past untouched.
//
// The keys are decoded HERE, in the consumer, on purpose:
// internal/preflight keeps the host block opaque because four backends share
// one protocol and no engine-side struct can describe all four hosts, so a
// package that needs a specific fact reads that fact itself rather than
// pushing a typed host back into the protocol.
//
// A key that is absent yields the zero value and, through Decide, a refusal
// — never an assumption. That direction is the whole discipline of issue
// #18/#63: a surface that echoed a configuration advertised
// `workspace-write` on three hosts whose kernel could not deliver it, and
// every file write was silently lost.
type hostBlock struct {
	DefaultSandboxMode string              `json:"default_sandbox_mode"`
	DispatchGrants     map[string][]string `json:"dispatch_grants"`
	Toolchains         []struct {
		Name       string            `json:"name"`
		UsableIn   []string          `json:"usable_in"`
		UnusableIn map[string]string `json:"unusable_in"`
	} `json:"toolchains"`
}

// LaneFromCapabilities reads a registered actor's `capabilities` document
// into the lane facts a routing decision needs.
//
// Every failure to read one — an absent block, a protocol version this
// control plane does not speak, a malformed document — produces a lane with
// SurfaceAdvertised false, which Decide routes to a human. That is
// deliberate and is the only safe reading: "this bridge told us nothing" and
// "this bridge can do anything" are the same bytes, and treating them as the
// same capability is how a repair gets dispatched into a lane that cannot run
// the suite it is repairing.
//
// The lane still carries its identity in every case, so a refusal can name
// the actor it refused.
func LaneFromCapabilities(actorID, actorKey string, capabilities json.RawMessage) Lane {
	lane := Lane{ActorID: actorID, ActorKey: actorKey}

	surface, ok, err := preflight.ParseSurface(capabilities)
	if err != nil || !ok {
		return lane
	}

	var host hostBlock
	if err := json.Unmarshal(surface.Host, &host); err != nil {
		return lane
	}

	lane.SurfaceAdvertised = true
	lane.Posture = host.DefaultSandboxMode
	// Only the grants the DEFAULT posture carries. A repair dispatch gets
	// the posture a dispatch that names no mode gets, so the grants of some
	// other, more permissive mode are irrelevant to it — reading the union
	// would advertise `danger-full-access`'s network as if a default
	// dispatch had it.
	lane.Grants = append([]string(nil), host.DispatchGrants[host.DefaultSandboxMode]...)
	for _, tc := range host.Toolchains {
		lane.Toolchains = append(lane.Toolchains, Toolchain{
			Name:       tc.Name,
			UsableIn:   tc.UsableIn,
			UnusableIn: tc.UnusableIn,
		})
	}
	return lane
}
