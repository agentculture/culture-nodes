package preflight_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/preflight"
)

// The gate's configuration-time refusal (task t14, acceptance criterion 3)
// and the deterministic composition of the document an actor acknowledges.
//
// Criterion 3 is the SAFETY property of this task: there is no preflight or
// acknowledgement surface anywhere in internal/ or cmd/ today, so a gate
// that fails closed for everyone would stop all ten registered actors on the
// day it merges. Every test below that asserts "off" is asserting that.

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return out
}

// surfaceJSON is a well-formed advertised capability surface: the protocol
// version this engine speaks, and one host fact.
func surfaceJSON() json.RawMessage {
	return json.RawMessage(`{"preflight":{"protocol_version":"1.0","host":{"hostname":"test-host","sandbox_modes":["read-only"]}}}`)
}

// --- the gate is off unless a deployment turns it on ------------------------

func TestGateIsOffForAnActorThatConfiguresNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata json.RawMessage
	}{
		{"absent metadata", nil},
		{"empty object", json.RawMessage(`{}`)},
		{"unrelated keys only", json.RawMessage(`{"auth_token_env":"NODES_ACTOR_TOKEN"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate, err := preflight.ParseGate(tc.metadata)
			if err != nil {
				t.Fatalf("ParseGate: %v", err)
			}
			if gate.Enabled {
				t.Error("gate is enabled for an actor that configured nothing; it must be default-off")
			}
			// And the same metadata must survive registration, with or
			// without an advertised surface: the ten actors registered
			// before this shipped have neither.
			if err := preflight.CheckConfiguration(nil, tc.metadata); err != nil {
				t.Errorf("CheckConfiguration refused an ungated actor: %v", err)
			}
			if err := preflight.CheckConfiguration(surfaceJSON(), tc.metadata); err != nil {
				t.Errorf("CheckConfiguration refused an ungated actor that advertises a surface: %v", err)
			}
		})
	}
}

// TestEnablingTheGateWithoutASurfaceIsRefusedAtConfigurationTime is
// acceptance criterion 3's first half: the refusal happens when the actor is
// REGISTERED, not when a run later stalls against it.
func TestEnablingTheGateWithoutASurfaceIsRefusedAtConfigurationTime(t *testing.T) {
	for _, tc := range []struct {
		name         string
		capabilities json.RawMessage
		wantIn       string
	}{
		{"no capabilities at all", nil, "advertises no preflight capability surface"},
		{"capabilities without the block", json.RawMessage(`{"streaming":true}`), "advertises no preflight capability surface"},
		{"surface is not an object", json.RawMessage(`{"preflight":"yes"}`), "must be an object"},
		{"unsupported protocol version", json.RawMessage(`{"preflight":{"protocol_version":"9.0","host":{"hostname":"h"}}}`), "protocol version"},
		{"no host block", json.RawMessage(`{"preflight":{"protocol_version":"1.0"}}`), "host"},
		{"empty host block", json.RawMessage(`{"preflight":{"protocol_version":"1.0","host":{}}}`), "at least one host fact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := preflight.CheckConfiguration(tc.capabilities, raw(t, map[string]any{
				"preflight_gate": map[string]any{"enabled": true},
			}))
			var cfgErr *preflight.ConfigError
			if !errors.As(err, &cfgErr) {
				t.Fatalf("CheckConfiguration error = %v (%T), want a *ConfigError refusal", err, err)
			}
			if !strings.Contains(cfgErr.Error(), tc.wantIn) {
				t.Errorf("refusal = %q, want it to name %q", cfgErr.Error(), tc.wantIn)
			}
			if cfgErr.Remediation == "" {
				t.Error("refusal carries no remediation; a configuration refusal must say what to do about it")
			}
		})
	}
}

func TestEnablingTheGateWithASurfaceIsAccepted(t *testing.T) {
	metadata := raw(t, map[string]any{"preflight_gate": map[string]any{"enabled": true, "window_seconds": 600}})
	if err := preflight.CheckConfiguration(surfaceJSON(), metadata); err != nil {
		t.Fatalf("CheckConfiguration: %v", err)
	}
	gate, err := preflight.ParseGate(metadata)
	if err != nil {
		t.Fatalf("ParseGate: %v", err)
	}
	if !gate.Enabled {
		t.Error("gate is not enabled after being explicitly enabled")
	}
	if gate.Window() != 10*time.Minute {
		t.Errorf("window = %s, want the declared 600s", gate.Window())
	}
}

func TestAnUnconfiguredWindowIsTheDocumentedDefault(t *testing.T) {
	gate, err := preflight.ParseGate(json.RawMessage(`{"preflight_gate":{"enabled":true}}`))
	if err != nil {
		t.Fatalf("ParseGate: %v", err)
	}
	if gate.Window() != preflight.DefaultWindow {
		t.Errorf("window = %s, want the default %s", gate.Window(), preflight.DefaultWindow)
	}
}

// A malformed gate block is refused rather than read as "off": an operator
// who wrote `"enabled": "true"` meant to enable it, and silently dispatching
// ungated would be the failure this gate exists to prevent, made quiet.
func TestAMalformedGateBlockIsRefusedRatherThanReadAsOff(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata string
	}{
		{"gate is not an object", `{"preflight_gate":true}`},
		{"enabled is a string", `{"preflight_gate":{"enabled":"true"}}`},
		{"window is a string", `{"preflight_gate":{"enabled":true,"window_seconds":"600"}}`},
		{"window below the floor", `{"preflight_gate":{"enabled":true,"window_seconds":5}}`},
		{"window above the ceiling", `{"preflight_gate":{"enabled":true,"window_seconds":9999999}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := preflight.ParseGate(json.RawMessage(tc.metadata)); err == nil {
				t.Fatal("ParseGate accepted a malformed gate block")
			}
			if err := preflight.CheckConfiguration(surfaceJSON(), json.RawMessage(tc.metadata)); err == nil {
				t.Error("CheckConfiguration accepted a malformed gate block")
			}
		})
	}
}

