// Overlap detection (spec c78/h42, ADR 0014): "when two declarations share
// similar conditions (overlapping trigger and condition), the user can see
// it, or be notified of it." At publish and on every activation (the two
// choke points activation.go's Publish and Activate already own),
// ReportOverlaps re-scans the namespace's currently ACTIVE declarations and
// reports every pair that:
//
//   - starts on the same node (StartNode.Name);
//   - shares the same trigger kind;
//   - has conditions that CAN both be true.
//
// CEL satisfiability is undecidable in general, so this is deliberately a
// sound, narrow analysis rather than a SAT solver: it only DECIDES a
// condition that is a conjunction ("&&") of equality/inequality comparisons
// of a single event field against a literal -- e.g.
// `event.priority == 'High' && event.priority != 'Low'`. Anything the
// grammar below does not cover (an "||", a function call, a comparison
// between two fields, a non-literal operand, a macro) makes that
// declaration's condition UNDECIDABLE, and any pair involving it is
// reported as "possibly overlapping" -- never silently dropped, and never
// wrongly claimed decided.
package declengine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/operators"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

// OverlapTriggerKind is the registered trigger kind (internal/decl/kinds)
// a declaration declares to be notified of an overlap report.
const OverlapTriggerKind = "declaration.overlap"

// Overlap report statuses. There is deliberately no "not overlapping"
// status: a pair the analysis decides can never both be true is simply
// absent from the report, not reported with a negative verdict.
const (
	OverlapConfirmed = "overlapping"
	OverlapPossible  = "possibly overlapping"
)

// OverlapDeclarationRef names one side of a reported pair.
type OverlapDeclarationRef struct {
	DeclarationID string `json:"declaration_id"`
	VersionID     string `json:"version_id"`
	Name          string `json:"name"`
	Condition     string `json:"condition"`
}

// OverlapPair is one reported pair of active declarations. A and B are
// ordered by declaration ID so the same pair always reports identically
// regardless of scan order.
type OverlapPair struct {
	Node        string                `json:"node"`
	TriggerKind string                `json:"trigger_kind"`
	A           OverlapDeclarationRef `json:"a"`
	B           OverlapDeclarationRef `json:"b"`
	Status      string                `json:"status"`
}

// DetectOverlaps is the pure analysis over an already-fetched active set
// (e.g. PostgresBackend.Active's result): no I/O, deterministic output.
func DetectOverlaps(active []ActiveDeclaration) []OverlapPair {
	type candidate struct {
		ref  OverlapDeclarationRef
		node string
		trig string
		conj conjunction
	}
	candidates := make([]candidate, 0, len(active))
	for _, a := range active {
		candidates = append(candidates, candidate{
			ref: OverlapDeclarationRef{
				DeclarationID: a.ID,
				VersionID:     a.VersionID,
				Name:          a.Declaration.Name,
				Condition:     a.Declaration.Condition,
			},
			node: a.Declaration.StartNode.Name,
			trig: a.Declaration.Trigger.Kind,
			conj: analyzeCondition(a.Declaration.Condition),
		})
	}
	var pairs []OverlapPair
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			ci, cj := candidates[i], candidates[j]
			if ci.ref.DeclarationID == cj.ref.DeclarationID {
				// Active returns at most one version per declaration_id;
				// this never fires in practice, but two versions of one
				// declaration are never a "pair" either way.
				continue
			}
			if ci.node != cj.node || ci.trig != cj.trig {
				continue
			}
			overlap, decided := canBothBeTrue(ci.conj, cj.conj)
			if decided && !overlap {
				continue
			}
			status := OverlapPossible
			if decided {
				status = OverlapConfirmed
			}
			first, second := ci.ref, cj.ref
			if second.DeclarationID < first.DeclarationID {
				first, second = second, first
			}
			pairs = append(pairs, OverlapPair{Node: ci.node, TriggerKind: ci.trig, A: first, B: second, Status: status})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].A.DeclarationID != pairs[j].A.DeclarationID {
			return pairs[i].A.DeclarationID < pairs[j].A.DeclarationID
		}
		return pairs[i].B.DeclarationID < pairs[j].B.DeclarationID
	})
	return pairs
}

