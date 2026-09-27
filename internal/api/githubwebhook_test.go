package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubWebhookSignatureRequired(t *testing.T) {
	s := &Server{githubWebhook: githubWebhookConfig{secret: []byte("secret")}}
	body := []byte(`{"action":"submitted"}`)
	unsigned := httptest.NewRequest("POST", "/v1alpha1/webhooks/github", strings.NewReader(string(body)))
	if s.verifyGitHubWebhook(unsigned, body) {
		t.Fatal("unsigned payload accepted")
	}
	bad := httptest.NewRequest("POST", "/v1alpha1/webhooks/github", nil)
	bad.Header.Set("X-Hub-Signature-256", "sha256=00")
	if s.verifyGitHubWebhook(bad, body) {
		t.Fatal("bad signature accepted")
	}
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	good := httptest.NewRequest("POST", "/v1alpha1/webhooks/github", nil)
	good.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if !s.verifyGitHubWebhook(good, body) {
		t.Fatal("valid signature refused")
	}
}

func TestGitHubWebhookRefusalIsLogged(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{githubWebhook: githubWebhookConfig{secret: []byte("secret")}, log: slog.New(slog.NewJSONHandler(&logs, nil))}
	r := httptest.NewRequest("POST", "/v1alpha1/webhooks/github", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	if err := s.handleGitHubWebhook(w, r); err == nil {
		t.Fatal("unsigned webhook was not refused")
	}
	if !strings.Contains(logs.String(), "GitHub webhook refused") {
		t.Fatalf("refusal missing from log: %s", logs.String())
	}
}

func TestGitHubWebhookMapsApprovedAndCreated(t *testing.T) {
	tests := []struct {
		name, action, state, expected string
	}{
		{"review approval", "pull_request_review", "approved", "github.pr.approved"},
		{"pull request created", "pull_request", "", "github.pr.created"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"action":"` + map[string]string{"pull_request_review": "submitted", "pull_request": "opened"}[tt.action] + `","repository":{"full_name":"acme/widgets"},"pull_request":{"number":17,"html_url":"https://github.com/acme/widgets/pull/17","title":"Fix it","user":{"login":"alice"}},"review":{"state":"` + tt.state + `","user":{"login":"bob"}}}`)
			fact, ok, err := githubWebhookFact(tt.action, body, "delivery-1")
			if err != nil || !ok {
				t.Fatalf("githubWebhookFact() = (%v, %v, %v)", fact, ok, err)
			}
			if fact.Name != tt.expected {
				t.Fatalf("event name = %q, want %q", fact.Name, tt.expected)
			}
			if fact.SourceKey != "github:delivery-1" || fact.Subject != "acme/widgets#17" {
				t.Fatalf("fact identity = source %q subject %q", fact.SourceKey, fact.Subject)
			}
		})
	}
}
