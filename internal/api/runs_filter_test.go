package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRunState(t *testing.T) {
	for _, state := range []string{"", "created", "running", "waiting", "completed", "failed", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1alpha1/runs?state="+state, nil)
			got, err := parseRunState(r)
			if err != nil {
				t.Fatalf("parseRunState(%q): %v", state, err)
			}
			if got != state {
				t.Fatalf("parseRunState(%q) = %q", state, got)
			}
		})
	}
}

func TestListRunsRejectsUnknownStateWithHint(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1alpha1/runs?state=runnning", nil)
	err := (&Server{}).handleListRuns(httptest.NewRecorder(), r)
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("handleListRuns returned %T %v, want *apiError", err, err)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", apiErr.Status, http.StatusBadRequest)
	}
	if !strings.Contains(apiErr.Body.Message, `unrecognized state="runnning"`) {
		t.Fatalf("message = %q, want rejected value", apiErr.Body.Message)
	}
	if !strings.Contains(apiErr.Body.Remediation, "running") {
		t.Fatalf("remediation = %q, want valid-state hint", apiErr.Body.Remediation)
	}
}