// FormatOverlapReport renders pairs as the readable report a future
// show/focus surface reads from (this task's acceptance criterion: "the
// report must be readable, for example via a function the future
// show/focus API will call"). Empty input reports that nothing was found
// rather than an empty string, so a caller can print the result directly.
func FormatOverlapReport(pairs []OverlapPair) string {
	if len(pairs) == 0 {
		return "no overlapping declarations found"
	}
	var b strings.Builder
	for _, p := range pairs {
		fmt.Fprintf(&b, "%s: %q and %q both start on node %q on trigger %q (conditions %q vs %q)\n",
			p.Status, p.A.Name, p.B.Name, p.Node, p.TriggerKind, p.A.Condition, p.B.Condition)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ReportOverlaps re-scans namespaceID's currently active declarations for
// overlapping pairs (c78/h42) and returns them regardless of anything else.
// When configured -- the namespace has an active declaration whose trigger
// kind is declaration.overlap -- it also delivers one declaration.overlap
// signal event per pair via store.DeliverSignalEvent, so a namespace that
// never opts in a listener never accumulates event-log noise for a report
// nobody asked to be notified of.
func ReportOverlaps(ctx context.Context, store *postgres.Store, namespaceID string) ([]OverlapPair, error) {
	active, err := (PostgresBackend{Store: store}).Active(ctx, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("declengine: overlap report: %w", err)
	}
	pairs := DetectOverlaps(active)
	if len(pairs) == 0 {
		return pairs, nil
	}
	configured := false
	for _, a := range active {
		if a.Declaration.Trigger.Kind == OverlapTriggerKind {
			configured = true
			break
		}
	}
	if !configured {
		return pairs, nil
	}
	for _, p := range pairs {
		payload, err := json.Marshal(p)
		if err != nil {
			return pairs, fmt.Errorf("declengine: overlap report: encode pair: %w", err)
		}
		if _, err := store.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{
			NamespaceID: namespaceID,
			Name:        OverlapTriggerKind,
			Payload:     payload,
			Emitter:     "declengine.overlap",
		}); err != nil {
			return pairs, fmt.Errorf("declengine: overlap report: deliver event: %w", err)
		}
	}
	return pairs, nil
}

// conjunction is the decided-or-not result of analyzing one declaration's
// condition string. decidable is false whenever the source falls outside
// the narrow grammar this package handles; impossible is true when the
// conjunction is internally self-contradictory (e.g. `x=='a' && x!='a'`, or
// a literal `false`) -- a declaration whose condition can never be true
// can never overlap with anything, decided.
type conjunction struct {
	decidable  bool
	impossible bool
	eq         map[string]literal
	neq        map[string][]literal
}

// literal is a normalized CEL constant. kind disambiguates the zero values
// of the scalar fields (an int 0 and a bool false must never compare equal).
type literal struct {
	kind string
	b    bool
	i    int64
	u    uint64
	d    float64
	s    string
}

func litEqual(a, b literal) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case "bool":
		return a.b == b.b
	case "int":
		return a.i == b.i
	case "uint":
		return a.u == b.u
	case "double":
		return a.d == b.d
	case "string":
		return a.s == b.s
	default:
		return false
	}
}

// celOverlapEnv mirrors match.go's condition() environment: conditions are
// compiled against the same event/lineage variables they run against at
// evaluation time, so a condition that fails to compile here would also
// fail to compile there.
func celOverlapEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("event", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("lineage", cel.MapType(cel.StringType, cel.DynType)),
	)
}

// analyzeCondition parses source and walks its AST looking for a
// conjunction of `event.<field> == <literal>` / `event.<field> != <literal>`
// comparisons. A source that does not compile, or does not fit that
// grammar, comes back with decidable == false.
func analyzeCondition(source string) conjunction {
	c := conjunction{eq: map[string]literal{}, neq: map[string][]literal{}}
	env, err := celOverlapEnv()
	if err != nil {
		return c
	}
	ast, issues := env.Compile(source)
	if issues != nil && issues.Err() != nil {
		return c
	}
	expr := ast.Expr()
	if expr == nil {
		return c
	}
	c.decidable = walkConjunct(expr, &c)
	return c
}

func walkConjunct(e *exprpb.Expr, c *conjunction) bool {
	if e == nil {
		return false
	}
	if call := e.GetCallExpr(); call != nil {
		switch call.GetFunction() {
		case operators.LogicalAnd:
			args := call.GetArgs()
			if len(args) != 2 {
				return false
			}
			// Both sides must be decidable, and BOTH are always walked
			// (not short-circuited) so every constraint the conjunction
			// carries is recorded even when an earlier arg already turned
			// out impossible.
			left := walkConjunct(args[0], c)
			right := walkConjunct(args[1], c)
			return left && right
		case operators.Equals, operators.NotEquals:
			args := call.GetArgs()
			if len(args) != 2 {
				return false
			}
			field, lit, ok := fieldLiteral(args[0], args[1])
			if !ok {
				return false
			}
			if call.GetFunction() == operators.Equals {
				addEq(c, field, lit)
			} else {
				addNeq(c, field, lit)
			}
			return true
		default:
			return false
		}
	}
	if constExpr := e.GetConstExpr(); constExpr != nil {
		if b, ok := constExpr.GetConstantKind().(*exprpb.Constant_BoolValue); ok {
			if !b.BoolValue {
				c.impossible = true
			}
			return true
		}
		return false
	}
	return false
}