// --- the composed document --------------------------------------------------

func testTask() preflight.Task {
	deadline := time.Date(2026, 8, 14, 12, 30, 0, 0, time.UTC)
	return preflight.Task{
		RunID:          "run_01",
		NodeRunID:      "nr_01",
		NodeID:         "fix",
		NodeKind:       "agent",
		ActorRef:       "actor://company/codex-thor",
		ActorKey:       "company/codex-thor",
		ActorID:        "actor_row_01",
		WorkflowName:   "pr-upkeep",
		WorkflowDigest: "sha256:" + strings.Repeat("a", 64),
		ContractDigest: "sha256:" + strings.Repeat("b", 64),
		Outcomes:       []string{"fixed", "changes_required"},
		Deadline:       &deadline,
	}
}

func mustSurface(t *testing.T) preflight.Surface {
	t.Helper()
	surface, ok, err := preflight.ParseSurface(surfaceJSON())
	if err != nil || !ok {
		t.Fatalf("ParseSurface: ok=%v err=%v", ok, err)
	}
	return surface
}

// TestComposeStatesTheFactsTheTaskDependsOn pins what the document must
// carry: the host capabilities as the BRIDGE advertised them, the task
// declaration, the expected terminal shape, and — the property inherited
// from the destructive protocol — a refusal that states what does not
// proceed.
func TestComposeStatesTheFactsTheTaskDependsOn(t *testing.T) {
	issued := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	doc := preflight.Compose(mustSurface(t), testTask(), issued, preflight.DefaultWindow)

	if doc.Verdict != preflight.VerdictHold {
		t.Errorf("verdict = %q, want %q: the document defaults to hold, exactly like the destructive protocol's file",
			doc.Verdict, preflight.VerdictHold)
	}
	if doc.ExpiresAt != issued.Add(preflight.DefaultWindow) {
		t.Errorf("expires_at = %s, want issued + %s", doc.ExpiresAt, preflight.DefaultWindow)
	}
	if doc.Refusal == "" || !strings.Contains(doc.Refusal, "not been invoked") {
		t.Errorf("refusal = %q, want it to state that the actor has not been invoked", doc.Refusal)
	}
	if doc.Acknowledgement.Verb == "" || doc.Acknowledgement.Endpoint == "" {
		t.Errorf("acknowledgement = %+v, want both the verb and the endpoint that commit it", doc.Acknowledgement)
	}

	// The host block is the bridge's, copied verbatim: the engine states who
	// said it, never re-renders it.
	var host map[string]any
	if err := json.Unmarshal(doc.HostCapabilities, &host); err != nil {
		t.Fatalf("decode host capabilities: %v", err)
	}
	if host["hostname"] != "test-host" {
		t.Errorf("host capabilities = %v, want the advertised block verbatim", host)
	}
	if doc.CapabilityProtocolVersion != preflight.ProtocolVersion {
		t.Errorf("capability protocol version = %q, want %q", doc.CapabilityProtocolVersion, preflight.ProtocolVersion)
	}

	if doc.Task.NodeID != "fix" || doc.Task.ActorKey != "company/codex-thor" {
		t.Errorf("task declaration = %+v, want the node and actor this dispatch is addressed to", doc.Task)
	}
	if len(doc.ExpectedResult.Outcomes) != 2 || doc.ExpectedResult.ContractDigest == "" {
		t.Errorf("expected result = %+v, want the declared outcomes and the pinned contract digest", doc.ExpectedResult)
	}
}

