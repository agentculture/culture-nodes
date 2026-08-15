package api

import (
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/compiler"
)

func TestWorkflowGenerationTemplateCompilesAndRoutesExhaustion(t *testing.T) {
	source := renderWorkflowGeneration("actor://company/planner@sha256:aaaaaaaa")
	compiled, diagnostics, err := compiler.Compile([]byte(source), compiler.FormatYAML)
	if err != nil || compiled == nil {
		t.Fatalf("generation workflow compile: compiled=%v err=%v diagnostics=%+v", compiled != nil, err, diagnostics)
	}
	if strings.Contains(source, "openai") || strings.Contains(source, "anthropic") {
		t.Fatal("generation orchestration names a model provider; it must dispatch through the actor ref")
	}
	if !strings.Contains(source, "from: generate.generation_exhausted\n      to: exhausted") {
		t.Fatal("generation exhaustion is not routed as a domain outcome")
	}
}

func TestSourceDiffNamesPinnedBaseAndChangedLines(t *testing.T) {
	diff := sourceDiff("sha256:base", "one\ntwo\nthree", "one\nchanged\nthree")
	want := "--- sha256:base\n+++ proposed\n-two\n+changed\n"
	if diff != want {
		t.Fatalf("diff = %q, want %q", diff, want)
	}
}

func TestGenerationInstructionRequiresExactServerValidationAndNeverPublish(t *testing.T) {
	instruction := generationInstruction("make a review flow", "sha256:base", "old source")
	for _, required := range []string{"POST /v1alpha1/workflows/validate", "valid=true", "zero error", "Do not publish", "sha256:base", "old source"} {
		if !strings.Contains(instruction, required) {
			t.Errorf("instruction does not contain %q", required)
		}
	}
}
