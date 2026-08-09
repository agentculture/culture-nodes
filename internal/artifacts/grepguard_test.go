package artifacts_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// forbiddenFilesystemPatterns are the literal substrings a pod-local
// filesystem write would use. Their presence anywhere in non-test .go code
// under internal/artifacts is, by itself, a bug: every artifact ref is
// resolved through a Store (Postgres rows or an S3-compatible bucket), and
// task t15's whole premise (spec claim c38) is that no code path here ever
// writes to -- or even names -- a pod's local disk.
var forbiddenFilesystemPatterns = []string{
	"os.WriteFile(",
	"os.Create(",
	"filepath.",
}

// TestNoPodLocalFilesystem greps every non-test .go file under
// internal/artifacts (this package and its postgres/s3/artifactstest
// subpackages) for forbiddenFilesystemPatterns and fails if any are found.
// It is deliberately cheap and textual rather than an AST walk: the point
// is a fast, hard-to-route-around tripwire a future change cannot
// accidentally slip past, not a sophisticated static analysis.
//
// _test.go files are excluded on purpose -- this file itself uses
// path/filepath to do the walk, and internal/artifacts/artifactstest's
// Docker-based test fixtures are legitimately out of scope (they never
// touch artifact content).
func TestNoPodLocalFilesystem(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate internal/artifacts to walk")
	}
	root := filepath.Dir(thisFile) // internal/artifacts

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		content := string(data)

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}

		for _, pattern := range forbiddenFilesystemPatterns {
			if strings.Contains(content, pattern) {
				t.Errorf("%s: contains forbidden pattern %q (no pod-local filesystem artifact storage; artifacts are addressed via a Store, never a path)", rel, pattern)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}
