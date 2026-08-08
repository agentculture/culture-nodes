package worker

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// Decision nodes (PRD §9.2, §11.2).
//
// A decision node is the one MVP kind the control plane executes itself, and
// §11.2 says why that is allowed: CEL is for "edge predicates; bounded
// decisions; policy predicates; small deterministic transforms where
// explicitly allowed". A decision node is a bounded decision over data the
// run already holds. It calls nothing, waits for nothing, and cannot fail
// nondeterministically, so evaluating it in the worker process is not the
// control plane executing arbitrary code — it is the control plane reading
// its own state through a total, side-effect-free expression language.
//
// # What the CEL variables mean here
//
// The compiler declares one environment for every expression it compiles —
// edge guards and decision ports alike — with three variables: input, output,
// outcome. For an edge guard those read naturally. For a decision port they
// need a stated convention, and this is it:
//
//	input    the run's input document, exactly as for an edge guard.
//	output   the decision node's RESOLVED INPUT payload — what it was given
//	         to decide on. A decision node computes nothing of its own, so
//	         the data it routes on IS its output, and that is what gets
//	         recorded as the node's output when the attempt completes. A
//	         downstream /nodes/<decision>/output binding therefore reads the
//	         same document the ports were evaluated against.
//	outcome  the empty string. There is no prior outcome at a decision port:
//	         the port is choosing one. It is declared rather than omitted
//	         because the compiler's environment declares it, and an
//	         expression that references it should evaluate rather than fail
//	         to plan.
//
// # First match wins
//
// Ports are evaluated in the order the IR carries them, and the first whose
// predicate is true is selected. That mirrors edge selection, which is also
// first-match-wins over the compiler's normalized order, so an author does
// not have to hold two different resolution rules in their head.
//
// A node where no port matches is a failed attempt with a diagnostic, not a
// silent stall and not an invented default. The workflow declared the answers
// it accepts; producing one it did not declare would be the engine deciding a
// domain question.

// CEL variable names, matching internal/compiler's environment exactly.
const (
	celVarInput   = "input"
	celVarOutput  = "output"
	celVarOutcome = "outcome"
)

// decisionPrograms holds one node's compiled ports.
type decisionPrograms struct {
	outcomes []string
	programs []cel.Program
}

// decisionCache memoizes compiled decision ports per (digest, node). Guards
// are recompiled from the IR's expression text rather than carried from the
// compiler, for the same reason internal/engine recompiles edge guards: after
// a restart there is no compilation in memory, and having one code path makes
// the restart path as exercised as the fresh one. The compiler has already
// proven each expression compiles and yields a boolean, so this is a rebuild,
// not a second validation pass.
type decisionCache struct {
	mu    sync.Mutex
	env   *cel.Env
	byKey map[string]*decisionPrograms
}

func newDecisionCache() (*decisionCache, error) {
	env, err := cel.NewEnv(
		cel.Variable(celVarInput, cel.DynType),
		cel.Variable(celVarOutput, cel.DynType),
		cel.Variable(celVarOutcome, cel.DynType),
	)
	if err != nil {
		return nil, fmt.Errorf("worker: build CEL environment: %w", err)
	}
	return &decisionCache{env: env, byKey: make(map[string]*decisionPrograms)}, nil
}

func (c *decisionCache) programs(digest string, node *nodeSpec) (*decisionPrograms, error) {
	key := digest + "\x00" + node.ID

	c.mu.Lock()
	cached, ok := c.byKey[key]
	c.mu.Unlock()
	if ok {
		return cached, nil
	}

	compiled := &decisionPrograms{
		outcomes: make([]string, 0, len(node.Select)),
		programs: make([]cel.Program, 0, len(node.Select)),
	}
	for i, port := range node.Select {
		ast, issues := c.env.Compile(port.When)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("worker: node %q select[%d] (%s): %w", node.ID, i, port.Outcome, issues.Err())
		}
		program, err := c.env.Program(ast)
		if err != nil {
			return nil, fmt.Errorf("worker: node %q select[%d] (%s): %w", node.ID, i, port.Outcome, err)
		}
		compiled.outcomes = append(compiled.outcomes, port.Outcome)
		compiled.programs = append(compiled.programs, program)
	}

	c.mu.Lock()
	if existing, ok := c.byKey[key]; ok {
		compiled = existing
	} else {
		c.byKey[key] = compiled
	}
	c.mu.Unlock()
	return compiled, nil
}

// evaluateDecision selects the first port whose predicate holds.
//
// The second return value is false when no port matched, which is a domain
// gap in the definition rather than an error in evaluation — the caller turns
// it into a diagnosed failure.
func (c *decisionCache) evaluateDecision(digest string, node *nodeSpec, runInput, nodeInput json.RawMessage) (string, bool, error) {
	compiled, err := c.programs(digest, node)
	if err != nil {
		return "", false, err
	}
	if len(compiled.programs) == 0 {
		return "", false, fmt.Errorf("worker: decision node %q declares no select ports", node.ID)
	}

	inputValue, err := decodeCELValue(runInput)
	if err != nil {
		return "", false, fmt.Errorf("worker: node %q: run input is not usable in a predicate: %w", node.ID, err)
	}
	outputValue, err := decodeCELValue(nodeInput)
	if err != nil {
		return "", false, fmt.Errorf("worker: node %q: resolved input is not usable in a predicate: %w", node.ID, err)
	}

	activation := map[string]any{
		celVarInput:   inputValue,
		celVarOutput:  outputValue,
		celVarOutcome: "",
	}

	for i, program := range compiled.programs {
		value, _, err := program.Eval(activation)
		if err != nil {
			// A predicate that errors at runtime — a missing member, a type
			// mismatch — has not said "false", it has said nothing. Treating
			// it as false would silently route past a broken port.
			return "", false, fmt.Errorf("worker: node %q select[%d] (%s) did not evaluate: %w",
				node.ID, i, compiled.outcomes[i], err)
		}
		if isTrue(value) {
			return compiled.outcomes[i], true, nil
		}
	}
	return "", false, nil
}

// decodeCELValue turns a JSON document into the plain Go values CEL's dyn
// type accepts. A null or absent document becomes an empty map rather than
// nil, so a predicate reading a member of it gets a clean "no such key"
// rather than a null-dereference-shaped error.
func decodeCELValue(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if value == nil {
		return map[string]any{}, nil
	}
	return value, nil
}

// isTrue reports whether a CEL result is the boolean true. Anything else —
// false, a non-boolean, an error value — is not a match. The compiler already
// refuses an expression whose static type is neither bool nor dyn, so a
// non-boolean here can only come from a dyn expression that resolved to one.
func isTrue(value ref.Val) bool {
	if value == nil {
		return false
	}
	b, ok := value.Value().(bool)
	if !ok {
		return value == types.True
	}
	return b
}
