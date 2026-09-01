package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestJiraWebhookAuthentication(t *testing.T) {
	body := []byte(`{"issue":{"key":"SCRUM-42"}}`)
	s := &Server{jiraWebhook: jiraWebhookConfig{secret: []byte("secret"), token: []byte("token")}}
	request := httptest.NewRequest("POST", "/v1alpha1/webhooks/jira?token=token", nil)
	if !s.verifyJiraWebhook(request, body) {
		t.Fatal("valid URL token refused")
	}
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	request.Header.Set("X-Hub-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if !s.verifyJiraWebhook(request, body) {
		t.Fatal("valid HMAC refused")
	}
	request.Header.Set("X-Hub-Signature", "sha256=00")
	if s.verifyJiraWebhook(request, body) {
		t.Fatal("bad HMAC fell back to URL token")
	}
	if (&Server{}).verifyJiraWebhook(httptest.NewRequest("POST", "/?token=", nil), body) {
		t.Fatal("unset credentials opened route")
	}
}

func TestJiraWebhookRouteExistsOnlyOnAccessListener(t *testing.T) {
	s := &Server{}
	lan := httptest.NewRecorder()
	s.Handler().ServeHTTP(lan, httptest.NewRequest("POST", "/v1alpha1/webhooks/jira", nil))
	if lan.Code != 404 {
		t.Fatalf("LAN status = %d, want 404", lan.Code)
	}
	access := httptest.NewRecorder()
	s.AccessHandler().ServeHTTP(access, httptest.NewRequest("POST", "/v1alpha1/webhooks/jira", nil))
	if access.Code != 401 {
		t.Fatalf("Access status = %d, want closed-route 401", access.Code)
	}
}

func TestJiraReplayMatchesPythonSweepSeam(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is absent")
	}
	fixture := filepath.Join("testdata", "jira_issue.json")
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var issue map[string]any
	if err := json.Unmarshal(body, &issue); err != nil {
		t.Fatal(err)
	}
	got := jiraEmissions(issue, "team.example.com", "SCRUM", "bot")
	script := `import json,sys;sys.path.insert(0,'examples/pr-upkeep');import pr_upkeep_jira as j;i=json.load(open(sys.argv[1]));print(json.dumps(j.jira_emissions({'issues':[i]},site='team.example.com',project='SCRUM',bot_account_id='bot'),sort_keys=True,separators=(',',':')))`
	cmd := exec.Command(python, "-c", script, filepath.Join("internal", "api", fixture))
	cmd.Dir = filepath.Join("..", "..")
	wantBytes, err := cmd.Output()
	if err != nil {
		t.Fatalf("python parity oracle: %v", err)
	}
	var want []map[string]any
	if err := json.Unmarshal(wantBytes, &want); err != nil {
		t.Fatal(err)
	}
	var normalized []map[string]any
	for _, fact := range got {
		var payload, watermark any
		_ = json.Unmarshal(fact.Payload, &payload)
		_ = json.Unmarshal(fact.Watermark, &watermark)
		normalized = append(normalized, map[string]any{"name": fact.Name, "payload": payload, "source_key": fact.SourceKey, "watermark": watermark, "subject": fact.Subject})
	}
	if !reflect.DeepEqual(normalized, want) {
		a, _ := json.MarshalIndent(normalized, "", "  ")
		b, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("Go/Python replay differs\nGo: %s\nPython: %s", a, b)
	}
}

func TestJiraIssueKeysAreOrderIndependentAndUnique(t *testing.T) {
	a := jiraIssueKeys(map[string]any{"issue": map[string]any{"key": "SCRUM-2"}, "other": []any{map[string]any{"issueKey": "SCRUM-1"}, map[string]any{"key": "SCRUM-2"}}})
	if !reflect.DeepEqual(a, []string{"SCRUM-1", "SCRUM-2"}) {
		t.Fatalf("keys = %v", a)
	}
}
