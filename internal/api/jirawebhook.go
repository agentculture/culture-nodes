package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

type jiraWebhookConfig struct {
	secret, token                 []byte
	apiBase, site, project        string
	email, apiToken, botAccountID string
	client                        *http.Client
}

type jiraFact struct {
	Name      string          `json:"name"`
	SourceKey string          `json:"source_key"`
	Subject   string          `json:"subject"`
	Payload   json.RawMessage `json:"payload"`
	Watermark json.RawMessage `json:"watermark"`
}

type jiraWebhookDelivery struct {
	Fact     jiraFact         `json:"fact"`
	Delivery EventDeliveryOut `json:"delivery"`
}

func (s *Server) handleJiraWebhook(w http.ResponseWriter, r *http.Request) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		return badRequest("send a readable Jira webhook body", "read body: %v", err)
	}
	if !s.verifyJiraWebhook(r, body) {
		return unauthorized("configure and present the Jira webhook secret or URL token", "Jira webhook authentication failed")
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return badRequest("send Jira webhook JSON", "decode Jira webhook: %v", err)
	}
	keys := jiraIssueKeys(payload)
	if len(keys) == 0 {
		return badRequest("include a Jira issue key in the webhook payload", "no issue key found")
	}
	deliveries := make([]jiraWebhookDelivery, 0)
	for _, key := range keys {
		issue, err := s.fetchJiraIssue(r.Context(), key)
		if err != nil {
			return internalError(err)
		}
		for _, fact := range jiraEmissions(issue, s.jiraWebhook.site, projectForIssue(s.jiraWebhook.project, key), s.jiraWebhook.botAccountID) {
			delivery, err := s.deliverJiraFact(r.Context(), fact)
			if err != nil {
				return internalError(err)
			}
			deliveries = append(deliveries, jiraWebhookDelivery{Fact: fact, Delivery: delivery})
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"issues": keys, "deliveries": deliveries})
	return nil
}

func (s *Server) verifyJiraWebhook(r *http.Request, body []byte) bool {
	c := s.jiraWebhook
	if signature := r.Header.Get("X-Hub-Signature"); signature != "" {
		if len(c.secret) == 0 {
			return false
		}
		mac := hmac.New(sha256.New, c.secret)
		_, _ = mac.Write(body)
		decoded, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
		return err == nil && hmac.Equal(decoded, mac.Sum(nil))
	}
	if len(c.token) == 0 {
		return false
	}
	presented, expected := sha256.Sum256([]byte(r.URL.Query().Get("token"))), sha256.Sum256(c.token)
	return subtle.ConstantTimeCompare(presented[:], expected[:]) == 1
}

