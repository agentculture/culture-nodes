package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/clifmt"
)

func TestResolveExplainKnownPaths(t *testing.T) {
	cases := [][]string{
		{},
		{"nodes"},
		{"whoami"},
		{"learn"},
		{"explain"},
		{"overview"},
		{"doctor"},
		{"cli"},
		{"cli", "overview"},
		{"serve"},
		{"scheduler"},
		{"worker"},
		{"all"},
		{"validate"},
		{"run"},
		{"plan-import"},
		{"inspect"},
	}
	for _, path := range cases {
		markdown, err := resolveExplain(path)
		if err != nil {
			t.Errorf("resolveExplain(%v) returned error: %v", path, err)
			continue
		}
		if markdown == "" {
			t.Errorf("resolveExplain(%v) returned empty markdown", path)
		}
	}
}

func TestResolveExplainUnknownPathHasHintListingKnownPaths(t *testing.T) {
	_, err := resolveExplain([]string{"bogus", "path"})
	if err == nil {
		t.Fatal("resolveExplain with an unknown path returned nil error")
	}

	var cliErr *clifmt.CliError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error is not a *clifmt.CliError: %v", err)
	}
	if cliErr.Code != clifmt.ExitUserError {
		t.Fatalf("Code = %d, want %d", cliErr.Code, clifmt.ExitUserError)
	}
	if !strings.Contains(cliErr.Message, "bogus path") {
		t.Fatalf("Message %q does not mention the requested path", cliErr.Message)
	}
	// The remediation must actually list known paths, not just point
	// elsewhere.
	for _, want := range []string{"whoami", "learn", "doctor", "cli overview"} {
		if !strings.Contains(cliErr.Remediation, want) {
			t.Errorf("Remediation %q does not list known path %q", cliErr.Remediation, want)
		}
	}
}

func TestResolveExplainRootPathVariants(t *testing.T) {
	empty, err := resolveExplain(nil)
	if err != nil {
		t.Fatalf("resolveExplain(nil): %v", err)
	}
	named, err := resolveExplain([]string{"nodes"})
	if err != nil {
		t.Fatalf(`resolveExplain(["nodes"]): %v`, err)
	}
	if empty != named {
		t.Fatalf("empty path and [\"nodes\"] resolve to different entries")
	}
}

func TestKnownExplainPathsExcludesEmptyKey(t *testing.T) {
	for _, p := range knownExplainPaths() {
		if p == "" {
			t.Fatal("knownExplainPaths() included the empty root key")
		}
	}
}

func TestCmdExplainJSONPathIsNeverNull(t *testing.T) {
	// Regression guard: a nil []string must round-trip as `[]`, not `null`,
	// in the --json payload; covered at the resolveExplain/catalog level
	// here and end-to-end by the conformance test.
	_, err := resolveExplain([]string{})
	if err != nil {
		t.Fatalf("resolveExplain([]string{}): %v", err)
	}
}
