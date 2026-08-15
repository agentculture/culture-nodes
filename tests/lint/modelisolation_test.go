package testslint

import (
	"regexp"
	"testing"
)

// modelEgressPatterns are the three shapes an escape would take: acquiring a
// model credential, naming a model endpoint, or importing a model SDK.
var modelEgressPatterns = []scanPattern{
	{
		name:    "model credential",
		pattern: regexp.MustCompile(`(?:os\.Getenv|os\.LookupEnv)\s*\(\s*["'](?:OPENAI|ANTHROPIC|GEMINI|GOOGLE)_API_KEY["']`),
	},
	{
		name:    "model endpoint",
		pattern: regexp.MustCompile(`["']https://(?:api\.(?:openai\.com|anthropic\.com)|generativelanguage\.googleapis\.com)/`),
	},
	{
		name:    "model SDK import",
		pattern: regexp.MustCompile(`["']github\.com/[^"']*(?:openai|anthropic|gemini|generative-ai)[^"']*["']`),
	},
}

// TestModelEgressIsolation is the executable boundary for issue #81:
// generation is dispatched to a registered agent node; internal/ and cmd/
// never acquire a model credential, SDK, or direct model endpoint.
func TestModelEgressIsolation(t *testing.T) {
	root := repoRoot(t)
	files := mustReadSourceFiles(t, root, treePaths(t, root, "internal", "cmd"), isGoSourceNotTest)
	if len(files) == 0 {
		t.Fatal("model isolation lint scanned no control-plane Go files")
	}
	for _, finding := range scanFiles(files, modelEgressPatterns, nil) {
		t.Errorf("%s:%d contains %q (%s): model calls and credentials belong in fleet agents, never the control plane",
			finding.file, finding.line, finding.match, finding.name)
	}
}
