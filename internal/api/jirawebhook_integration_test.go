package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

type jiraWebhookResult struct {
	Deliveries []struct {
		Fact struct {
			Name, SourceKey string
			Watermark       json.RawMessage
		} `json:"fact"`
		Delivery struct {
			Duplicate bool `json:"duplicate"`
		} `json:"delivery"`
	} `json:"deliveries"`
}

func TestJiraWebhookReplayIsOrderAndDuplicateSafe(t *testing.T) {
	store := requireStore(t)
	fixture, err := os.ReadFile(filepath.Join("testdata", "jira_issue.json"))
	if err != nil {
		t.Fatal(err)
	}
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer jira.Close()
	nsID := pgtest.MustNamespace(t, store, "jira-webhook").ID
	srv, err := apipkg.NewServer(store, nsID, apipkg.WithJiraWebhook("", "wake-token", jira.URL, "team.example.com", "SCRUM", "reader@example.com", "api-token", "bot"))
	if err != nil {
		t.Fatal(err)
	}
	access := httptest.NewServer(srv.AccessHandler())
	defer access.Close()

	webhooks := []string{
		`{"webhookEvent":"jira:issue_created","issue":{"key":"SCRUM-42"}}`,
		`{"webhookEvent":"comment_created","issue":{"key":"SCRUM-42"}}`,
		`{"webhookEvent":"jira:issue_updated","issue":{"key":"SCRUM-42"}}`,
	}
	var baseline []struct{ Name, SourceKey, Watermark string }
	for pass, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {0, 1, 2}} {
		for _, index := range order {
			var result jiraWebhookResult
			resp, body := doJSON(t, access.Client(), http.MethodPost, access.URL+"/v1alpha1/webhooks/jira?token=wake-token", json.RawMessage(webhooks[index]), &result)
			requireStatus(t, resp, body, http.StatusCreated)
			var tuple []struct{ Name, SourceKey, Watermark string }
			for _, item := range result.Deliveries {
				tuple = append(tuple, struct{ Name, SourceKey, Watermark string }{item.Fact.Name, item.Fact.SourceKey, string(item.Fact.Watermark)})
				if pass > 0 || index > 0 {
					if !item.Delivery.Duplicate {
						t.Fatalf("pass %d event %d repeated fact %s was not duplicate", pass, index, item.Fact.SourceKey)
					}
				}
			}
			if baseline == nil {
				baseline = tuple
			} else if !reflect.DeepEqual(tuple, baseline) {
				t.Fatalf("event order changed replay tuples\n got: %#v\nwant: %#v", tuple, baseline)
			}
		}
	}
}
