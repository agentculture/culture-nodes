package testslint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// Deadline cancellation is an external side effect. Keep it out of
// applyEffect, whose caller still has the timer transaction open, and require
// fireOne to commit before it reaches the cancellation helper.
func TestSchedulerDeadlineCancellationStaysAfterCommit(t *testing.T) {
	path := filepath.Join(repoRoot(t), "internal", "scheduler", "scheduler.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var apply, fire *ast.FuncDecl
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		switch fn.Name.Name {
		case "applyEffect":
			apply = fn
		case "fireOne":
			fire = fn
		}
	}
	if apply == nil || fire == nil {
		t.Fatal("scheduler applyEffect/fireOne declaration missing")
	}
	ast.Inspect(apply.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Cancel" || sel.Sel.Name == "Resolve") {
			t.Errorf("applyEffect calls %s; deadline network work must be post-commit", sel.Sel.Name)
		}
		return true
	})

	commitAt, cancelAt := token.NoPos, token.NoPos
	ast.Inspect(fire.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fun.Sel.Name == "Commit" {
				commitAt = call.Pos()
			}
			if fun.Sel.Name == "cancelDeadlineInvocation" {
				cancelAt = call.Pos()
			}
		case *ast.Ident:
			if fun.Name == "cancelDeadlineInvocation" {
				cancelAt = call.Pos()
			}
		}
		return true
	})
	if commitAt == token.NoPos || cancelAt == token.NoPos || cancelAt < commitAt {
		t.Fatalf("fireOne must call cancelDeadlineInvocation after tx.Commit (commit=%v cancel=%v)", commitAt, cancelAt)
	}
}
