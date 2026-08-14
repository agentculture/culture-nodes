package notify_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/notify"
)

const discordWebhookURL = "https://discord.com/api/webhooks/123456/token-abc"
const genericWebhookURL = "https://hooks.example.com/incoming/xyz"

// TestPayloadHasExactlyFiveFields pins Payload's shape by reflection. This
// is the structural half of the c40 boundary ("no embed field derives from
// ledger records, node output, or workflow input"): BuildMessage has no
// parameter besides Payload it could read richer content from, so keeping
// Payload to exactly these five string fields is what makes the boundary
// true by construction rather than by convention. A PR that adds a sixth
// field here must also touch this test — that friction is deliberate.
func TestPayloadHasExactlyFiveFields(t *testing.T) {
	typ := reflect.TypeOf(notify.Payload{})

	wantFields := []string{"RunID", "Workflow", "Event", "Actor", "DashboardLink"}
	if typ.NumField() != len(wantFields) {
		t.Fatalf("Payload has %d fields, want exactly %d (%v) — a field was added or removed", typ.NumField(), len(wantFields), wantFields)
	}
	for i, name := range wantFields {
		f := typ.Field(i)
		if f.Name != name {
			t.Errorf("field %d = %q, want %q", i, f.Name, name)
		}
		if f.Type.Kind() != reflect.String {
			t.Errorf("field %q has kind %v, want string", f.Name, f.Type.Kind())
		}
	}
}

func TestBuildMessageGenericShapeHasExactlyThePayloadFields(t *testing.T) {
	payload := notify.Payload{
		RunID:         "run_abc123",
		Workflow:      "deliver-change",
		Event:         "run.completed",
		Actor:         "codex-thor",
		DashboardLink: "https://dashboard.example/runs/run_abc123",
	}

	raw, err := notify.BuildMessage(genericWebhookURL, payload)
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := map[string]any{
		"run_id":         payload.RunID,
		"workflow":       payload.Workflow,
		"event":          payload.Event,
		"actor":          payload.Actor,
		"dashboard_link": payload.DashboardLink,
	}
	if len(got) != len(want) {
		t.Fatalf("generic message has %d top-level keys %v, want exactly %d %v", len(got), keysOf(got), len(want), keysOf(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q = %v, want %v", k, got[k], v)
		}
	}
	if strings.Contains(string(raw), "embeds") {
		t.Fatalf("a non-Discord URL must never get the Discord embed envelope: %s", raw)
	}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// discordWireShape mirrors just enough of the wire JSON to inspect it,
// independent of notify's own (unexported) discordMessage type — so this
// test is checking the actual bytes BuildMessage produced, not reusing
// production code to grade itself.
type discordWireShape struct {
	Content string `json:"content"`
	Embeds  []struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Fields      []struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Inline bool   `json:"inline"`
		} `json:"fields"`
	} `json:"embeds"`
}

