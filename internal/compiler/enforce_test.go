package compiler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/schemas"
)

// TestAcceptanceEnforceModesCompile is the issue #37 schema acceptance case:
// a workflow declaring each enforce mode — observe, route_technical, a
// route_outcome naming a declared outcome, and no enforce at all (the observe
// default) — compiles with zero diagnostics, and the authored strings survive
// into the normalized IR verbatim.
func TestAcceptanceEnforceModesCompile(t *testing.T) {
	compiled, diags := compileFixture(t, "acceptance-enforce-ok.workflow.yaml", FormatYAML)
	if compiled == nil {
		t.Fatalf("acceptance-enforce-ok fixture did not compile: %s", renderDiagnostics(diags))
	}
	if len(diags) != 0 {
		t.Errorf("acceptance-enforce-ok fixture produced diagnostics, want none: %s", renderDiagnostics(diags))
	}

	var ir map[string]any
	if err := json.Unmarshal(compiled.Normalized, &ir); err != nil {
		t.Fatalf("normalized IR is not valid JSON: %v", err)
	}
	nodes := ir["spec"].(map[string]any)["nodes"].(map[string]any)

	want := map[string]string{
		"build":  "route_outcome:failed",
		"test":   "route_technical",
		"review": "observe",
	}
	for id, mode := range want {
		acceptance, ok := nodes[id].(map[string]any)["acceptance"].(map[string]any)
		if !ok {
			t.Fatalf("node %s carries no acceptance block in the normalized IR", id)
		}
		if acceptance["enforce"] != mode {
			t.Errorf("node %s enforce = %v, want %q", id, acceptance["enforce"], mode)
		}
	}

	// The default is documentation, not materialization: a node that omitted
	// enforce keeps omitting it, so this change re-digests no published
	// workflow (the IR's bytes are what the content digest addresses).
	audit := nodes["audit"].(map[string]any)["acceptance"].(map[string]any)
	if _, present := audit["enforce"]; present {
		t.Errorf("node audit authored no enforce but the IR carries %v; the observe default must stay implicit", audit["enforce"])
	}
}

// TestWorkflowSchemaDocumentsObserveDefault pins acceptance criterion 3 of
// task t16: the schema's own documentation of the enforce field states the
// observe default, and the schema's default annotation agrees with the
// behavior the compiler assumes for an omitted field.
func TestWorkflowSchemaDocumentsObserveDefault(t *testing.T) {
	raw, err := schemas.FS.ReadFile("workflow/workflow.schema.json")
	if err != nil {
		t.Fatalf("read embedded workflow schema: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("parse embedded workflow schema: %v", err)
	}

	acceptance, ok := schema["$defs"].(map[string]any)["acceptance"].(map[string]any)
	if !ok {
		t.Fatal("workflow schema has no $defs/acceptance")
	}
	enforce, ok := acceptance["properties"].(map[string]any)["enforce"].(map[string]any)
	if !ok {
		t.Fatal("$defs/acceptance declares no enforce property")
	}

	if enforce["default"] != "observe" {
		t.Errorf("enforce default = %v, want %q", enforce["default"], "observe")
	}
	description, _ := enforce["description"].(string)
	if !strings.Contains(description, "observe") || !strings.Contains(strings.ToLower(description), "default") {
		t.Errorf("enforce description %q does not state the observe default", description)
	}

	pattern, _ := enforce["pattern"].(string)
	for _, mode := range []string{"observe", "route_technical", "route_outcome"} {
		if !strings.Contains(pattern, mode) {
			t.Errorf("enforce pattern %q does not admit %q", pattern, mode)
		}
	}
}
