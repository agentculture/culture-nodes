package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"reflect"
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

func TestGitHubApprovalReviewIdentityMatchesPoller(t *testing.T) {
	body := []byte(`{"action":"submitted","repository":{"full_name":"acme/widgets"},"pull_request":{"number":17,"html_url":"https://github.com/acme/widgets/pull/17","title":"Fix it","user":{"login":"alice"}},"review":{"id":412,"state":"approved","user":{"login":"bob"},"submitted_at":"2026-09-27T10:00:00Z"}}`)
	fact, ok, err := githubWebhookFact("pull_request_review", body, "delivery-1")
	if err != nil || !ok {
		t.Fatalf("githubWebhookFact() = (%v, %v, %v)", fact, ok, err)
	}
	if fact.Name != "github.pr.approved" || fact.SourceKey != "github:acme/widgets:pr:17:review:412:approved" || fact.Subject != "acme/widgets#17" {
		t.Fatalf("fact identity = %+v", fact)
	}
	wantPayload := `{"source":"github","repository":"acme/widgets","id":"17","title":"Fix it","url":"https://github.com/acme/widgets/pull/17","author":"alice","reviewer":"bob"}`
	var got, want any
	if err := json.Unmarshal(fact.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(wantPayload), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payload = %s, want %s", fact.Payload, wantPayload)
	}
	if string(fact.Watermark) != `{"review_id":"412"}` {
		t.Fatalf("watermark = %s", fact.Watermark)
	}
	again, ok, err := githubWebhookFact("pull_request_review", body, "another-delivery")
	if err != nil || !ok || again.SourceKey != fact.SourceKey || string(again.Watermark) != string(fact.Watermark) {
		t.Fatalf("same review on another delivery = (%+v, %v, %v)", again, ok, err)
	}
}

func TestGitHubJSONNumberText(t *testing.T) {
	var decoded any
	if err := json.Unmarshal([]byte(`412`), &decoded); err != nil {
		t.Fatal(err)
	}
	if got := text(decoded); got != "412" {
		t.Fatalf("text(JSON number 412) = %q", got)
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
			body := []byte(`{"action":"` + map[string]string{"pull_request_review": "submitted", "pull_request": "opened"}[tt.action] + `","repository":{"full_name":"acme/widgets"},"pull_request":{"number":17,"html_url":"https://github.com/acme/widgets/pull/17","title":"Fix it","user":{"login":"alice"}},"review":{"id":412,"state":"` + tt.state + `","user":{"login":"bob"}}}`)
			fact, ok, err := githubWebhookFact(tt.action, body, "delivery-1")
			if err != nil || !ok {
				t.Fatalf("githubWebhookFact() = (%v, %v, %v)", fact, ok, err)
			}
			if fact.Name != tt.expected {
				t.Fatalf("event name = %q, want %q", fact.Name, tt.expected)
			}
			wantKey := "github:delivery-1"
			if tt.action == "pull_request_review" {
				wantKey = "github:acme/widgets:pr:17:review:412:approved"
			}
			if fact.SourceKey != wantKey || fact.Subject != "acme/widgets#17" {
				t.Fatalf("fact identity = source %q subject %q", fact.SourceKey, fact.Subject)
			}
		})
	}
}
