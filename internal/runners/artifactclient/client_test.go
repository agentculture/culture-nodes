package artifactclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/artifacts"
)

func TestPutUploadsThroughThePublicationRoute(t *testing.T) {
	var gotPath, gotAuth, gotName, gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		gotName, gotType = r.Header.Get("Artifact-Name"), r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ref":"artifact://ns_durable/01K2TESTARTIFACT0000000000"}`))
	}))
	defer srv.Close()

	reg := New(srv.Client())
	callback := srv.URL + "/v1/runner-operations/op_1/events"
	if err := reg.Register("att_1", callback, "tok_1"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ref, err := reg.Put(context.Background(),
		artifacts.ArtifactMeta{AttemptID: "att_1", Name: "stdout", MediaType: "text/plain; charset=utf-8"},
		strings.NewReader(`{"emitted": 3}`))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ref != "artifact://ns_durable/01K2TESTARTIFACT0000000000" {
		t.Errorf("ref = %q", ref)
	}
	if gotPath != "/v1alpha1/attempts/att_1/artifacts" {
		t.Errorf("path = %q, want the publication route", gotPath)
	}
	if gotAuth != "Bearer tok_1" || gotName != "stdout" || !strings.HasPrefix(gotType, "text/plain") {
		t.Errorf("headers = (%q, %q, %q)", gotAuth, gotName, gotType)
	}
	if gotBody != `{"emitted": 3}` {
		t.Errorf("body = %q", gotBody)
	}
}

func TestPutRefusesUnregisteredAndReleasedAttempts(t *testing.T) {
	reg := New(nil)
	if _, err := reg.Put(context.Background(), artifacts.ArtifactMeta{AttemptID: "att_x"}, strings.NewReader("x")); err == nil {
		t.Fatal("Put on an unregistered attempt = nil error")
	}
	if err := reg.Register("att_x", "http://h/v1/runner-operations/op/events", "tok"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	reg.Release("att_x")
	if _, err := reg.Put(context.Background(), artifacts.ArtifactMeta{AttemptID: "att_x"}, strings.NewReader("x")); err == nil {
		t.Fatal("Put after Release = nil error")
	}
}

func TestRegisterRefusesUnderivableCallbackURLs(t *testing.T) {
	reg := New(nil)
	for _, bad := range []string{
		"http://host/some/other/path",
		"",
	} {
		if err := reg.Register("att_1", bad, "tok"); err == nil && bad != "" {
			t.Errorf("Register(%q) = nil error, want underivable-origin refusal", bad)
		}
	}
	if err := reg.Register("", "http://h/v1/runner-operations/op/events", "tok"); err == nil {
		t.Error("Register with empty attempt = nil error")
	}
}

func TestPutSurfacesServerRefusals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"artifact callback token rejected"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	reg := New(srv.Client())
	_ = reg.Register("att_1", srv.URL+"/v1/runner-operations/op/events", "tok_expired")
	_, err := reg.Put(context.Background(), artifacts.ArtifactMeta{AttemptID: "att_1", Name: "stdout"}, strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want the 401 surfaced", err)
	}
}
