package testslint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestModelEgressIsolation is the executable boundary for issue #81:
// generation is dispatched to a registered agent node; internal/ and cmd/
// never acquire a model credential, SDK, or direct model endpoint.
func TestModelEgressIsolation(t *testing.T) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?:os\.Getenv|os\.LookupEnv)\s*\(\s*["'](?:OPENAI|ANTHROPIC|GEMINI|GOOGLE)_API_KEY["']`),
		regexp.MustCompile(`["']https://(?:api\.(?:openai\.com|anthropic\.com)|generativelanguage\.googleapis\.com)/`),
		regexp.MustCompile(`["']github\.com/[^"']*(?:openai|anthropic|gemini|generative-ai)[^"']*["']`),
	}
	root := repoRoot(t)
	scanned := 0
	for _, packageRoot := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, packageRoot), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			scanned++
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, pattern := range patterns {
				if match := pattern.Find(content); match != nil {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s contains %q: model calls and credentials belong in fleet agents, never the control plane", rel, match)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(fmt.Errorf("scan %s: %w", packageRoot, err))
		}
	}
	if scanned == 0 {
		t.Fatal("model isolation lint scanned no control-plane Go files")
	}
}