// The composition is DERIVED, which is a claim about determinism as much as
// about authority: the same inputs must produce the same document, byte for
// byte, or "derived" would be decoration.
func TestComposeIsDeterministic(t *testing.T) {
	issued := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	first := preflight.Compose(mustSurface(t), testTask(), issued, preflight.DefaultWindow)
	second := preflight.Compose(mustSurface(t), testTask(), issued, preflight.DefaultWindow)

	a, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("two compositions of the same inputs differ:\n%s\n%s", a, b)
	}
}

// A bridge that could not measure anything still advertises the block; the
// composer must not invent a fact to fill it, and must not fail either.
func TestComposeCarriesAnEmptyHostBlockRatherThanInventingFacts(t *testing.T) {
	surface := preflight.Surface{ProtocolVersion: preflight.ProtocolVersion}
	doc := preflight.Compose(surface, testTask(), time.Now().UTC(), preflight.DefaultWindow)
	if string(doc.HostCapabilities) != "{}" {
		t.Errorf("host capabilities = %s, want an empty object rather than a fabricated one", doc.HostCapabilities)
	}
}

// --- the two record shapes --------------------------------------------------

// The builders exist so the dispatch site and the confirm verb cannot
// disagree about the records' shape. These pin the two facts about them that
// are not negotiable: their authorities.
func TestTheRecordBuildersFixTheAuthorities(t *testing.T) {
	issued := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	doc := preflight.Compose(mustSurface(t), testTask(), issued, preflight.DefaultWindow)

	pre, err := preflight.NewPreflightRecord(doc, "")
	if err != nil {
		t.Fatalf("NewPreflightRecord: %v", err)
	}
	if pre.RecordType != ledger.RecordDispatchPreflight {
		t.Errorf("record type = %q, want %q", pre.RecordType, ledger.RecordDispatchPreflight)
	}
	if pre.Authority != ledger.AuthorityDerived || pre.Origin.Kind != ledger.OriginEngine {
		t.Errorf("preflight origin/authority = %s/%s, want engine/derived", pre.Origin.Kind, pre.Authority)
	}
	if pre.Origin.ActorID != preflight.DispatchGateActorID {
		t.Errorf("producer = %q, want the gate's own identity %q", pre.Origin.ActorID, preflight.DispatchGateActorID)
	}
	if pre.RunID != "run_01" || pre.NodeRunID.String() != "nr_01" {
		t.Errorf("preflight run/node run = %s/%s, want the dispatch it briefs", pre.RunID, pre.NodeRunID)
	}

	ack, err := preflight.NewAcknowledgementRecord(preflight.AcknowledgementInput{
		RunID:             "run_01",
		NodeRunID:         "nr_01",
		PreflightRecordID: "ledger_1",
		PreflightDigest:   "sha256:" + strings.Repeat("c", 64),
		OriginActorID:     "actor_row_01",
		AcknowledgedBy:    "actor_row_01",
	})
	if err != nil {
		t.Fatalf("NewAcknowledgementRecord: %v", err)
	}
	if ack.Authority != ledger.AuthorityProposed || ack.Origin.Kind != ledger.OriginAgent {
		t.Errorf("acknowledgement origin/authority = %s/%s, want agent/proposed",
			ack.Origin.Kind, ack.Authority)
	}
	if ack.SubjectRef.String() != "ledger_1" || len(ack.ProvenanceRefs) != 1 {
		t.Errorf("acknowledgement = %+v, want it to point at the briefing it answers", ack)
	}
}

// An acknowledgement that names no briefing is refused before it can reach
// the ledger: the digest is what makes the record worth anything.
func TestAnAcknowledgementMustNameTheBriefing(t *testing.T) {
	if _, err := preflight.NewAcknowledgementRecord(preflight.AcknowledgementInput{
		RunID:             "run_01",
		PreflightRecordID: "ledger_1",
	}); err == nil {
		t.Error("an acknowledgement with no preflight digest was built")
	}
}
