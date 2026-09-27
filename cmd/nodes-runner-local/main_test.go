package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHealthRespondsWhileRunnerRequestBlocks(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	h := handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusAccepted)
	}))
	defer close(release)
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/operations", strings.NewReader("{}")))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runner request did not start")
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d", response.Code)
	}
}

// Keep process launch imports out of the authoritative service. Only runner
// adapters and their client may own this boundary.
func TestControlPlaneHasNoExecImport(t *testing.T) {
	root := filepath.Join("..", "..", "internal")
	for _, dir := range []string{filepath.Join(root, "api"), filepath.Join(root, "worker"), filepath.Join(root, "scheduler"), filepath.Join(root, "engine"), filepath.Join(root, "tca"), filepath.Join("..", "nodes")} {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range file.Imports {
				if imp.Path.Value == `"os/exec"` {
					t.Errorf("%s imports process launch package %s", path, imp.Path.Value)
				}
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if selector.Sel.Name == "StartProcess" || selector.Sel.Name == "ForkExec" {
					t.Errorf("%s calls process launch method %s", path, selector.Sel.Name)
				}
				return true
			})
		}
	}
}
