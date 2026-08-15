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
//
// This is the one guard in the package that reads structure rather than text,
// so it does not use sourcescan_test.go's line scanner. The shape is the same
// three steps -- select, look, report -- expressed over an AST: parseFuncDecls
// selects, callsTo looks, and the assertions below report.
func TestSchedulerDeadlineCancellationStaysAfterCommit(t *testing.T) {
	path := filepath.Join(repoRoot(t), "internal", "scheduler", "scheduler.go")
	decls := parseFuncDecls(t, path)
	apply, fire := decls["applyEffect"], decls["fireOne"]
	if apply == nil || fire == nil {
		t.Fatal("scheduler applyEffect/fireOne declaration missing")
	}

	for _, call := range callsTo(apply.Body, "Cancel", "Resolve") {
		t.Errorf("applyEffect calls %s; deadline network work must be post-commit", call.name)
	}

	commitAt := lastCallPos(callsTo(fire.Body, "Commit"))
	cancelAt := lastCallPos(callsTo(fire.Body, "cancelDeadlineInvocation"))
	if commitAt == token.NoPos || cancelAt == token.NoPos || cancelAt < commitAt {
		t.Fatalf("fireOne must call cancelDeadlineInvocation after tx.Commit (commit=%v cancel=%v)", commitAt, cancelAt)
	}
}

// callSite is one call expression a walk matched: what it called, and where.
type callSite struct {
	name string
	pos  token.Pos
}

// parseFuncDecls parses one Go file and indexes its top-level functions and
// methods by name.
func parseFuncDecls(t *testing.T, path string) map[string]*ast.FuncDecl {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	decls := map[string]*ast.FuncDecl{}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			decls[fn.Name.Name] = fn
		}
	}
	return decls
}

// callsTo returns every call under node whose callee is one of names, in
// source order. Matching is on the callee's final identifier, so both
// `cancelDeadlineInvocation(...)` and `sch.cancelDeadlineInvocation(...)`
// count -- a guard about what a function reaches should not care which of
// those forms the code happens to use.
func callsTo(node ast.Node, names ...string) []callSite {
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	var sites []callSite
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if name := calleeName(call); wanted[name] {
			sites = append(sites, callSite{name: name, pos: call.Pos()})
		}
		return true
	})
	return sites
}

// calleeName is the final identifier of a call's callee -- `Commit` for both
// `tx.Commit()` and a bare `Commit()`. Anything else (a call through a func
// value, an index expression) has no name here, and returns "".
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// lastCallPos is where the LAST of sites appears, or token.NoPos when the call
// never appears at all. Last rather than first is deliberate and load-bearing:
// "cancel after commit" has to hold against the final commit in the function,
// not merely the first one.
func lastCallPos(sites []callSite) token.Pos {
	if len(sites) == 0 {
		return token.NoPos
	}
	return sites[len(sites)-1].pos
}
