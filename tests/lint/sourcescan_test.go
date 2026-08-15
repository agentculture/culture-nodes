package testslint

// The shape every text-scanning guard in this package was reimplementing.
//
// Five guards here -- filelength, credentialisolation, modelisolation,
// workspacereaper and schedulerdeadline -- do the same four things: select a
// set of files, read them, look for something in each, and report what they
// found with enough location to act on. Each one had its own inline copy of
// the first three steps, and the nesting of those copies (walk inside loop
// inside filter inside match) is where their cognitive complexity came from.
// The complexity was duplication, not difficulty.
//
// So this file owns the shared middle -- collection and matching -- and
// deliberately owns nothing else. Every guard keeps its own assertions and its
// own failure messages, because the message is the part that carries the
// argument for why the rule exists, and a generic "violation found in file X"
// would throw away exactly the thing worth keeping.
//
// The one guard that does not fit is schedulerdeadline: it reads a Go AST
// rather than lines of text, so its helpers live next to it.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// sourceFile is one file a guard scans: the path a failure message should
// name, and what the file says.
type sourceFile struct {
	rel      string // the name a reader can act on, forward-slash
	contents string
}

// pathFilter selects the files a guard cares about, by repo-relative path.
type pathFilter func(rel string) bool

// hasExtensionIn builds a pathFilter matching a set of lowercase extensions
// (dot included), the form filelength's sourceFileExtensions already uses.
func hasExtensionIn(extensions map[string]bool) pathFilter {
	return func(rel string) bool {
		return extensions[strings.ToLower(filepath.Ext(rel))]
	}
}

// isGoSourceNotTest selects compiled Go source and skips _test.go. The lints
// that use it are about what the shipped control plane does, and a test file
// naming a forbidden endpoint in a fixture is not the control plane reaching
// one.
func isGoSourceNotTest(rel string) bool {
	return strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go")
}

// treePaths lists every file under each of root's subdirs, repo-relative and
// forward-slash. It fails the test on a walk error rather than returning a
// short list: a guard that silently walked nothing reports green over any
// tree, which is the failure mode TestTheFileLengthScannerActuallyScans exists
// to make impossible for its own scanner.
func treePaths(t *testing.T, root string, subdirs ...string) []string {
	t.Helper()
	var rels []string
	for _, subdir := range subdirs {
		walkErr := filepath.WalkDir(filepath.Join(root, subdir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr == nil {
				rels = append(rels, filepath.ToSlash(rel))
			}
			return relErr
		})
		if walkErr != nil {
			t.Fatalf("scan %s: %v", subdir, walkErr)
		}
	}
	return rels
}

// readSourceFiles reads the paths keep selects, resolved against root, in the
// order given. It returns an error rather than failing a test so a caller can
// plant fixtures in a temp dir and assert on the scanner itself.
func readSourceFiles(root string, rels []string, keep pathFilter) ([]sourceFile, error) {
	var files []sourceFile
	for _, rel := range rels {
		if rel == "" || !keep(rel) {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("read source file %s: %w", rel, err)
		}
		files = append(files, sourceFile{rel: rel, contents: string(contents)})
	}
	return files, nil
}

// mustReadSourceFiles is readSourceFiles for the guards that scan the real
// tree, where an unreadable tracked file is a reason to stop rather than a
// case to handle.
func mustReadSourceFiles(t *testing.T, root string, rels []string, keep pathFilter) []sourceFile {
	t.Helper()
	files, err := readSourceFiles(root, rels, keep)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// adapterModuleFiles collects one named module from every adapter package that
// ships it. The rel is `<adapter>/<module>` rather than the full
// adapters/<adapter>/src/<pkg>/<module> path, because that is the form the
// adapter guards' failure messages have always used and it is the form a
// reader recognizes.
func adapterModuleFiles(t *testing.T, modules ...string) []sourceFile {
	t.Helper()
	var files []sourceFile
	for _, pkg := range discoverAdapterPackages(t) {
		for _, module := range modules {
			if !pkg.has(t, module) {
				continue
			}
			files = append(files, sourceFile{
				rel:      pkg.adapter + "/" + module,
				contents: pkg.read(t, module),
			})
		}
	}
	return files
}

// scanPattern is one named tripwire: what to look for, and which matches are
// placeholders rather than violations.
type scanPattern struct {
	name    string
	pattern *regexp.Regexp
	// allow reports whether a raw match is something this pattern
	// deliberately tolerates. nil means every match is a violation.
	allow func(match string) bool
}

// scanFinding is one pattern firing at one line of one file. It carries both
// the matched text and the whole (trimmed) line, because some guards quote the
// match and some quote the line, and which one reads better is the guard's
// call to make.
type scanFinding struct {
	file  string // as given to the scan; "" when scanning a bare string
	line  int    // 1-based, so it pastes into an editor
	name  string // which pattern fired
	match string // the matched text
	text  string // the trimmed source line
}

// lineFilter reports whether one line of one file is scannable. It is built
// fresh per file (see scanFiles) so it may carry state across that file's
// lines -- tracking a Python docstring, for instance.
type lineFilter func(line string) bool

// scanLines applies every pattern to content, line by line, and returns one
// finding per match. Line-by-line rather than whole-file is what makes a
// failure message a `file:line` a reviewer can jump to.
//
// filter, when non-nil, is consulted for every line in order, and lines it
// rejects are not matched against.
func scanLines(file, content string, patterns []scanPattern, filter lineFilter) []scanFinding {
	var findings []scanFinding
	for index, line := range strings.Split(content, "\n") {
		if filter != nil && !filter(line) {
			continue
		}
		findings = append(findings, matchLine(file, index+1, line, patterns)...)
	}
	return findings
}

// matchLine is the innermost loop, split out so scanLines stays flat: every
// match of every pattern on one line, minus the ones a pattern allows.
func matchLine(file string, number int, line string, patterns []scanPattern) []scanFinding {
	var findings []scanFinding
	for _, pattern := range patterns {
		for _, match := range pattern.pattern.FindAllString(line, -1) {
			if pattern.allow != nil && pattern.allow(match) {
				continue
			}
			findings = append(findings, scanFinding{
				file:  file,
				line:  number,
				name:  pattern.name,
				match: match,
				text:  strings.TrimSpace(line),
			})
		}
	}
	return findings
}

// scanFiles applies patterns to every file, tagging each finding with the file
// it came from. newFilter, when non-nil, is called once per file, so a
// stateful filter never leaks one file's state into the next.
func scanFiles(files []sourceFile, patterns []scanPattern, newFilter func() lineFilter) []scanFinding {
	var findings []scanFinding
	for _, file := range files {
		var filter lineFilter
		if newFilter != nil {
			filter = newFilter()
		}
		findings = append(findings, scanLines(file.rel, file.contents, patterns, filter)...)
	}
	return findings
}

// pythonCodeLines hides Python prose -- `#` comments and anything inside a
// triple-quoted docstring -- from a scan.
//
// This is not a convenience. The reaper's module docstrings quote git's own
// `--force` refusal verbatim, and its comments explain why the flag is absent;
// a scan that could not tell prose from code would fire on the very sentences
// that document the rule. Prose may name the thing; code may not.
func pythonCodeLines() lineFilter {
	inDocstring := false
	return func(line string) bool {
		trimmed := strings.TrimSpace(line)
		prose := inDocstring || strings.HasPrefix(trimmed, "#") || strings.Contains(line, `"""`)
		if strings.Count(line, `"""`)%2 == 1 {
			inDocstring = !inDocstring
		}
		return !prose
	}
}
