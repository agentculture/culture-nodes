package api_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// openapiDoc captures only what this test needs from api/openapi/openapi.yaml:
// the path+method+operationId inventory. sigs.k8s.io/yaml (already a
// dependency, per go.mod) converts YAML to JSON and this then decodes with
// ordinary encoding/json semantics, so every other field of the spec is
// simply ignored rather than needing to be modeled here.
type openapiDoc struct {
	Paths map[string]map[string]struct {
		OperationID string `json:"operationId"`
	} `json:"paths"`
}

func loadOpenAPISpec(t *testing.T) openapiDoc {
	t.Helper()
	path := filepath.Join("..", "..", "api", "openapi", "openapi.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc openapiDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(doc.Paths) == 0 {
		t.Fatalf("%s declared no paths", path)
	}
	return doc
}

var pathParamPattern = regexp.MustCompile(`\{[^}]+\}`)

// concretePath fills every {param} segment of an OpenAPI path template with
// a syntactically valid, certainly-nonexistent value, so it can be sent as
// a real HTTP request path.
func concretePath(template string) string {
	return pathParamPattern.ReplaceAllString(template, "route-sweep-placeholder")
}

// TestOpenAPIRoutesAreServed walks every path+method api/openapi/openapi.yaml
// declares and sweeps it against the live mux both ways — the "contract
// honesty" test this task's brief asks for, so the spec file and the mux
// cannot drift silently:
//
//   - every documented path must be registered: a request with an
//     undocumented method against it must get 405 (Method Not Allowed),
//     never 404 — 404 there would mean the *path itself* is not wired into
//     the mux at all, which is exactly the drift this test exists to catch.
//     DELETE is used as the probe method because it is not a documented
//     method anywhere in this spec, so it is always "wrong" for every
//     route.
//   - a path the spec does not declare must still 404 — proof the mux is
//     not simply routing everything through, which would make the 405
//     check above meaningless.
func TestOpenAPIRoutesAreServed(t *testing.T) {
	doc := loadOpenAPISpec(t)
	f := newFixture(t)

	operationOwner := make(map[string]string) // operationId -> "METHOD path" that declared it first
	for template, methods := range doc.Paths {
		for method, op := range methods {
			httpMethod := strings.ToUpper(method)
			template, httpMethod := template, httpMethod // pin for the closure below

			if op.OperationID == "" {
				t.Errorf("%s %s has no operationId", httpMethod, template)
			} else if existing, dup := operationOwner[op.OperationID]; dup {
				t.Errorf("operationId %q is used by both %q and %s %s", op.OperationID, existing, httpMethod, template)
			} else {
				operationOwner[op.OperationID] = httpMethod + " " + template
			}

			t.Run(httpMethod+"_"+template, func(t *testing.T) {
				path := concretePath(template)
				req, err := http.NewRequest(http.MethodDelete, f.url(path), nil)
				if err != nil {
					t.Fatalf("new request: %v", err)
				}
				resp, err := f.client.Do(req)
				if err != nil {
					t.Fatalf("DELETE %s: %v", path, err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusMethodNotAllowed {
					t.Fatalf("DELETE %s: status = %d, want %d (405) — the mux does not appear to serve %s %s",
						path, resp.StatusCode, http.StatusMethodNotAllowed, httpMethod, template)
				}
			})
		}
	}

	t.Run("undocumented_route_404s", func(t *testing.T) {
		resp, err := f.client.Get(f.url("/v1alpha1/not-a-documented-route"))
		if err != nil {
			t.Fatalf("GET undocumented route: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET /v1alpha1/not-a-documented-route: status = %d, want 404", resp.StatusCode)
		}
	})
}

// TestUndocumentedErrorsMatchTheDocumentedShape spot-checks that a
// representative 4xx from each write-path operation (a nonexistent
// resource, or a deliberately empty body) renders the documented
// {code,message,remediation} Error shape — the companion half of "the spec
// file and the mux cannot drift silently": TestOpenAPIRoutesAreServed
// proves every documented operation is wired; this proves its error
// responses are shaped the way the spec says every error response is
// shaped. The full run/ledger/review lifecycle tests already check this for
// their own specific error paths (404s, 409s, 422); this test rounds out
// coverage for operations those do not otherwise exercise with a failing
// call.
func TestUndocumentedErrorsMatchTheDocumentedShape(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"getWorkflow_unknown", http.MethodGet, "/v1alpha1/workflows/sha256:does-not-exist"},
		{"getRun_unknown", http.MethodGet, "/v1alpha1/runs/does-not-exist"},
		{"cancelRun_unknown", http.MethodPost, "/v1alpha1/runs/does-not-exist/cancel"},
		{"streamRunEvents_unknown", http.MethodGet, "/v1alpha1/runs/does-not-exist/events"},
		{"listLedgerRecords_unknown_run", http.MethodGet, "/v1alpha1/runs/does-not-exist/ledger"},
		{"getLedgerProjection_unknown_run", http.MethodGet, "/v1alpha1/runs/does-not-exist/ledger/projections/current_scope"},
		{"createReview_unknown_run", http.MethodPost, "/v1alpha1/runs/does-not-exist/reviews"},
		{"commitReview_unknown", http.MethodPost, "/v1alpha1/reviews/does-not-exist/commit"},
		{"createRun_empty_body", http.MethodPost, "/v1alpha1/runs"},
		{"publishWorkflow_empty_body", http.MethodPost, "/v1alpha1/workflows"},
		{"validateWorkflow_empty_body", http.MethodPost, "/v1alpha1/workflows/validate"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, f.url(tc.path), strings.NewReader("{}"))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := f.client.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", tc.method, tc.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode < 400 {
				t.Fatalf("%s %s: status = %d, want a 4xx for this case", tc.method, tc.path, resp.StatusCode)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("%s %s: read body: %v", tc.method, tc.path, err)
			}
			decodeAPIError(t, body)
		})
	}
}