func jiraIssueKeys(value any) []string {
	set := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if k == "key" || k == "issueKey" || k == "issue_key" {
					if value, ok := child.(string); ok && validJiraKey(value) {
						set[value] = true
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(value)
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validJiraKey(key string) bool {
	i := strings.LastIndexByte(key, '-')
	if i < 1 || i == len(key)-1 {
		return false
	}
	for _, r := range key[:i] {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	_, err := strconv.ParseUint(key[i+1:], 10, 64)
	return err == nil
}

func projectForIssue(configured, key string) string {
	if configured != "" {
		return configured
	}
	if i := strings.IndexByte(key, '-'); i > 0 {
		return key[:i]
	}
	return ""
}

func (s *Server) fetchJiraIssue(ctx context.Context, key string) (map[string]any, error) {
	c := s.jiraWebhook
	base := strings.TrimRight(c.apiBase, "/")
	if base == "" && c.site != "" {
		base = "https://" + strings.TrimRight(strings.TrimPrefix(c.site, "https://"), "/")
	}
	if base == "" || c.email == "" || c.apiToken == "" {
		return nil, fmt.Errorf("Jira API hydration is not configured")
	}
	fields := "summary,description,priority,status,issuetype,created,updated,comment"
	var issue map[string]any
	endpoint := base + "/rest/api/3/issue/" + url.PathEscape(key) + "?fields=" + url.QueryEscape(fields) + "&expand=changelog"
	if err := c.getJSON(ctx, endpoint, &issue); err != nil {
		return nil, err
	}
	if err := c.hydrateCollection(ctx, base, key, issue, "changelog"); err != nil {
		return nil, err
	}
	if err := c.hydrateCollection(ctx, base, key, issue, "comment"); err != nil {
		return nil, err
	}
	return issue, nil
}

func (c jiraWebhookConfig) getJSON(ctx context.Context, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.email+":"+c.apiToken)))
	req.Header.Set("Accept", "application/json")
	client := c.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("read Jira issue: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("read Jira issue: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func (c jiraWebhookConfig) hydrateCollection(ctx context.Context, base, key string, issue map[string]any, kind string) error {
	var page map[string]any
	if kind == "changelog" {
		page = object(issue["changelog"])
	} else {
		page = object(object(issue["fields"])["comment"])
	}
	entryKey, valuesKey := "histories", "values"
	if kind == "comment" {
		entryKey, valuesKey = "comments", "comments"
	}
	entries, total := array(page[entryKey]), int(number(page["total"]))
	for len(entries) < total {
		var next map[string]any
		endpoint := fmt.Sprintf("%s/rest/api/3/issue/%s/%s?startAt=%d&maxResults=100", base, url.PathEscape(key), kind, len(entries))
		if err := c.getJSON(ctx, endpoint, &next); err != nil {
			return err
		}
		values := array(next[valuesKey])
		if len(values) == 0 {
			break
		}
		entries = append(entries, values...)
		if n := int(number(next["total"])); n > 0 {
			total = n
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return historyLess(text(object(entries[i])["id"]), text(object(entries[j])["id"]))
	})
	page[entryKey] = entries
	return nil
}

// jiraEmissions is a Go port of pr_upkeep_jira.jira_emissions,
// jira_watermark, and jira_history_facts. Keep it in parity with those Python
// functions; the push receiver deliberately leaves the sweep module untouched.
func jiraEmissions(issue map[string]any, site, project, bot string) []jiraFact {
	fields, key := object(issue["fields"]), text(issue["key"])
	description := jiraText(fields["description"])
	base := map[string]any{"source": "jira", "id": key, "project": project, "severity": fallback(text(object(fields["priority"])["name"]), "Medium"), "kind": fallback(text(object(fields["issuetype"])["name"]), "Jira issue"), "file": "", "line": nil, "title": text(fields["summary"]), "description": truncate(description, 4000), "description_truncated": len([]rune(description)) > 4000, "status": text(object(fields["status"])["name"]), "details_url": "https://" + jiraSite(site) + "/browse/" + url.PathEscape(key)}
	type entry struct {
		created  string
		order    int
		id, kind string
		value    map[string]any
	}
	creation := map[string]any{"id": "0", "created": text(fields["created"]), "items": []any{map[string]any{"field": "status", "fromString": "", "toString": text(object(fields["status"])["name"])}}}
	timeline := []entry{{text(fields["created"]), -1, "0", "changelog", creation}}
	for _, value := range array(object(issue["changelog"])["histories"]) {
		x := object(value)
		timeline = append(timeline, entry{text(x["created"]), 0, text(x["id"]), "changelog", x})
	}
	for _, value := range array(object(fields["comment"])["comments"]) {
		x, created := object(value), text(object(value)["created"])
		if created == "" {
			created = text(x["updated"])
		}
		timeline = append(timeline, entry{created, 1, text(x["id"]), "comment", x})
	}
	sort.SliceStable(timeline, func(i, j int) bool {
		a, b := timeline[i], timeline[j]
		if a.created != b.created {
			return a.created < b.created
		}
		if a.order != b.order {
			return a.order < b.order
		}
		return historyLess(a.id, b.id)
	})
	var facts []jiraFact
	changeID, commentID := "", ""
	var seen []map[string]any
	for _, current := range timeline {
		if current.kind == "changelog" {
			changeID = current.id
		} else {
			commentID = current.id
			seen = append(seen, current.value)
		}
		watermark, payload, name := marshal(map[string]any{"changelog_id": changeID, "comment_id": commentID}), clone(base), ""
		if current.kind == "comment" {
			if commentIsSelfEcho(current.value, bot) {
				continue
			}
			if q := questionID(seen); q != "" {
				payload["originating_question_id"] = q
			}
			payload["answer"], name = map[string]any{"comment_id": current.id, "body": jiraText(current.value["body"])}, "pr-upkeep.jira.comment"
		} else {
			var status map[string]any
			items := array(current.value["items"])
			for _, item := range items {
				x := object(item)
				if text(x["field"]) == "status" {
					status = x
					break
				}
			}
			if status != nil && bot != "" && accountID(current.value) == bot {
				continue
			}
			payload["changelog_id"], payload["actor_account_id"] = current.id, accountID(current.value)
			if status == nil {
				payload["changes"], name = items, "pr-upkeep.jira.changed"
			} else {
				payload["status"], payload["from_status"] = text(status["toString"]), text(status["fromString"])
				name = "pr-upkeep.jira.transitioned." + statusSlug(text(status["toString"]))
			}
		}
		facts = append(facts, jiraFact{Name: name, Payload: marshal(payload), SourceKey: "jira:" + jiraSite(site) + ":" + key + ":history:" + current.kind + ":" + current.id, Watermark: watermark, Subject: key})
	}
	return facts
}

func (s *Server) deliverJiraFact(ctx context.Context, fact jiraFact) (EventDeliveryOut, error) {
	d, err := s.Store.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{NamespaceID: s.NamespaceID, Name: fact.Name, Payload: fact.Payload, Emitter: "jira-webhook", Pickup: s.Engine, Trigger: s.Engine, SourceKey: fact.SourceKey, Watermark: fact.Watermark, Subject: fact.Subject})
	if err != nil {
		return EventDeliveryOut{}, err
	}
	ev := d.Event
	return EventDeliveryOut{Event: SignalEventOut{ID: ev.ID, Name: ev.Name, RunID: ev.RunID, Payload: ev.Payload, Emitter: ev.Emitter, CreatedAt: ev.CreatedAt}, Resumed: []ResumedSubscriptionOut{}, PickedUp: []EventPickupOut{}, Triggered: d.Triggered, Duplicate: d.Duplicate}, nil
}

func object(v any) map[string]any {
	if x, ok := v.(map[string]any); ok {
		return x
	}
	return map[string]any{}
}
func array(v any) []any {
	if x, ok := v.([]any); ok {
		return x
	}
	return nil
}
func text(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
func number(v any) float64 { x, _ := v.(float64); return x }
func fallback(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
func marshal(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func clone(v map[string]any) map[string]any {
	b, _ := json.Marshal(v)
	var x map[string]any
	_ = json.Unmarshal(b, &x)
	return x
}
func jiraSite(site string) string {
	return strings.TrimPrefix(strings.TrimSuffix(site, "/"), "https://")
}
func historyLess(a, b string) bool {
	ai, ae := strconv.ParseUint(a, 10, 64)
	bi, be := strconv.ParseUint(b, 10, 64)
	if ae == nil && be == nil {
		return ai < bi
	}
	if ae == nil {
		return true
	}
	if be == nil {
		return false
	}
	return a < b
}
func accountID(v map[string]any) string { return text(object(v["author"])["accountId"]) }
func commentIsSelfEcho(comment map[string]any, bot string) bool {
	if bot != "" {
		return accountID(comment) == bot
	}
	return strings.Contains(jiraText(comment["body"]), "culture-nodes:jira-actor")
}
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
func jiraText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if s, ok := x["text"].(string); ok {
				b.WriteString(s)
			}
			for _, child := range array(x["content"]) {
				walk(child)
			}
			kind := text(x["type"])
			if (kind == "blockquote" || kind == "heading" || kind == "listItem" || kind == "paragraph") && b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
				b.WriteByte('\n')
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(v)
	return strings.TrimSpace(b.String())
}
func statusSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unspecified"
	}
	return out
}
func questionID(comments []map[string]any) string {
	ordered := append([]map[string]any(nil), comments...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := text(ordered[i]["updated"]), text(ordered[j]["updated"])
		if a == "" {
			a = text(ordered[i]["created"])
		}
		if b == "" {
			b = text(ordered[j]["created"])
		}
		return a < b
	})
	if len(ordered) == 0 || strings.Contains(jiraText(ordered[len(ordered)-1]["body"]), "culture-nodes:jira-actor") {
		return ""
	}
	for i := len(ordered) - 2; i >= 0; i-- {
		body, marker := jiraText(ordered[i]["body"]), "[culture-nodes:jira-actor question_id="
		if p := strings.Index(body, marker); p >= 0 {
			rest := body[p+len(marker):]
			if end := strings.IndexByte(rest, ']'); end > 0 {
				return rest[:end]
			}
		}
	}
	return ""
}
