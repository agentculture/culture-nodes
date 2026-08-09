package devague_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoExecInNonTestSources enforces the package doc's "no execution, no
// store" rule at the only layer that can actually catch a regression: a
// grep. This package maps devague's already-produced JSON; it must never
// shell out to the devague binary itself. The devague CLI is used only
// offline, from a developer/CI shell (never from Go code, test or otherwise)
// to regenerate testdata/*.json — see testdata/README.md — so scanning only
// non-test sources is deliberate: a hypothetical future fixture-regeneration
// _test.go helper gated behind a build tag would not trip this check, but
// nothing in the package as delivered needs one.
func TestNoExecInNonTestSources(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir(.): %v", err)
	}

	checked := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++

		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		text := string(data)
		if strings.Contains(text, "os/exec") {
			t.Errorf("%s imports/mentions os/exec; internal/devague's non-test sources must never exec the devague CLI (fixtures are pre-generated, see testdata/README.md)", name)
		}
	}

	if checked == 0 {
		t.Fatal("found no non-test .go sources to check; the package layout changed under this test")
	}
}
