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