// fieldLiteral recognizes `event.<field> <op> <literal>` in either operand
// order.
func fieldLiteral(a, b *exprpb.Expr) (string, literal, bool) {
	if field, ok := selectEventField(a); ok {
		if lit, ok := constLiteral(b); ok {
			return field, lit, true
		}
	}
	if field, ok := selectEventField(b); ok {
		if lit, ok := constLiteral(a); ok {
			return field, lit, true
		}
	}
	return "", literal{}, false
}

// selectEventField recognizes `event.<field>` -- a select expression whose
// operand is the bare identifier "event". Anything deeper (event.a.b),
// indexed (event["a"]), or off a different root (lineage.x) is left
// undecidable by the caller.
func selectEventField(e *exprpb.Expr) (string, bool) {
	sel := e.GetSelectExpr()
	if sel == nil || sel.GetTestOnly() {
		return "", false
	}
	ident := sel.GetOperand().GetIdentExpr()
	if ident == nil || ident.GetName() != "event" {
		return "", false
	}
	return sel.GetField(), true
}

func constLiteral(e *exprpb.Expr) (literal, bool) {
	c := e.GetConstExpr()
	if c == nil {
		return literal{}, false
	}
	switch v := c.GetConstantKind().(type) {
	case *exprpb.Constant_BoolValue:
		return literal{kind: "bool", b: v.BoolValue}, true
	case *exprpb.Constant_Int64Value:
		return literal{kind: "int", i: v.Int64Value}, true
	case *exprpb.Constant_Uint64Value:
		return literal{kind: "uint", u: v.Uint64Value}, true
	case *exprpb.Constant_DoubleValue:
		return literal{kind: "double", d: v.DoubleValue}, true
	case *exprpb.Constant_StringValue:
		return literal{kind: "string", s: v.StringValue}, true
	default:
		return literal{}, false
	}
}

func addEq(c *conjunction, field string, lit literal) {
	if existing, ok := c.eq[field]; ok {
		if !litEqual(existing, lit) {
			c.impossible = true
		}
		return
	}
	c.eq[field] = lit
	for _, n := range c.neq[field] {
		if litEqual(n, lit) {
			c.impossible = true
		}
	}
}

func addNeq(c *conjunction, field string, lit literal) {
	if existing, ok := c.eq[field]; ok && litEqual(existing, lit) {
		c.impossible = true
	}
	c.neq[field] = append(c.neq[field], lit)
}

// canBothBeTrue decides whether two conjunctions can be simultaneously
// satisfied. decided is false whenever either side was itself undecidable
// (the caller reports "possibly overlapping"); when decided is true,
// overlap says whether the analysis found a satisfying assignment.
func canBothBeTrue(a, b conjunction) (overlap, decided bool) {
	if !a.decidable || !b.decidable {
		return false, false
	}
	if a.impossible || b.impossible {
		// A conjunction that can never be true can never overlap with
		// anything -- decided, not reported.
		return false, true
	}
	fields := map[string]bool{}
	for f := range a.eq {
		fields[f] = true
	}
	for f := range a.neq {
		fields[f] = true
	}
	for f := range b.eq {
		fields[f] = true
	}
	for f := range b.neq {
		fields[f] = true
	}
	for f := range fields {
		aEq, aHas := a.eq[f]
		bEq, bHas := b.eq[f]
		switch {
		case aHas && bHas:
			if !litEqual(aEq, bEq) {
				return false, true
			}
		case aHas && !bHas:
			for _, n := range b.neq[f] {
				if litEqual(n, aEq) {
					return false, true
				}
			}
		case bHas && !aHas:
			for _, n := range a.neq[f] {
				if litEqual(n, bEq) {
					return false, true
				}
			}
		default:
			// Neither side pins this field to one value: any value
			// outside both exclusion lists satisfies both, UNLESS the
			// domain is the closed two-value bool domain and the union of
			// exclusions covers both true and false.
			if boolExcludesBoth(a.neq[f], b.neq[f]) {
				return false, true
			}
		}
	}
	return true, true
}

func boolExcludesBoth(a, b []literal) bool {
	var excludesTrue, excludesFalse bool
	for _, l := range a {
		if l.kind != "bool" {
			return false
		}
		if l.b {
			excludesTrue = true
		} else {
			excludesFalse = true
		}
	}
	for _, l := range b {
		if l.kind != "bool" {
			return false
		}
		if l.b {
			excludesTrue = true
		} else {
			excludesFalse = true
		}
	}
	return excludesTrue && excludesFalse
}
