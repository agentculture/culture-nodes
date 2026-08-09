// Package testslint holds lint-as-Go-test checks that are cheaper to run as
// part of `go test ./...` than to stand up as a separate tool -- the same
// rationale internal/actors/neutrality_test.go documents for the provider-
// neutrality guard: a fast tripwire enforced by `go test`, not a
// sophisticated static-analysis pass.
package testslint

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// sanctionedAWSPackageDirs are the only repo-relative package directories
// (forward-slash, relative to the repo root) permitted to import the AWS
// SDK for Go v2 (spec claim c17: "AWS code stays in AWS-specific
// packages").
//
//   - internal/queue/sqs -- the SQS internal/queue.Queue driver.
//   - internal/artifacts/s3 -- the S3-compatible internal/artifacts.Store
//     driver. It talks to S3 via minio-go, not the AWS SDK, today -- it is
//     sanctioned anyway, as a standing boundary for the day it needs the SDK
//     directly, per its own doc comment's note for this task.
//   - internal/runners/lambda -- the Lambda runners.Runner adapter.
//   - internal/awsauth -- the shared IRSA-ready credential-chain resolver
//     (task t17) every sanctioned driver/adapter above is expected to route
//     through, so that AssumeRole/AssumeRoleWithWebIdentity/profile/static-
//     key/ambient resolution lives in exactly one place.
//
// Every other package must reach AWS only indirectly, through one of these
// four -- an isolation boundary that keeps the blast radius of "which code
// paths can talk to AWS, and with which credentials" enumerable rather than
// grep-shaped.
var sanctionedAWSPackageDirs = map[string]bool{
	"internal/queue/sqs":      true,
	"internal/artifacts/s3":   true,
	"internal/runners/lambda": true,
	"internal/awsauth":        true,
}

// awsSDKModulePrefix is the import-path prefix every AWS SDK for Go v2
// package shares -- "github.com/aws/aws-sdk-go-v2" itself and everything
// under it (aws, config, credentials/..., service/...). Deliberately does
// NOT match "github.com/aws/smithy-go", a separate module the Lambda
// adapter and internal/awsauth both use for protocol-agnostic error typing;
// c17's rule is about the AWS SDK specifically, and smithy-go carries no AWS
// credentials or service calls of its own.
const awsSDKModulePrefix = "github.com/aws/aws-sdk-go-v2"

// TestAWSSDKImportsAreIsolated walks every non-test .go file in the repo
// (skipping web/ -- the React/Vite UI, not Go -- and this test's own
// directory) and fails if any file outside sanctionedAWSPackageDirs imports
// the AWS SDK.
//
// Imports are read with go/parser (parser.ImportsOnly, so this stays cheap
// even over the whole tree) rather than grepped for, so a match is a real
// import declaration -- not a comment, a string literal, or a substring of
// an unrelated import path -- and a renamed or dot-imported AWS SDK package
// is still caught, since the check is on the import path string, not on how
// (or whether) the file refers to the imported identifier afterward.
func TestAWSSDKImportsAreIsolated(t *testing.T) {
	repoRoot := repoRoot(t)
	fset := token.NewFileSet()

	scanned := 0
	violations := 0

	walkErr := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			return skipDirDecision(rel, d.Name())
		}
		return checkGoFileForAWSImports(t, fset, path, rel, &scanned, &violations)
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", repoRoot, walkErr)
	}

	if scanned == 0 {
		t.Fatal("the AWS SDK isolation lint scanned no files; it is not proving anything")
	}
	t.Logf("scanned %d non-test .go files outside %s for %q imports, found %d violation(s)",
		scanned, sortedSanctionedDirs(), awsSDKModulePrefix, violations)
}

