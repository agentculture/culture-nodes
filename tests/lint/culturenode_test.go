package testslint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cultureNodeImport = "culture-design/CultureNode"

func source(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// The run and authoring canvases reach the one visual through WorkflowNode;
// Active Graphs imports it directly. This pins the complete render chain,
// rather than accepting a dead import in each route.
func TestReactFlowCanvasesRenderTheCultureNode(t *testing.T) {
	adapter := source(t, "web/src/components/WorkflowNode.tsx")
	if !strings.Contains(adapter, cultureNodeImport) {
		t.Fatal("WorkflowNode must import culture-design/CultureNode")
	}
	for _, rel := range []string{
		"web/src/routes/RunView.tsx",
		"web/src/routes/AuthorWorkflow.tsx",
	} {
		if !strings.Contains(source(t, rel), "components/WorkflowNode") {
			t.Errorf("%s must render through the WorkflowNode adapter", rel)
		}
	}
	if !strings.Contains(source(t, "web/src/components/ActiveGraphCanvas.tsx"), cultureNodeImport) {
		t.Error("ActiveGraphCanvas must import culture-design/CultureNode")
	}
}

func TestNoSecondNodeCardImplementation(t *testing.T) {
	card := source(t, "web/src/components/NodeCard.tsx")
	if strings.Contains(card, "function NodeCard") || strings.Contains(card, "<div") {
		t.Fatal("NodeCard.tsx must remain only a compatibility adapter")
	}
	if !strings.Contains(card, cultureNodeImport) {
		t.Fatal("NodeCard adapter must re-export culture-design/CultureNode")
	}
}

func TestNodeRulesLiveInTheDesignLayer(t *testing.T) {
	app := source(t, "web/src/styles/app.css")
	for _, selector := range []string{".node-card {", ".active-node {", "@keyframes active-node"} {
		if strings.Contains(app, selector) {
			t.Errorf("%s must move from app.css to culture-design/node.css", selector)
		}
	}
	design := source(t, "web/src/culture-design/node.css")
	for _, selector := range []string{".culture-node {", ".culture-node__core", ".culture-node__halo", ".culture-node__pulse", "@keyframes culture-node"} {
		if !strings.Contains(design, selector) {
			t.Errorf("node.css is missing %s", selector)
		}
	}
}

func TestEveryCanvasKeepsTerminalGround(t *testing.T) {
	for _, rel := range []string{
		"web/src/routes/RunView.tsx",
		"web/src/routes/AuthorWorkflow.tsx",
		"web/src/components/ActiveGraphCanvas.tsx",
		"web/src/components/MeshCanvas.tsx",
	} {
		if !strings.Contains(source(t, rel), "canvas-surface") {
			t.Errorf("%s must carry canvas-surface", rel)
		}
	}
}
