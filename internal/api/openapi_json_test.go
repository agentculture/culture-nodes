package api

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestOpenAPIJSONIsTheYAMLRendered enforces the repo's "JSON is
// authoritative, YAML is authoring sugar" rule on the API contract itself:
// api/openapi/openapi.json must be exactly api/openapi/openapi.yaml
// converted (2-space indent, trailing newline). Regenerate with:
//
//	go run ./internal/api/testdata/regen-openapi-json  (or see this test)
func TestOpenAPIJSONIsTheYAMLRendered(t *testing.T) {
	src, err := os.ReadFile("../../api/openapi/openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	j, err := yaml.YAMLToJSON(src)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var v any
	if err := json.Unmarshal(j, &v); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want = append(want, '\n')

	got, err := os.ReadFile("../../api/openapi/openapi.json")
	if err != nil {
		t.Fatalf("read openapi.json (regenerate it from the YAML): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("api/openapi/openapi.json is stale relative to openapi.yaml — regenerate it (YAMLToJSON + MarshalIndent 2-space + trailing newline)")
	}
}
