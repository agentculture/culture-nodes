// Command regen-openapi-json renders api/openapi/openapi.json from
// openapi.yaml exactly the way TestOpenAPIJSONIsTheYAMLRendered checks it
// (YAMLToJSON + 2-space MarshalIndent + trailing newline). Run from the
// repo root: go run ./internal/api/testdata/regen-openapi-json
package main

import (
	"encoding/json"
	"log"
	"os"

	"sigs.k8s.io/yaml"
)

func main() {
	src, err := os.ReadFile("api/openapi/openapi.yaml")
	if err != nil {
		log.Fatalf("read openapi.yaml: %v", err)
	}
	j, err := yaml.YAMLToJSON(src)
	if err != nil {
		log.Fatalf("convert: %v", err)
	}
	var v any
	if err := json.Unmarshal(j, &v); err != nil {
		log.Fatalf("normalize: %v", err)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Fatalf("render: %v", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile("api/openapi/openapi.json", out, 0o644); err != nil {
		log.Fatalf("write openapi.json: %v", err)
	}
	log.Printf("wrote api/openapi/openapi.json (%d bytes)", len(out))
}
