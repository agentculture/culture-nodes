package api_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

func TestGitHubApprovedWebhookRedeliveryIsDeduplicated(t *testing.T) {
	store := requireStore(t)
	nsID := pgtest.MustNamespace(t, store, "github-webhook").ID
	srv, err := apipkg.NewServer(store, nsID, apipkg.WithGitHubWebhook("webhook-secret"))
	if err != nil {
		t.Fatal(err)
	}
	access := httptest.NewServer(srv.AccessHandler())
	defer access.Close()
	body := `{"action":"submitted","repository":{"full_name":"acme/widgets"},"pull_request":{"number":17,"html_url":"https://github.com/acme/widgets/pull/17","title":"Fix it","user":{"login":"alice"}},"review":{"state":"approved","user":{"login":"bob"}}}`
	mac := hmac.New(sha256.New, []byte("webhook-secret"))
	_, _ = mac.Write([]byte(body))
	post := func() (int, struct {
		Fact struct {
			Name string `json:"name"`
		} `json:"fact"`
		Delivery struct {
			Duplicate bool `json:"duplicate"`
		} `json:"delivery"`
	}) {
		req, err := http.NewRequest(http.MethodPost, access.URL+"/v1alpha1/webhooks/github", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-GitHub-Event", "pull_request_review")
		req.Header.Set("X-GitHub-Delivery", "delivery-abc")
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		resp, err := access.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var result struct {
			Fact struct {
				Name string `json:"name"`
			} `json:"fact"`
			Delivery struct {
				Duplicate bool `json:"duplicate"`
			} `json:"delivery"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, result
	}
	status, first := post()
	if status != http.StatusCreated || first.Fact.Name != "github.pr.approved" || first.Delivery.Duplicate {
		t.Fatalf("first delivery = status %d, name %q, duplicate %v", status, first.Fact.Name, first.Delivery.Duplicate)
	}
	status, second := post()
	if status != http.StatusCreated || second.Fact.Name != "github.pr.approved" || !second.Delivery.Duplicate {
		t.Fatalf("redelivery = status %d, name %q, duplicate %v", status, second.Fact.Name, second.Delivery.Duplicate)
	}
}
