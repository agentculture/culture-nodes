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
