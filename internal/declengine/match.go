package declengine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/decl/template"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
)

// Ancestor is a causal predecessor, nearest first. Physical re-mints share a
// CanonicalID and occupy only one position in variable lookup and loop counts.
type Ancestor struct {
	FiringID, CanonicalID, DeclarationID, Name string
	Variables                                  map[string]any
}

func ResolveLineage(rows []Ancestor) []Ancestor {
	seen := make(map[string]bool, len(rows))
	out := make([]Ancestor, 0, len(rows))
	for _, row := range rows {
		id := row.CanonicalID
		if id == "" {
			id = row.FiringID
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, row)
	}
	return out
}

func matches(d decl.Declaration, event Event) (bool, error) {
	if d.Trigger.Kind != event.Kind || d.StartNode.Name != event.Node {
		return false, nil
	}
	if len(d.Trigger.With) == 0 {
		return true, nil
	}
	var fields map[string]any
	if err := json.Unmarshal(d.Trigger.With, &fields); err != nil {
		return false, err
	}
	for k, v := range fields {
		if !reflect.DeepEqual(v, event.Variables[k]) {
			return false, nil
		}
	}
	return true, nil
}

func checkLinks(links []postgres.DeclarationLink, lineage []Ancestor) (bool, error) {
	present := map[string]bool{}
	for _, a := range lineage {
		present[a.DeclarationID] = true
	}
	for _, l := range links {
		switch l.Kind {
		case "must":
			if !present[l.ToDeclarationID] {
				return false, nil
			}
		case "can":
		default:
			return false, fmt.Errorf("unknown ordering relation %q", l.Kind)
		}
	}
	return true, nil
}

// CEL receives structured values, never text interpolation. Named predecessors
// are available under lineage; event holds this firing's trigger variables.
func condition(source string, event map[string]any, lineage []Ancestor) (bool, error) {
	env, err := cel.NewEnv(cel.Variable("event", cel.MapType(cel.StringType, cel.DynType)), cel.Variable("lineage", cel.MapType(cel.StringType, cel.DynType)))
	if err != nil {
		return false, err
	}
	ast, issues := env.Compile(source)
	if issues.Err() != nil {
		return false, issues.Err()
	}
	if ast.OutputType() != cel.BoolType {
		return false, fmt.Errorf("condition must return bool")
	}
	program, err := env.Program(ast, cel.CostLimit(100000))
	if err != nil {
		return false, err
	}
	vars := map[string]any{}
	for _, a := range lineage {
		if _, ok := vars[a.Name]; !ok {
			vars[a.Name] = a.Variables
		}
	}
	value, _, err := program.Eval(map[string]any{"event": event, "lineage": vars})
	if err != nil {
		return false, err
	}
	return value == types.True, nil
}

func renderAction(a decl.Action, current map[string]any, lineage []Ancestor) (decl.Action, error) {
	if len(a.With) == 0 {
		return a, nil
	}
	var value any
	if err := json.Unmarshal(a.With, &value); err != nil {
		return a, err
	}
	lookup := func(ref template.Reference) (string, bool) {
		vars := current
		if ref.Step != "0" {
			vars = nil
			if n, err := strconv.Atoi(ref.Step); err == nil {
				if n > 0 && n <= len(lineage) {
					vars = lineage[n-1].Variables
				}
			} else {
				for _, ancestor := range lineage {
					if ancestor.Name == ref.Step {
						vars = ancestor.Variables
						break
					}
				}
			}
		}
		v, ok := vars[ref.Name]
		if !ok || v == nil {
			return "", false
		}
		if s, ok := v.(string); ok {
			return s, true
		}
		raw, err := json.Marshal(v)
		return string(raw), err == nil
	}
	var walk func(any) (any, error)
	walk = func(v any) (any, error) {
		switch x := v.(type) {
		case string:
			t, err := template.Parse(x)
			if err != nil {
				return nil, err
			}
			return t.Render(lookup), nil
		case []any:
			for i, v := range x {
				r, err := walk(v)
				if err != nil {
					return nil, err
				}
				x[i] = r
			}
		case map[string]any:
			for k, v := range x {
				r, err := walk(v)
				if err != nil {
					return nil, err
				}
				x[k] = r
			}
		}
		return v, nil
	}
	value, err := walk(value)
	if err != nil {
		return a, err
	}
	a.With, err = json.Marshal(value)
	return a, err
}
