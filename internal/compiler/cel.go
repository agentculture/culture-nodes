package compiler

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
)

// CEL variables available to every edge guard and decision rule (PRD §11.2).
// All three are dyn: the values they carry are shaped by per-node JSON Schema
// contracts, which CEL's type system cannot import, so the honest declaration
// is "dynamic" rather than a fabricated static type.
//
//   - input    the run input (whatever /run/input holds)
//   - output   the deciding node's output for the outcome being routed
//   - outcome  the domain outcome name that produced this transition
//   - event    the delivered signal event on an `onEvent` edge (issue #43)
//
// `event` ({name, payload, emitter}) is declared in the one shared
// environment rather than a second event-only one, because the engine has to
// rebuild the identical environment from the IR after a restart and two
// environments would be two chances to drift. On a node-outcome transition it
// evaluates as an empty map, so a guard that reaches into it reports "no such
// key" — a guard failure, which does not match — rather than matching on
// nothing.
const (
	CELVarInput   = "input"
	CELVarOutput  = "output"
	CELVarOutcome = "outcome"
	CELVarEvent   = "event"
	CELVarNode    = "node"
	CELVarBudget  = "budget"
)

// newCELEnv builds the compile-time environment. A failure here is an internal
// error, not a statement about any document.
func newCELEnv() (*cel.Env, error) {
	env, err := cel.NewEnv(
		cel.Variable(CELVarInput, cel.DynType),
		cel.Variable(CELVarOutput, cel.DynType),
		cel.Variable(CELVarOutcome, cel.DynType),
		cel.Variable(CELVarEvent, cel.DynType),
		cel.Variable(CELVarNode, cel.DynType),
		cel.Variable(CELVarBudget, cel.DynType),
	)
	if err != nil {
		return nil, fmt.Errorf("compiler: build CEL environment: %w", err)
	}
	return env, nil
}

// compileCEL compiles one expression and records the resulting program under
// its JSON path. Expressions are compiled at *compile* time, not first
// execution, so a typo in a guard is a publication failure rather than a run
// that wedges halfway through.
func (c *compilation) compileCEL(path, expression string) {
	ast, issues := c.env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		c.add(LevelError, path, CodeContractCELInvalid,
			fmt.Sprintf("CEL expression does not compile: %s", firstIssueLine(issues.Err())),
			fmt.Sprintf("fix the expression; the declared variables are %s, %s, %s and %s",
				CELVarInput, CELVarOutput, CELVarOutcome, CELVarEvent))
		return
	}

	// A guard that cannot yield a boolean is a guard that will never be
	// evaluable. dyn is accepted because dyn inputs make most useful
	// expressions dyn-typed.
	if out := ast.OutputType(); out != nil {
		if name := out.String(); name != "bool" && name != "dyn" {
			c.add(LevelError, path, CodeContractCELNotBoolean,
				fmt.Sprintf("CEL expression has type %s; a guard must evaluate to bool", name),
				"rewrite the expression as a predicate, e.g. `outcome == 'passed'`")
			return
		}
	}

	program, err := c.env.Program(ast)
	if err != nil {
		c.add(LevelError, path, CodeContractCELInvalid,
			fmt.Sprintf("CEL expression could not be planned: %v", err),
			"simplify the expression; it parsed and type-checked but could not be turned into a program")
		return
	}
	c.programs[path] = program
}

// firstIssueLine reduces cel-go's multi-line issue rendering — a headline
// followed by the offending source and a caret — to its headline. A
// Diagnostic.Message is one line by contract: `nodes validate` prints one line
// per diagnostic, and a message carrying its own newlines would silently break
// anything reading that output line by line. The line:column position that
// makes the caret redundant is kept.
func firstIssueLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	line = strings.TrimPrefix(line, "ERROR: ")
	line = strings.TrimPrefix(line, "<input>:")
	return strings.TrimSpace(line)
}
