package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

type githubWebhookConfig struct{ secret []byte }

type githubWebhookEvent struct {
	Name      string          `json:"name"`
	SourceKey string          `json:"source_key"`
	Subject   string          `json:"subject"`
	Payload   json.RawMessage `json:"payload"`
	Watermark json.RawMessage `json:"-"`
}

type githubWebhookDelivery struct {
	Fact     githubWebhookEvent `json:"fact"`
	Delivery EventDeliveryOut   `json:"delivery"`
}

// WithGitHubWebhook configures the GitHub webhook signing secret. An empty
// secret leaves the endpoint closed.
func WithGitHubWebhook(secret string) Option {
	return func(s *Server) { s.githubWebhook = githubWebhookConfig{secret: []byte(secret)} }
}

func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		return badRequest("send a readable GitHub webhook body", "read body: %v", err)
	}
	if !s.verifyGitHubWebhook(r, body) {
		s.log.Warn("GitHub webhook refused", "reason", "signature verification failed", "delivery", r.Header.Get("X-GitHub-Delivery"))
		return unauthorized("configure the GitHub webhook secret and send a valid X-Hub-Signature-256", "GitHub webhook authentication failed")
	}
	deliveryID := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	if deliveryID == "" {
		return badRequest("include X-GitHub-Delivery", "GitHub webhook delivery id is missing")
	}
	fact, emit, err := githubWebhookFact(r.Header.Get("X-GitHub-Event"), body, deliveryID)
	if err != nil {
		return badRequest("send a valid GitHub webhook payload", "decode GitHub webhook: %v", err)
	}
	if !emit {
		writeJSON(w, http.StatusOK, map[string]any{"ignored": true})
		return nil
	}
	delivery, err := s.deliverGitHubFact(r.Context(), fact)
	if err != nil {
		return internalError(err)
	}
	writeJSON(w, http.StatusCreated, githubWebhookDelivery{Fact: fact, Delivery: delivery})
	return nil
}

func (s *Server) verifyGitHubWebhook(r *http.Request, body []byte) bool {
	if len(s.githubWebhook.secret) == 0 {
		return false
	}
	signature := strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
	if signature == "" || len(signature) != sha256.Size*2 {
		return false
	}
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, s.githubWebhook.secret)
	_, _ = mac.Write(body)
	return subtle.ConstantTimeCompare(got, mac.Sum(nil)) == 1
}

func githubWebhookFact(event string, body []byte, deliveryID string) (githubWebhookEvent, bool, error) {
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		return githubWebhookEvent{}, false, err
	}
	action := text(p["action"])
	name := ""
	switch {
	case event == "pull_request_review" && action == "submitted" && text(object(p["review"])["state"]) == "approved":
		name = "github.pr.approved"
	case event == "pull_request" && action == "opened":
		name = "github.pr.created"
	default:
		return githubWebhookEvent{}, false, nil
	}
	repo := text(object(p["repository"])["full_name"])
	pr := object(p["pull_request"])
	number := text(pr["number"])
	if repo == "" || number == "" || deliveryID == "" {
		return githubWebhookEvent{}, false, fmt.Errorf("repository.full_name, pull_request.number and delivery id are required")
	}
	payload := map[string]any{
		"source": "github", "repository": repo, "id": number,
		"title": text(pr["title"]), "url": text(pr["html_url"]),
		"author": text(object(pr["user"])["login"]),
	}
	if name == "github.pr.approved" {
		review := object(p["review"])
		reviewID := text(review["id"])
		if reviewID == "" {
			return githubWebhookEvent{}, false, fmt.Errorf("review.id is required for approval")
		}
		payload["reviewer"] = text(object(review["user"])["login"])
		return githubWebhookEvent{
			Name: name, SourceKey: "github:" + repo + ":pr:" + number + ":review:" + reviewID + ":approved",
			Subject: repo + "#" + number, Payload: marshal(payload),
			Watermark: marshal(map[string]string{"review_id": reviewID}),
		}, true, nil
	}
	return githubWebhookEvent{Name: name, SourceKey: "github:" + deliveryID, Subject: repo + "#" + number, Payload: marshal(payload), Watermark: marshal(map[string]string{"source_key": "github:" + deliveryID})}, true, nil
}

func (s *Server) deliverGitHubFact(ctx context.Context, fact githubWebhookEvent) (EventDeliveryOut, error) {
	d, err := s.Store.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{
		NamespaceID: s.NamespaceID, Name: fact.Name, Payload: fact.Payload,
		Emitter: "github-webhook", SourceKey: fact.SourceKey, Watermark: fact.Watermark, Subject: fact.Subject,
		Pickup: s.Engine, Trigger: s.Engine,
	})
	if err != nil {
		return EventDeliveryOut{}, err
	}
	ev := d.Event
	return EventDeliveryOut{Event: SignalEventOut{ID: ev.ID, Name: ev.Name, RunID: ev.RunID, Payload: ev.Payload, Emitter: ev.Emitter, CreatedAt: ev.CreatedAt}, Resumed: []ResumedSubscriptionOut{}, PickedUp: []EventPickupOut{}, Triggered: d.Triggered, Duplicate: d.Duplicate}, nil
}
