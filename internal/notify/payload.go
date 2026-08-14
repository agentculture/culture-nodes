package notify

import (
	"encoding/json"
	"fmt"
)

// maxTitleChars and maxDescriptionChars are defensive trims kept well
// under Discord's actual caps (embed title 256, embed description /
// message content 4096, but Discord treats a message's total character
// budget as ~2000) — matching devex's own defensive margin, not Discord's
// documented ceiling exactly, so a near-limit payload never gets the POST
// rejected.
const (
	maxTitleChars       = 256
	maxDescriptionChars = 1900
)

// ellipsis marks a trimmed field so a truncated title/description reads
// as truncated rather than silently cut off mid-word.
const ellipsis = "…"

// Payload is the entire set of fields a run-lifecycle notification may
// carry. This is the boundary from the economy-discord-graphs spec: "the
// notifier posts minimal metadata only — run id, workflow name, state,
// actor, and a dashboard link — never ledger records, node output,
// instructions, or diffs." Payload's struct shape enforces this
// structurally, not just by convention — BuildMessage has no other
// parameter it could read richer content from, and
// TestPayloadHasExactlyFiveFields in payload_test.go pins the field list
// so a future field can't be added here without that test forcing a
// conscious look at this comment.
type Payload struct {
	// RunID identifies the run this notification is about.
	RunID string
	// Workflow is the workflow's name (not its content — no node
	// definitions, no schema, no input/output shape).
	Workflow string
	// Event is the run-lifecycle state or event name (e.g.
	// "run.completed", "run.waiting") — a label, never a domain outcome
	// payload or a node's output.
	Event string
	// Actor is the actor identity associated with the event, when one
	// applies (may be empty for a run-level event with no single actor).
	Actor string
	// DashboardLink is a URL into the dashboard for a human to click
	// through to full detail — the notification's only pointer to
	// anything richer than these five fields.
	DashboardLink string
}

// discordEmbed and discordEmbedField mirror the subset of Discord's embed
// object this package needs: title, description, and a flat field list.
// See https://discord.com/developers/docs/resources/channel#embed-object
// for the full shape; nothing else in it is used here.
type discordEmbed struct {
	Title       string              `json:"title"`
	Description string              `json:"description"`
	Fields      []discordEmbedField `json:"fields"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type discordMessage struct {
	Content string         `json:"content"`
	Embeds  []discordEmbed `json:"embeds"`
}

// trim cuts s to at most limit runes, appending ellipsis when it had to
// cut. Counts runes, not bytes, so a multi-byte character is never split
// mid-encoding.
func trim(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	if limit <= len([]rune(ellipsis)) {
		return string([]rune(ellipsis)[:limit])
	}
	return string(runes[:limit-len([]rune(ellipsis))]) + ellipsis
}

// BuildMessage shapes payload for delivery to rawURL: a Discord embed
// envelope when rawURL classifies as a Discord webhook (IsDiscordURL),
// otherwise a generic flat-JSON object any other webhook receiver can
// read without knowing Discord's format. Every value in either shape
// comes from payload's five fields and nothing else — see Payload's doc
// comment for why that is a structural guarantee, not just a convention
// followed here.
func BuildMessage(rawURL string, payload Payload) ([]byte, error) {
	if IsDiscordURL(rawURL) {
		return json.Marshal(buildDiscordMessage(payload))
	}
	return json.Marshal(buildGenericMessage(payload))
}

// buildDiscordMessage renders p as one embed. An empty Actor is omitted
// entirely — from the description's parenthetical and from the field list
// — rather than rendered blank: a code-node or wait-node run legitimately
// has no agent actor, and "absent" and "empty" are different facts, only
// one of which is worth a line (issue #66).
func buildDiscordMessage(p Payload) discordMessage {
	title := trim(fmt.Sprintf("%s — %s", p.Workflow, p.Event), maxTitleChars)
	description := fmt.Sprintf("Run %s reached %s", p.RunID, p.Event)
	if p.Actor != "" {
		description += fmt.Sprintf(" (actor: %s)", p.Actor)
	}
	content := trim(fmt.Sprintf("%s: %s", p.Workflow, p.Event), maxTitleChars)

	fields := []discordEmbedField{
		{Name: "Run", Value: p.RunID, Inline: true},
		{Name: "Workflow", Value: p.Workflow, Inline: true},
		{Name: "Event", Value: p.Event, Inline: true},
	}
	if p.Actor != "" {
		fields = append(fields, discordEmbedField{Name: "Actor", Value: p.Actor, Inline: true})
	}
	fields = append(fields, discordEmbedField{Name: "Dashboard", Value: p.DashboardLink, Inline: false})

	return discordMessage{
		Content: content,
		Embeds: []discordEmbed{
			{
				Title:       title,
				Description: trim(description, maxDescriptionChars),
				Fields:      fields,
			},
		},
	}
}

// genericMessage is the flat-JSON shape sent to a non-Discord webhook
// receiver. Field names are snake_case to match the wire convention this
// codebase's other JSON APIs use (see internal/api). Actor is omitempty
// for the same reason the Discord embed drops a blank actor field: a
// receiver should be able to tell "this run has no agent actor" from "this
// actor's name is the empty string".
type genericMessage struct {
	RunID         string `json:"run_id"`
	Workflow      string `json:"workflow"`
	Event         string `json:"event"`
	Actor         string `json:"actor,omitempty"`
	DashboardLink string `json:"dashboard_link"`
}

func buildGenericMessage(p Payload) genericMessage {
	return genericMessage{
		RunID:         p.RunID,
		Workflow:      p.Workflow,
		Event:         p.Event,
		Actor:         p.Actor,
		DashboardLink: p.DashboardLink,
	}
}
