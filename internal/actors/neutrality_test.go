package actors_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Provider neutrality (PRD §9.5): "the core engine does not branch on
// provider names. Provider and model details are telemetry metadata reported
// by the adapter."
//
// This test is the mechanical half of that promise. The architectural half is
// that internal/actors speaks one HTTP protocol and internal/worker takes one
// code path for `agent` and `action.http` nodes — but an architecture is only
// as durable as the thing that notices when it stops holding, and a
// provider-specific special case is exactly the kind of change that looks
// harmless in a diff ("just handle this one vendor's quirk") and is
// irreversible by the time there are three of them.
//
// So: grep. It is deliberately textual rather than an AST walk. The point is
// a fast tripwire that is hard to route around by accident, not a
// sophisticated analysis — and a vendor name in a *comment* is just as much a
// sign that the boundary has moved as one in an identifier.
//
// The second half of the neutrality requirement — that the binary links no
// vendor agent SDK — is not enforced here because it is enforced by go.mod:
// the runtime has no such dependency, and adding one would be a visible
// change to go.mod and go.sum in the same review. TestNoAgentSDKDependency
// below states it as an assertion anyway, so the requirement is written down
// somewhere a reader will find it.

// forbiddenProviderPatterns are model-vendor and agent-product names. A match
// anywhere in a non-test .go file under the scanned trees fails this test.
//
// They are matched case-insensitively and on word boundaries, so a name that
// happens to be a substring of an unrelated identifier does not trip the
// guard.
var forbiddenProviderPatterns = []string{
	"colleague",
	"claude",
	"codex",
	"qwen",
	"openai",
	"anthropic",
	"bedrock",
}

// scannedTrees are the packages §9.5's rule binds: the protocol client, the
// dispatcher, the engine, the API surface, and the compiler. Between them
// they are every place a provider name could influence what the control plane
// *does*, as opposed to what it merely reports.
//
// Deliberately not scanned: cmd/ (its `overview` verb names sibling tools in
// the AgentCulture family, which is prose about the ecosystem, not a runtime
// branch) and the repo's own culture.yaml/mesh-agent surface.
var scannedTrees = []string{
	"engine",
	"api",
	"compiler",
	"actors",
	"worker",
}

func TestNoProviderNamesInRuntimeCode(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the internal/ tree to scan")
	}
	internalDir := filepath.Dir(filepath.Dir(thisFile)) // internal/

	patterns := make(map[string]*regexp.Regexp, len(forbiddenProviderPatterns))
	for _, name := range forbiddenProviderPatterns {
		patterns[name] = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\b`)
	}

	scanned := 0
	for _, tree := range scannedTrees {
		root := filepath.Join(internalDir, tree)
		if _, err := os.Stat(root); os.IsNotExist(err) {
			t.Errorf("scanned tree %s does not exist; the neutrality guard is watching nothing", root)
			continue
		}

		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			scanned++

			rel, relErr := filepath.Rel(internalDir, path)
			if relErr != nil {
				rel = path
			}
			for name, pattern := range patterns {
				if loc := pattern.FindIndex(data); loc != nil {
					t.Errorf("%s: names provider %q at byte %d (%s)\n"+
						"PRD §9.5: the core engine does not branch on provider names; "+
						"provider and model details are telemetry metadata reported by the adapter",
						rel, name, loc[0], lineContaining(data, loc[0]))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if scanned == 0 {
		t.Fatal("the neutrality guard scanned no files; it is not proving anything")
	}
	t.Logf("scanned %d non-test .go files across %v for %d provider names",
		scanned, scannedTrees, len(forbiddenProviderPatterns))
}

// TestNoAgentSDKDependency states the second half of §9.5's requirement: the
// control-plane binary links no vendor agent SDK. It reads go.mod rather than
// the build graph because go.mod is where such a dependency would have to
// appear, and a test that reads it says so in a place a reviewer of a
// dependency change will see.
func TestNoAgentSDKDependency(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate go.mod")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // internal/actors -> internal -> repo
	gomod, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	for _, name := range forbiddenProviderPatterns {
		pattern := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\b`)
		if loc := pattern.FindIndex(gomod); loc != nil {
			t.Errorf("go.mod names provider %q (%s): the control plane speaks the §13 HTTP protocol and links no vendor agent SDK",
				name, lineContaining(gomod, loc[0]))
		}
	}
}

// lineContaining returns the source line an offset falls on, trimmed, for a
// failure message that points at the actual text rather than a byte number.
func lineContaining(data []byte, offset int) string {
	start := strings.LastIndexByte(string(data[:offset]), '\n') + 1
	end := strings.IndexByte(string(data[offset:]), '\n')
	if end < 0 {
		end = len(data)
	} else {
		end += offset
	}
	line := strings.TrimSpace(string(data[start:end]))
	const maxLine = 120
	if len(line) > maxLine {
		line = line[:maxLine] + "…"
	}
	return fmt.Sprintf("%q", line)
}
