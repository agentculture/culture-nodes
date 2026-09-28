// Package decl parses and normalizes one trigger-condition-action declaration.
package decl

import "encoding/json"

// Declaration is the sole authorable automation unit. Nodes are named waiting
// states; their deadlines are explicit, including the value "none".
type Declaration struct {
	Name        string  `json:"name"`
	Trigger     Trigger `json:"trigger"`
	Condition   string  `json:"condition"`
	Action      Action  `json:"action"`
	StartNode   Node    `json:"start_node"`
	LandingNode Node    `json:"landing_node"`
	// Exposes lists the variable references this declaration may render
	// into a wider audience than they came from (task t30b, #328, owner
	// decision d4). Each entry names a reference as templates write it:
	// `name` for the triggering event's own variable, `step:name` for a
	// lineage one (`1:summary`, `jira-intake:reporter`). Listing an entry is
	// the author's request; the widening fires only once the variable's
	// owner approves it (internal/declengine/sensitivity.go). A widening
	// reference absent from the list is blocked without asking anyone.
	// Parse and CanonicalJSON normalize the list (exposure.go), and an empty
	// list is omitted, so declarations without one keep their digests.
	Exposes []string `json:"exposes,omitempty"`
	// StartFrom (task t38d, #328, owner decision d6) replaces StartNode for
	// matching when present: the declaration fires from any open node, or
	// from open nodes whose engine-recorded types match (startfrom.go).
	// StartNode stays required and names the node the declaration is drawn
	// from. Absent (nil), nothing changes, including the digest.
	StartFrom *StartFrom `json:"start_from,omitempty"`
}

type Trigger struct {
	Kind               string          `json:"kind"`
	With               json.RawMessage `json:"with,omitempty"`
	ReentryLimit       int             `json:"reentry_limit"`
	HopLimit           int             `json:"hop_limit"`
	RateCeiling        string          `json:"rate_ceiling"`
	AllowSelfRetrigger bool            `json:"allow_self_retrigger,omitempty"`
	// MaxConcurrentSubject caps how many of this declaration's firings may
	// be in flight (their landing node still open) for one subject key at
	// once (task t10, spec c84/h57) -- the declaration-level mirror of a
	// graph workflow's limits.maxConcurrentSubjectRuns
	// (internal/store/postgres/subjectconcurrency.go). Zero (the default)
	// means no cap: subject concurrency is a no-op unless a declaration
	// opts in, and events that carry no subject are never affected either
	// way.
	MaxConcurrentSubject int `json:"max_concurrent_subject,omitempty"`
}

type Action struct {
	Kind string          `json:"kind"`
	With json.RawMessage `json:"with,omitempty"`
}

type Node struct {
	Name     string `json:"name"`
	Deadline string `json:"deadline"`
}