// checkGoFileForAWSImports applies the isolation rule to one regular file:
// non-test .go files outside the sanctioned package dirs must not import the
// AWS SDK. It bumps scanned/violations through the caller's counters.
func checkGoFileForAWSImports(t *testing.T, fset *token.FileSet, path, rel string, scanned, violations *int) error {
	t.Helper()
	if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
		return nil
	}
	*scanned++

	dir := filepath.ToSlash(filepath.Dir(rel))
	if sanctionedAWSPackageDirs[dir] {
		return nil
	}

	awsImports, checkErr := awsSDKImportsIn(fset, path, rel)
	if checkErr != nil {
		return checkErr
	}
	for _, importPath := range awsImports {
		*violations++
		t.Errorf("%s: imports %s, but only %s may import the AWS SDK directly (spec claim c17) -- "+
			"route through internal/awsauth and one of the sanctioned drivers/adapters instead",
			rel, importPath, sortedSanctionedDirs())
	}
	return nil
}

// TestSanctionedPackagesActuallyExist proves sanctionedAWSPackageDirs is not
// quietly stale -- every directory it names must exist as a real package
// directory in the repo, so a rename or removal that forgets to update this
// list fails loudly here instead of the allowlist silently protecting
// nothing.
func TestSanctionedPackagesActuallyExist(t *testing.T) {
	repoRoot := repoRoot(t)
	for dir := range sanctionedAWSPackageDirs {
		full := filepath.Join(repoRoot, filepath.FromSlash(dir))
		info, err := os.Stat(full)
		if err != nil {
			t.Errorf("sanctioned AWS package dir %q: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("sanctioned AWS package dir %q is not a directory", dir)
		}
	}
}

// skipDirDecision reports how filepath.WalkDir should handle the directory
// at rel (repo-relative, forward-slash) named name: skip it entirely
// (a dotfile/dir, the web/ UI tree, or this test's own package) or descend
// into it.
func skipDirDecision(rel, name string) error {
	switch {
	case rel == ".":
		return nil
	case strings.HasPrefix(name, "."):
		// .git (a file in a worktree, but a directory in a primary
		// checkout), .github, .claude, .devague, .eidetic: tooling
		// and metadata, never Go source this lint is about.
		return filepath.SkipDir
	case rel == "web":
		// The React/Vite UI (PRD's Go control plane + React/Vite UI
		// split) -- no Go source lives here.
		return filepath.SkipDir
	case rel == "tests/lint":
		// This test's own package. Excluded per task t17's brief so
		// the isolation lint does not have to reason about its own
		// directory (a future fixture file here proving the lint
		// fires would otherwise need special-casing below instead).
		return filepath.SkipDir
	}
	return nil
}

// awsSDKImportsIn parses the non-test .go file at path (rel is its
// repo-relative, forward-slash path, used only for error messages) and
// returns every import path it declares that is the AWS SDK for Go v2 or a
// package under it.
func awsSDKImportsIn(fset *token.FileSet, path, rel string) ([]string, error) {
	f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if parseErr != nil {
		return nil, fmt.Errorf("parse %s: %w", rel, parseErr)
	}

	var found []string
	for _, imp := range f.Imports {
		importPath, unquoteErr := strconv.Unquote(imp.Path.Value)
		if unquoteErr != nil {
			return nil, fmt.Errorf("%s: unquote import %s: %w", rel, imp.Path.Value, unquoteErr)
		}
		if importPath == awsSDKModulePrefix || strings.HasPrefix(importPath, awsSDKModulePrefix+"/") {
			found = append(found, importPath)
		}
	}
	return found, nil
}

// repoRoot locates the repository root from this test file's own path
// (tests/lint/awsisolation_test.go -> tests/lint -> tests -> repo root),
// the same runtime.Caller(0) technique
// internal/actors/neutrality_test.go uses to locate internal/.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repo root to scan")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // tests/lint -> tests -> repo root
}

// sortedSanctionedDirs returns sanctionedAWSPackageDirs's keys, sorted, for
// a deterministic failure message.
func sortedSanctionedDirs() []string {
	dirs := make([]string, 0, len(sanctionedAWSPackageDirs))
	for dir := range sanctionedAWSPackageDirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}