// TestBuildMessageDiscordFieldsTraceOnlyToPayload is the behavioral half
// of the c40 boundary proof: every field value in the built embed, and
// every character of the title/description/content once the five payload
// values are stripped back out, comes from Payload and nothing else. Each
// payload field is a unique sentinel token, so any leftover text after
// removing all five sentinels pins the exact (small, static) template —
// proving nothing else was interpolated in.
func TestBuildMessageDiscordFieldsTraceOnlyToPayload(t *testing.T) {
	payload := notify.Payload{
		RunID:         "SENTINEL-RUNID",
		Workflow:      "SENTINEL-WORKFLOW",
		Event:         "SENTINEL-EVENT",
		Actor:         "SENTINEL-ACTOR",
		DashboardLink: "SENTINEL-DASHBOARD",
	}

	raw, err := notify.BuildMessage(discordWebhookURL, payload)
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}

	var msg discordWireShape
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msg.Embeds) != 1 {
		t.Fatalf("want exactly one embed, got %d", len(msg.Embeds))
	}
	embed := msg.Embeds[0]

	if len(embed.Fields) != 5 {
		t.Fatalf("want exactly 5 embed fields, got %d: %+v", len(embed.Fields), embed.Fields)
	}
	wantFields := []struct {
		Name   string
		Value  string
		Inline bool
	}{
		{"Run", payload.RunID, true},
		{"Workflow", payload.Workflow, true},
		{"Event", payload.Event, true},
		{"Actor", payload.Actor, true},
		{"Dashboard", payload.DashboardLink, false},
	}
	for i, want := range wantFields {
		got := embed.Fields[i]
		if got.Name != want.Name || got.Value != want.Value || got.Inline != want.Inline {
			t.Errorf("field %d = %+v, want %+v", i, got, want)
		}
	}

	// Strip every sentinel out of title/description/content; what remains
	// must be an exact, known, static skeleton — never additional content.
	strip := func(s string) string {
		for _, sentinel := range []string{payload.RunID, payload.Workflow, payload.Event, payload.Actor, payload.DashboardLink} {
			s = strings.ReplaceAll(s, sentinel, "")
		}
		return s
	}

	if got, want := strip(embed.Title), " — "; got != want {
		t.Errorf("title skeleton (sentinels stripped) = %q, want %q — title carries content beyond Workflow/Event", got, want)
	}
	if got, want := strip(embed.Description), "Run  reached  (actor: )"; got != want {
		t.Errorf("description skeleton (sentinels stripped) = %q, want %q — description carries content beyond RunID/Event/Actor", got, want)
	}
	if got, want := strip(msg.Content), ": "; got != want {
		t.Errorf("content skeleton (sentinels stripped) = %q, want %q — content carries content beyond Workflow/Event", got, want)
	}
}

