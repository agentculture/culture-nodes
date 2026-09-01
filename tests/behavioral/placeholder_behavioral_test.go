//go:build behavioral

// Package testsbehavioral holds the repo's Go behavioral tests: assertions
// that a confirmed claim or acceptance criterion promised, run agent-side by
// the /validate-delivery leg and cited by `devague evidence`. Every file here
// carries the `behavioral` build tag so `go test ./...` skips the package.
// See tests/behavioral/README.md for the convention.
package testsbehavioral

import "testing"

// TestBehavioralConventionPlaceholder proves the plumbing: the folder builds
// under `-tags behavioral` and a tagged test runs and passes. Delete it once
// a real Go behavioral test exists.
func TestBehavioralConventionPlaceholder(t *testing.T) {
	const tag = "behavioral"
	if tag != "behavioral" {
		t.Fatalf("placeholder: expected the behavioral tag, got %q", tag)
	}
}
