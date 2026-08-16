// Package testslint holds lint-as-Go-test checks that are cheaper to run as
// part of `go test ./...` than to stand up as a separate tool -- the same
// rationale internal/actors/neutrality_test.go documents for the provider-
// neutrality guard: a fast tripwire enforced by `go test`, not a
// sophisticated static-analysis pass.
package testslint

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// databaseURLEnvHandle is the environment variable that points the test
// suite at an already-running PostgreSQL SERVER (pgtest.DatabaseURLEnv).
// Split so this file does not trip its own scan.
const databaseURLEnvHandle = "NODES_TEST_DATABASE" + "_URL"

// resolvesDatabaseURLEnv matches the act of RESOLVING that variable, by
// literal or by the pgtest.DatabaseURLEnv constant. Merely naming it -- a
// doc comment, or a skip message telling the reader to set it, as
// tests/e2e/bench_test.go does -- is not a use, and matching those would
// make this guard cry wolf.
var resolvesDatabaseURLEnv = regexp.MustCompile(
	`Getenv\(\s*(?:"` + databaseURLEnvHandle + `"|(?:pgtest\.)?DatabaseURLEnv)\s*\)`)

// isolationCall is the call every reader of that variable must make before
// connecting: it carves a private database out of the named server.
const isolationCall = "pgtest.IsolatedDatabase"

// isolationDefiner is the one file allowed to read the variable without
// calling pgtest.IsolatedDatabase -- it is where IsolatedDatabase lives.
const isolationDefiner = "internal/store/postgres/pgtest/pgtest.go"

// TestEveryTestMainCarvesOutItsOwnDatabase is the standing guard on issue
// #126's root cause.
//
// Several components of this control plane are deployment-wide on purpose:
// the outbox relay drains every pending row regardless of namespace
// (internal/events/relay.go), and the scheduler claims every due timer in
// one batch. Two test binaries pointed at ONE database therefore steal each
// other's rows, and no per-test namespacing can help, because those
// components deliberately ignore namespaces.
//
// That is exactly how CI and a developer's laptop diverged: with the
// variable unset, each package starts a private container and is isolated;
// CI set it to a single shared database, so ~20 concurrently running
// packages shared one `outbox` and one `timers` table. The relay, scheduler
// and engine suites failed intermittently there and never locally.
//
// So: reading the variable and connecting straight to it is banned. Read it,
// then hand it to pgtest.IsolatedDatabase, which treats it as a server and
// returns a private database on it.
func TestEveryTestMainCarvesOutItsOwnDatabase(t *testing.T) {
	root := repoRoot(t)
	files, err := readSourceFiles(root, committedFiles(t, root), func(rel string) bool {
		return filepath.Ext(rel) == ".go" && !strings.HasPrefix(rel, "tests/lint/")
	})
	if err != nil {
		t.Fatal(err)
	}

	var readers, offenders []string
	for _, file := range files {
		if !resolvesDatabaseURLEnv.MatchString(file.contents) {
			continue
		}
		readers = append(readers, file.rel)
		if file.rel == isolationDefiner {
			continue
		}
		if !strings.Contains(file.contents, isolationCall) {
			offenders = append(offenders, file.rel)
		}
	}
	sort.Strings(readers)
	sort.Strings(offenders)

	if len(readers) == 0 {
		t.Fatalf("no file under %s resolves %s; this gate would pass on any tree", root, databaseURLEnvHandle)
	}
	if len(offenders) > 0 {
		t.Fatalf("these files resolve %s and connect to it directly, which makes every "+
			"concurrently running test binary share one outbox and one timers table (issue #126):\n  %s\n\n"+
			"Pass the value to %s and use the database it returns instead.",
			databaseURLEnvHandle, strings.Join(offenders, "\n  "), isolationCall)
	}
}

// clusterWideCatalogs are PostgreSQL views that span every database on the
// server. A per-database test fixture that reads one of these is looking at
// other test binaries' backends too, so it must say which database it means.
var clusterWideCatalogs = []string{"pg_locks", "pg_stat_activity"}

// TestClusterWideCatalogQueriesSayWhichDatabaseTheyMean is the second half
// of the isolation contract.
//
// Giving each test binary its own database stops them sharing TABLES. It
// does not stop them sharing the server's own catalogs: pg_locks and
// pg_stat_activity list backends across every database. A query that reads
// one unscoped and then acts on the pid it finds -- as this repo's scheduler
// takeover test did with pg_terminate_backend -- kills an unrelated
// package's connection mid-statement, which surfaces somewhere else
// entirely as "FATAL: terminating connection due to administrator command".
//
// Naming current_database() is the cheap, checkable form of "I mean my own".
func TestClusterWideCatalogQueriesSayWhichDatabaseTheyMean(t *testing.T) {
	root := repoRoot(t)
	files, err := readSourceFiles(root, committedFiles(t, root), func(rel string) bool {
		return filepath.Ext(rel) == ".go" && !strings.HasPrefix(rel, "tests/lint/")
	})
	if err != nil {
		t.Fatal(err)
	}

	var readers, offenders []string
	for _, file := range files {
		hit := ""
		for _, catalog := range clusterWideCatalogs {
			if strings.Contains(file.contents, catalog) {
				hit = catalog
				break
			}
		}
		if hit == "" {
			continue
		}
		readers = append(readers, file.rel)
		if !strings.Contains(file.contents, "current_database()") {
			offenders = append(offenders, fmt.Sprintf("%s (reads %s)", file.rel, hit))
		}
	}
	sort.Strings(readers)
	sort.Strings(offenders)

	if len(readers) == 0 {
		t.Fatalf("no file under %s reads %v; this gate would pass on any tree", root, clusterWideCatalogs)
	}
	if len(offenders) > 0 {
		t.Fatalf("these files read a cluster-wide catalog without scoping it to current_database(), "+
			"so they see (and can act on) other test binaries' backends:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestTheIsolationScannerActuallyFires is the gate on the gate: the check
// above passes both when every TestMain isolates AND when the scan is
// broken. Plant a file that resolves the variable without isolating and
// prove the same predicate rejects it.
func TestTheIsolationScannerActuallyFires(t *testing.T) {
	planted := fmt.Sprintf("dbURL := os.Getenv(%q)\nstore, _ := postgres.Connect(ctx, dbURL)\n", databaseURLEnvHandle)
	if !resolvesDatabaseURLEnv.MatchString(planted) {
		t.Fatal("the planted violation does not even match the scanner's selector")
	}
	if strings.Contains(planted, isolationCall) {
		t.Fatalf("the planted violation calls %s, so it proves nothing", isolationCall)
	}

	// And the converse: a file that only NAMES the variable in prose must
	// not be reported, or the guard becomes noise people route around.
	mention := "// set " + databaseURLEnvHandle + " to point at a server\n" +
		`raw := os.Getenv("NODES_BENCH_LEDGER_RECORDS")` + "\n"
	if resolvesDatabaseURLEnv.MatchString(mention) {
		t.Fatalf("the scanner flags a file that merely mentions %s in a comment", databaseURLEnvHandle)
	}
}