// TestBuildMessageOmitsAnEmptyActorEntirely pins issue #66's second
// finding at the rendering layer: an empty Actor is a legitimate fact (a
// code-node or wait-node run has no agent actor), and the honest rendering
// omits the field rather than emitting a blank one. Absent and empty are
// different facts, and only one of them is worth a line.
func TestBuildMessageOmitsAnEmptyActorEntirely(t *testing.T) {
	payload := notify.Payload{
		RunID:         "run_1",
		Workflow:      "parallel-live-proof (8d4c768)",
		Event:         "run.created",
		DashboardLink: "https://dashboard.example/runs/run_1",
	}

	discordRaw, err := notify.BuildMessage(discordWebhookURL, payload)
	if err != nil {
		t.Fatalf("BuildMessage(discord): %v", err)
	}
	if strings.Contains(strings.ToLower(string(discordRaw)), "actor") {
		t.Errorf("an empty actor must not render at all, got: %s", discordRaw)
	}

	var msg discordWireShape
	if err := json.Unmarshal(discordRaw, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, want := len(msg.Embeds[0].Fields), 4; got != want {
		t.Errorf("embed has %d fields, want %d (no blank Actor field): %+v", got, want, msg.Embeds[0].Fields)
	}
	if got, want := msg.Embeds[0].Description, "Run run_1 reached run.created"; got != want {
		t.Errorf("description = %q, want %q (no dangling actor parenthetical)", got, want)
	}

	genericRaw, err := notify.BuildMessage(genericWebhookURL, payload)
	if err != nil {
		t.Fatalf("BuildMessage(generic): %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(genericRaw, &generic); err != nil {
		t.Fatalf("unmarshal generic: %v", err)
	}
	if _, present := generic["actor"]; present {
		t.Errorf(`generic body carries an "actor" key when there is no actor: %s`, genericRaw)
	}
}

// TestBuildMessageNeverMentionsForbiddenVocabulary is a defensive,
// grep-style guard (matching this codebase's neutrality_test.go idiom):
// even though Payload cannot structurally carry ledger/node-output/
// workflow-input content today, this test names the forbidden vocabulary
// explicitly so a future change that widens Payload or BuildMessage's
// inputs trips an readable failure here instead of only the more abstract
// reflection test above.
func TestBuildMessageNeverMentionsForbiddenVocabulary(t *testing.T) {
	payload := notify.Payload{
		RunID:         "run_1",
		Workflow:      "deliver-change",
		Event:         "run.completed",
		Actor:         "codex-thor",
		DashboardLink: "https://dashboard.example/runs/run_1",
	}

	forbidden := []string{"ledger", "node_output", "workflow_input", "instructions", "diff", "claim"}

	for _, rawURL := range []string{discordWebhookURL, genericWebhookURL} {
		raw, err := notify.BuildMessage(rawURL, payload)
		if err != nil {
			t.Fatalf("BuildMessage(%q): %v", rawURL, err)
		}
		lower := strings.ToLower(string(raw))
		for _, word := range forbidden {
			if strings.Contains(lower, word) {
				t.Errorf("built message for %q unexpectedly mentions forbidden word %q: %s", rawURL, word, raw)
			}
		}
	}
}

func TestBuildMessageDiscordTrimsLongFieldsWithEllipsis(t *testing.T) {
	long := func(n int) string { return strings.Repeat("x", n) }

	payload := notify.Payload{
		RunID:         "run_1",
		Workflow:      long(2000),
		Event:         long(2000),
		Actor:         long(2000),
		DashboardLink: "https://dashboard.example/runs/run_1",
	}

	raw, err := notify.BuildMessage(discordWebhookURL, payload)
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}

	var msg discordWireShape
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	embed := msg.Embeds[0]

	const maxTitleChars = 256
	const maxDescriptionChars = 1900

	titleRunes := []rune(embed.Title)
	if len(titleRunes) > maxTitleChars {
		t.Errorf("title is %d runes, want <= %d", len(titleRunes), maxTitleChars)
	}
	if !strings.HasSuffix(embed.Title, "…") {
		t.Errorf("truncated title should end with an ellipsis marker, got %q", embed.Title)
	}

	descRunes := []rune(embed.Description)
	if len(descRunes) > maxDescriptionChars {
		t.Errorf("description is %d runes, want <= %d", len(descRunes), maxDescriptionChars)
	}
	if !strings.HasSuffix(embed.Description, "…") {
		t.Errorf("truncated description should end with an ellipsis marker, got %q", embed.Description)
	}

	contentRunes := []rune(msg.Content)
	if len(contentRunes) > maxTitleChars {
		t.Errorf("content is %d runes, want <= %d", len(contentRunes), maxTitleChars)
	}
}

func TestBuildMessageDiscordDoesNotTrimShortFields(t *testing.T) {
	payload := notify.Payload{
		RunID:         "run_1",
		Workflow:      "deliver-change",
		Event:         "run.completed",
		Actor:         "codex-thor",
		DashboardLink: "https://dashboard.example/runs/run_1",
	}

	raw, err := notify.BuildMessage(discordWebhookURL, payload)
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}
	if strings.Contains(string(raw), "…") {
		t.Errorf("short fields should never be trimmed: %s", raw)
	}
}

func TestIsDiscordURLClassificationDrivesShapeSelection(t *testing.T) {
	payload := notify.Payload{RunID: "r", Workflow: "w", Event: "e", Actor: "a", DashboardLink: "d"}

	discordRaw, err := notify.BuildMessage("https://discord.com/api/webhooks/1/tok", payload)
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}
	if !strings.Contains(string(discordRaw), `"embeds"`) {
		t.Errorf("a Discord webhook URL must produce the embed envelope, got %s", discordRaw)
	}

	genericRaw, err := notify.BuildMessage("https://discord.com/not-a-webhook-path", payload)
	if err != nil {
		t.Fatalf("BuildMessage: %v", err)
	}
	if strings.Contains(string(genericRaw), `"embeds"`) {
		t.Errorf("a discord.com URL without /api/webhooks/ must NOT get the embed envelope, got %s", genericRaw)
	}
}
