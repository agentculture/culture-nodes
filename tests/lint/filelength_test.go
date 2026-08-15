package testslint

// The handover file-length contract, enforced repo-wide (task t4).
//
// The hard limit is 1000 physical lines, comments included. The design target
// of 300 lines is deliberately advisory: hundreds of existing source files
// exceed it, so treating that target as a gate would turn an incremental
// quality signal into a repository-wide exception list.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const maxSourceFileLines = 1000

var sourceFileExtensions = map[string]bool{
	".c": true, ".cc": true, ".go": true,
	".h": true, ".js": true, ".jsx": true,
	".mjs": true, ".py": true, ".sh": true, ".sql": true,
	".ts": true, ".tsx": true,
}

// countLines counts PHYSICAL lines, including comments and a final line with
// no trailing newline. Comments count on purpose: the limit is about how much
// a reviewer has to hold in their head, and a 1400-line file does not become
// reviewable because 500 of those lines explain the other 900.
func countLines(contents string) int {
	lines := strings.Count(contents, "\n")
	if len(contents) > 0 && contents[len(contents)-1] != '\n' {
		lines++
	}
	return lines
}

// scanOversized returns the over-limit files under root, and how many files it
// actually looked at. The second return value is the point: a scanner that
// examined nothing reports no violations, which is indistinguishable from a
// clean tree unless someone checks. See TestTheFileLengthScannerActuallyScans.
func scanOversized(root string, names []string) (oversized []string, scanned int, err error) {
	files, err := readSourceFiles(root, names, hasExtensionIn(sourceFileExtensions))
	if err != nil {
		return nil, 0, err
	}
	for _, file := range files {
		if lines := countLines(file.contents); lines > maxSourceFileLines {
			oversized = append(oversized, fmt.Sprintf("%s: %d lines", file.rel, lines))
		}
	}
	sort.Strings(oversized)
	return oversized, len(files), nil
}

func TestTrackedSourceFilesStayWithinTheHardLineLimit(t *testing.T) {
	// committedFiles (credentialisolation_test.go) is the package's one
	// `git ls-files` reader: the limit is about tracked source, so the index
	// is the right input, and a second inline copy of the same command was
	// how the two guards could drift over what "tracked" means.
	root := repoRoot(t)
	oversized, scanned, err := scanOversized(root, committedFiles(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatalf("scanned no source files under %s; this gate would pass on any tree", root)
	}
	if len(oversized) > 0 {
		t.Fatalf("tracked source files exceed the %d-line hard limit (comments count):\n  %s",
			maxSourceFileLines, strings.Join(oversized, "\n  "))
	}
}

// TestTheFileLengthScannerActuallyScans is the gate on the gate.
//
// The repo-wide test above passes when the tree is clean AND when the scanner
// is broken -- a wrong root, an empty extension map, an off-by-one on the
// threshold all produce the same silent green. Every one of those is a change
// somebody could make while "tidying" this file. So plant files that must be
// caught and files that must not be, and check the verdict on each.
func TestTheFileLengthScannerActuallyScans(t *testing.T) {
	root := t.TempDir()
	write := func(name string, lines int, trailingNewline bool) {
		t.Helper()
		body := strings.Repeat("x\n", lines)
		if !trailingNewline && lines > 0 {
			body = strings.TrimSuffix(body, "\n")
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("over.py", maxSourceFileLines+1, true)
	write("exactly_at_limit.go", maxSourceFileLines, true)
	// One line over, with no trailing newline -- the case countLines exists for.
	write("over_no_final_newline.ts", maxSourceFileLines+1, false)
	// Not a source extension: long, and deliberately ignored.
	write("huge.md", maxSourceFileLines*2, true)

	names := []string{"over.py", "exactly_at_limit.go", "over_no_final_newline.ts", "huge.md"}
	oversized, scanned, err := scanOversized(root, names)
	if err != nil {
		t.Fatal(err)
	}
	if scanned != 3 {
		t.Errorf("scanned %d files, want 3 (the .md must be skipped, the other three counted)", scanned)
	}

	got := strings.Join(oversized, "\n")
	for _, want := range []string{"over.py", "over_no_final_newline.ts"} {
		if !strings.Contains(got, want) {
			t.Errorf("scanner missed %s; the gate would not catch a new over-limit file. got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "exactly_at_limit.go") {
		t.Errorf("a file of exactly %d lines was flagged; the limit is inclusive", maxSourceFileLines)
	}
	if strings.Contains(got, "huge.md") {
		t.Errorf("a non-source file was flagged; prose wraps at the reader, which is why MD013 is off too")
	}
}
