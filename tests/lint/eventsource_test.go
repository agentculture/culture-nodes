package testslint_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestWebHasExactlyOneEventSourceConstructionSite(t *testing.T) {
	root := filepath.Join("..", "..", "web", "src")
	construction := regexp.MustCompile(`\bnew\s+EventSource\s*\(`)
	count := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Ext(path) != ".ts" && filepath.Ext(path) != ".tsx" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		count += len(construction.FindAll(body, -1))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("web/src contains %d EventSource construction sites, want exactly 1", count)
	}
}

// TestSnapshotMarkerNameAgreesAcrossServerAndClient pins the one string the
// tail-only (`?from=latest`) boundary marker is named by on both sides of the
// wire. EventSource dispatch is keyed by the server's `event:` field, so a
// client listening for a name the server never writes silently receives
// nothing: the marker id is never recorded, and the manager's own reconnect
// asks for `latest` again instead of resuming from the boundary — skipping
// whatever committed while the connection was down. That is exactly the
// defect PR #292 shipped and this test exists to make un-shippable: the
// listener name, the server's frame, and the fixtures must all read
// `stream.snapshot`, and none of them may reintroduce the envelope-typed
// `dev.culture.nodes.stream.snapshot` spelling, which no server writes.
func TestSnapshotMarkerNameAgreesAcrossServerAndClient(t *testing.T) {
	server, err := os.ReadFile(filepath.Join("..", "..", "internal", "api", "events.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`event: stream\.snapshot`).Match(server) {
		t.Fatal("internal/api/events.go no longer writes `event: stream.snapshot`; update the client and this test together")
	}

	client, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "hooks", "useSharedEvents.tsx"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`SNAPSHOT_EVENT_NAME = "stream\.snapshot"`).Match(client) {
		t.Fatal("web/src/hooks/useSharedEvents.tsx does not define SNAPSHOT_EVENT_NAME as \"stream.snapshot\"")
	}

	stale := regexp.MustCompile(`dev\.culture\.nodes\.stream\.snapshot`)
	for _, root := range []string{
		filepath.Join("..", "..", "web", "src"),
		filepath.Join("..", "..", "web", "e2e"),
	} {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || filepath.Ext(path) != ".ts" && filepath.Ext(path) != ".tsx" {
				return nil
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if stale.Match(body) {
				t.Errorf("%s names the marker `dev.culture.nodes.stream.snapshot`; the server writes `stream.snapshot`", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
