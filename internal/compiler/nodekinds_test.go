package compiler_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/agentculture/culture-nodes/internal/compiler"
)

// The node-kind enum stays closed at nine (task t16, issue #101,
// criterion 1).
//
// The pressure this guard exists against is specific and recurring. A merge
// gate is a deterministic validator, and the obvious way to express one is a
// `validator` node kind. The owner's decision was the opposite: the gate is a
// `code` node dispatched through the runner boundary via `runner://`, because
// KindCode already IS the deterministic executor and a tenth kind would ripple
// through the compiler, the authoring schema and the worker for a node that
// runs a command and reports an exit code — which `code` does.
//
// "internal/compiler/vocabulary.go is unchanged" is an acceptance criterion
// about one commit, and would rot the moment the commit landed. This is its
// durable form: the enum's CONTENTS, asserted, so a tenth kind is a deliberate
// act with a failing test in front of it rather than a diff nobody notices.

// closedNodeKinds is the enum as PRD §9.2 freezes it for the MVP.
var closedNodeKinds = []string{
	compiler.KindAgent,
	compiler.KindCode,
	compiler.KindActionHTTP,
	compiler.KindDecision,
	compiler.KindApproval,
	compiler.KindWait,
	compiler.KindParallel,
	compiler.KindJoin,
	compiler.KindEnd,
}

// TestNodeKindEnumStaysClosedAtNine reads the authoring schema — the artefact
// an author is actually validated against — and asserts it against the
// compiler's own constants, so neither can grow without the other.
func TestNodeKindEnumStaysClosedAtNine(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "workflow", "workflow.schema.json"))
	if err != nil {
		t.Fatalf("read the authoring schema: %v", err)
	}
	var doc struct {
		Defs struct {
			Node struct {
				Properties struct {
					Kind struct {
						Enum []string `json:"enum"`
					} `json:"kind"`
				} `json:"properties"`
			} `json:"node"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the authoring schema: %v", err)
	}

	got := doc.Defs.Node.Properties.Kind.Enum
	if len(got) == 0 {
		t.Fatal("the authoring schema declares no node-kind enum — decoding is broken, and a guard over " +
			"zero kinds passes vacuously")
	}
	if len(got) != len(closedNodeKinds) {
		t.Fatalf("the authoring schema declares %d node kinds (%v), the compiler names %d (%v)",
			len(got), got, len(closedNodeKinds), closedNodeKinds)
	}
	for _, kind := range closedNodeKinds {
		if !slices.Contains(got, kind) {
			t.Errorf("the compiler names kind %q, which the authoring schema does not declare", kind)
		}
	}
	for _, kind := range got {
		if !slices.Contains(closedNodeKinds, kind) {
			t.Errorf("the authoring schema declares kind %q, which this guard does not know about. "+
				"If that is a deliberate tenth kind, say so here — and if it is a `validator` or `gate` kind, "+
				"read internal/worker/code.go's gate vocabulary first: a merge gate is a `code` node through "+
				"`runner://`, decided in task t16", kind)
		}
	}
}
