package preflight

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// ProtocolVersion is the capability-surface version this engine speaks. A
// bridge advertises the version it produces; a version this engine does not
// know is refused at configuration time rather than composed into a document
// whose fields might mean something else.
const ProtocolVersion = "1.0"

// CapabilityKey is the key a bridge advertises its surface under, inside the
// actors row's `capabilities` document. GateKey is the key a deployment
// turns the gate on under, inside the same row's `metadata`.
//
// They are separate documents on purpose (see the package doc): capabilities
// are FACTS supplied by the party that can measure them, metadata is
// CONFIGURATION supplied by the operator. An actor cannot enable its own
// gate, and an operator cannot advertise a host fact on a bridge's behalf.
const (
	CapabilityKey = "preflight"
	GateKey       = "preflight_gate"
)

// The two verdicts, keeping deploy/prod/install-secrets.sh's vocabulary: a
// freshly composed document holds, and an acknowledgement is the edit that
// turns it into a proceed.
const (
	VerdictHold    = "hold"
	VerdictProceed = "proceed"
)

// Window bounds. The default is install-secrets.sh's own
// CONFIRM_WINDOW_SECONDS (900), for the reason that script states: a stale
// confirmation from last week must not authorize today's action.
const (
	DefaultWindow = 15 * time.Minute
	// MinWindow keeps a configured window long enough for the acknowledging
	// party to actually read the document. Anything shorter would make the
	// gate a race rather than a briefing.
	MinWindow = 30 * time.Second
	// MaxWindow bounds how long an acknowledgement stays good. A window of
	// days would let a briefing composed against last week's host state
	// authorize a dispatch onto this week's.
	MaxWindow = 24 * time.Hour
)

// Surface is the capability block a bridge advertises under
// `capabilities.preflight` — the engine-side half of the all-backends
// contract task t15 implements on claude-code, codex, colleague and notify.
//
// Host is deliberately opaque. The facts that matter are backend-specific
// (which sandbox modes actually work on this host, what the commit/harvest
// policy for this lane is, which paths a dispatch may write), and a typed
// struct here would either constrain four backends to one backend's idea of
// a host or drift into a union of everything. So the engine carries the
// block verbatim and states who advertised it; it never re-renders a fact it
// did not measure. See docs and api/actor-protocol/README.md for the keys
// the bridges agree to use.
type Surface struct {
	ProtocolVersion string          `json:"protocol_version"`
	Host            json.RawMessage `json:"host"`
}

// Gate is the per-actor configuration under `metadata.preflight_gate`.
//
// The zero value is the gate OFF, which is what every actor registered
// before this shipped has and what any actor that says nothing gets.
type Gate struct {
	Enabled bool `json:"enabled"`
	// WindowSeconds is how long a composed preflight stays acknowledgeable,
	// and how long an acknowledgement authorizes the dispatch it was made
	// for. Zero means the DefaultWindow.
	WindowSeconds int `json:"window_seconds,omitempty"`
}

// Window is the configured window, or the default when none was declared.
func (g Gate) Window() time.Duration {
	if g.WindowSeconds <= 0 {
		return DefaultWindow
	}
	return time.Duration(g.WindowSeconds) * time.Second
}

// ConfigError is a configuration-time refusal: a registration that asks for
// something the actor cannot satisfy. It carries a remediation for the same
// reason the CLI's error format does — a refusal that does not say what to
// do about it sends the reader to the source.
type ConfigError struct {
	Reason      string
	Remediation string
}

func (e *ConfigError) Error() string {
	if e.Remediation == "" {
		return "preflight: " + e.Reason
	}
	return "preflight: " + e.Reason + "; " + e.Remediation
}

// ParseSurface reads the advertised capability surface out of an actor row's
// `capabilities` document.
//
// It returns ok=false when the block is simply absent, which is not an error:
// an actor that advertises nothing is every actor registered before this
// shipped. A block that is PRESENT but malformed is an error, because a
// bridge that tried to advertise and got it wrong must not be read as one
// that never advertised.
func ParseSurface(capabilities json.RawMessage) (Surface, bool, error) {
	block, ok, err := blockOf(capabilities, CapabilityKey)
	if err != nil || !ok {
		return Surface{}, false, err
	}

	var surface Surface
	if err := json.Unmarshal(block, &surface); err != nil {
		return Surface{}, false, &ConfigError{
			Reason: fmt.Sprintf("capabilities.%s must be an object with a protocol_version and a host block: %v",
				CapabilityKey, err),
			Remediation: fmt.Sprintf(
				`advertise it as {"%s":{"protocol_version":%q,"host":{...}}}`, CapabilityKey, ProtocolVersion),
		}
	}
	if surface.ProtocolVersion != ProtocolVersion {
		return Surface{}, false, &ConfigError{
			Reason: fmt.Sprintf("capabilities.%s declares protocol version %q, which this control plane does not speak",
				CapabilityKey, surface.ProtocolVersion),
			Remediation: fmt.Sprintf("advertise protocol_version %q, or upgrade the control plane", ProtocolVersion),
		}
	}
	if err := checkHost(surface.Host); err != nil {
		return Surface{}, false, err
	}
	return surface, true, nil
}

// checkHost refuses a surface whose host block is missing, not an object, or
// empty.
//
// An empty host block is refused rather than accepted as "this bridge
// measured nothing": every bridge knows at least its own hostname, and a
// gate whose document states no operating fact would refuse a dispatch in
// order to tell the actor nothing. The composer is more permissive than this
// on purpose (see Compose) — it will carry an empty block rather than invent
// a fact — because that path is about honesty at composition time, while
// this one is about whether the gate may be turned on at all.
func checkHost(host json.RawMessage) error {
	remedy := &ConfigError{
		Reason: fmt.Sprintf("capabilities.%s.host must be an object carrying at least one host fact "+
			"(hostname, sandbox modes, commit policy, writable paths, ...)", CapabilityKey),
		Remediation: "advertise the operating facts a dispatched task depends on, so the preflight document " +
			"has something to state; a gate that briefs an actor about nothing refuses dispatch for no gain",
	}
	if len(host) == 0 {
		return remedy
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(host, &fields); err != nil {
		return remedy
	}
	if len(fields) == 0 {
		return &ConfigError{
			Reason:      remedy.Reason + " — it declares at least one host fact nowhere",
			Remediation: remedy.Remediation,
		}
	}
	return nil
}

// ParseGate reads the per-actor gate configuration out of an actor row's
// `metadata` document.
//
// An absent block is the gate off. A PRESENT but malformed block is an
// error rather than an off: an operator who wrote `"enabled": "true"` was
// asking for the gate, and reading that as "off" would silently drop the
// protection they configured — which is the shape of failure this gate
// exists to prevent, made quiet.
func ParseGate(metadata json.RawMessage) (Gate, error) {
	block, ok, err := blockOf(metadata, GateKey)
	if err != nil || !ok {
		return Gate{}, err
	}

	var gate Gate
	decoder := json.NewDecoder(bytes.NewReader(block))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&gate); err != nil {
		return Gate{}, &ConfigError{
			Reason: fmt.Sprintf("metadata.%s must be an object of the form "+
				`{"enabled":true,"window_seconds":900}: %v`, GateKey, err),
			Remediation: "fix the block, or remove it entirely to leave this actor ungated (the default)",
		}
	}
	if gate.WindowSeconds != 0 {
		window := time.Duration(gate.WindowSeconds) * time.Second
		if window < MinWindow || window > MaxWindow {
			return Gate{}, &ConfigError{
				Reason: fmt.Sprintf("metadata.%s.window_seconds is %d, outside the supported range %s-%s",
					GateKey, gate.WindowSeconds, MinWindow, MaxWindow),
				Remediation: fmt.Sprintf("declare a window between %d and %d seconds, or omit it for the default %s",
					int(MinWindow.Seconds()), int(MaxWindow.Seconds()), DefaultWindow),
			}
		}
	}
	return gate, nil
}

// CheckConfiguration is the configuration-time refusal (task t14 acceptance
// criterion 3): enabling the gate for an actor whose registration advertises
// no usable capability surface is refused when the actor is REGISTERED,
// rather than discovered later by a run that stalls against a gate nothing
// can satisfy.
//
// It is called from both doors into the actors table — the API handler, so
// the operator gets a 400 naming the remediation, and the store's
// RegisterActor, so a caller that bypasses the handler is refused the same
// way. Migration 0026 adds the same rule as a CHECK constraint, for the
// third door: raw SQL.
//
// An actor that does not enable the gate is never refused for anything here,
// including advertising a malformed surface. That is deliberate: the surface
// only becomes load-bearing when something depends on it, and refusing an
// ungated registration over an unused block would be this gate breaking
// actors it is not even enabled for.
func CheckConfiguration(capabilities, metadata json.RawMessage) error {
	gate, err := ParseGate(metadata)
	if err != nil {
		return err
	}
	if !gate.Enabled {
		return nil
	}

	if _, ok, err := ParseSurface(capabilities); err != nil {
		return err
	} else if !ok {
		return &ConfigError{
			Reason: fmt.Sprintf("metadata.%s enables the clarify-then-commit gate, but this actor "+
				"advertises no preflight capability surface", GateKey),
			Remediation: fmt.Sprintf("register the actor with capabilities.%s "+
				`{"protocol_version":%q,"host":{...}} — the bridge advertises the host facts; `+
				"until it does, leave the gate off", CapabilityKey, ProtocolVersion),
		}
	}
	return nil
}

// Task is the declaration half of the composition: what the engine knows
// about the dispatch this preflight is for. Every field comes from the
// pinned workflow definition or the run's own state — nothing here is
// supplied by the actor, which is what keeps the composed document derived.
type Task struct {
	RunID     string
	NodeRunID string
	NodeID    string
	NodeKind  string
	// ActorRef is the node's `uses` reference; ActorKey is the identity it
	// resolves to; ActorID is the actors-table row id when the registry
	// could answer, "" when it could not.
	ActorRef string
	ActorKey string
	ActorID  string

	WorkflowName   string
	WorkflowDigest string
	ContractDigest string
	// Outcomes are the domain outcomes the node's contract declares — the
	// "expected terminal shape" the issue asks the preflight to state.
	Outcomes []string
	// Deadline is the attempt's own deadline when the node declares one.
	Deadline *time.Time
}

// TaskDeclaration is the Task as it appears in the document.
// Every field but the three the schema requires (run_id, node_id,
// actor_key) is omitted when empty rather than emitted as "": an empty
// string in a briefing reads as a fact about the dispatch ("its workflow is
// named nothing") where absence reads as what it is. The three required ones
// stay unconditional, so a composition missing them fails schema validation
// loudly instead of shipping a briefing that names no dispatch.
type TaskDeclaration struct {
	RunID     string `json:"run_id"`
	NodeRunID string `json:"node_run_id,omitempty"`
	NodeID    string `json:"node_id"`
	NodeKind  string `json:"node_kind,omitempty"`
	ActorRef  string `json:"actor_ref,omitempty"`
	ActorKey  string `json:"actor_key"`
	ActorID   string `json:"actor_id,omitempty"`
	Workflow  string `json:"workflow,omitempty"`
	// WorkflowDigest pins the immutable definition this run executes, so an
	// acknowledgement is against a graph that cannot have changed since.
	WorkflowDigest string     `json:"workflow_digest,omitempty"`
	Deadline       *time.Time `json:"deadline,omitempty"`
}

// ExpectedResult is what "done" has to look like for this dispatch to be
// accepted: the outcomes the node declares, and the contract digest whose
// schema the output is validated against.
type ExpectedResult struct {
	Outcomes       []string `json:"outcomes"`
	ContractDigest string   `json:"contract_digest,omitempty"`
	Note           string   `json:"note"`
}

// AckInstruction is how the reader commits the second, separate action: the
// verb and the endpoint. Both are stated because the two readers differ —
// a human or an operator agent reaches for the verb, a bridge for the
// endpoint (issue #67's "probably both").
type AckInstruction struct {
	Verb     string `json:"verb"`
	Endpoint string `json:"endpoint"`
	Verdict  string `json:"verdict"`
}

// Document is the composed preflight: the payload of the DERIVED ledger
// record an actor acknowledges before its first billable turn.
//
// Field order here is only readability — the record's canonical JSON form
// sorts keys before digesting.
type Document struct {
	// Verdict is VerdictHold on every composed document, exactly like the
	// destructive protocol's file: composing a preflight never authorizes
	// anything, and the acknowledgement is a separate action by a different
	// party.
	Verdict                   string          `json:"verdict"`
	ProtocolVersion           string          `json:"protocol_version"`
	CapabilityProtocolVersion string          `json:"capability_protocol_version"`
	Task                      TaskDeclaration `json:"task"`
	// HostCapabilities is the bridge's own advertised host block, verbatim.
	HostCapabilities json.RawMessage `json:"host_capabilities"`
	ExpectedResult   ExpectedResult  `json:"expected_result"`
	Acknowledgement  AckInstruction  `json:"acknowledgement"`
	// Refusal states what does not proceed and why — the property inherited
	// from install-secrets.sh's confirmation file, which names what breaks
	// rather than merely asking for a keystroke.
	Refusal   string    `json:"refusal"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Compose builds the preflight document. It is a pure function of its
// arguments — no clock, no lookup, no randomness — which is what lets the
// record it becomes carry `derived` authority honestly.
//
// A surface with no host block composes an EMPTY host object rather than an
// error or a fabricated fact. Compose is the wrong place to refuse: by the
// time it runs, an operator has already enabled the gate against a surface
// CheckConfiguration accepted, and a bridge whose facts went missing between
// registration and dispatch is exactly the case where saying so plainly (an
// empty block a reader can see) beats inventing content.
func Compose(surface Surface, task Task, issuedAt time.Time, window time.Duration) Document {
	host := surface.Host
	if len(host) == 0 {
		host = json.RawMessage(`{}`)
	}
	outcomes := task.Outcomes
	if outcomes == nil {
		outcomes = []string{}
	}
	issuedAt = issuedAt.UTC()

	return Document{
		Verdict:                   VerdictHold,
		ProtocolVersion:           ProtocolVersion,
		CapabilityProtocolVersion: surface.ProtocolVersion,
		Task: TaskDeclaration{
			RunID:          task.RunID,
			NodeRunID:      task.NodeRunID,
			NodeID:         task.NodeID,
			NodeKind:       task.NodeKind,
			ActorRef:       task.ActorRef,
			ActorKey:       task.ActorKey,
			ActorID:        task.ActorID,
			Workflow:       task.WorkflowName,
			WorkflowDigest: task.WorkflowDigest,
			Deadline:       utcOrNil(task.Deadline),
		},
		HostCapabilities: host,
		ExpectedResult: ExpectedResult{
			Outcomes:       outcomes,
			ContractDigest: task.ContractDigest,
			Note: "the dispatch is accepted only if it ends in one of these declared outcomes, " +
				"with output satisfying the pinned contract digest",
		},
		Acknowledgement: AckInstruction{
			Verb:     "nodes dispatch confirm <preflight-id>",
			Endpoint: "POST /v1alpha1/preflights/<preflight-id>/acknowledge",
			Verdict:  VerdictProceed,
		},
		Refusal: fmt.Sprintf(
			"This dispatch does not proceed until this preflight is acknowledged: the actor has "+
				"not been invoked, no session was opened, and nothing billable has happened. "+
				"Acknowledge it to commit the dispatch. The acknowledgement is single-use — the next "+
				"dispatch of node %q needs its own — and it expires at %s, after which this dispatch "+
				"is refused rather than sent.",
			task.NodeID, issuedAt.Add(window).Format(time.RFC3339)),
		IssuedAt:  issuedAt,
		ExpiresAt: issuedAt.Add(window),
	}
}

// utcOrNil normalises an optional timestamp without turning absence into a
// zero time a reader would mistake for the epoch.
func utcOrNil(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// blockOf reads one top-level object key out of a JSON document without
// decoding the rest of it. The actors table's capabilities and metadata
// columns are deliberately open documents (see internal/worker/registry.go's
// authTokenEnvOf), so a typed decode of the whole thing would fail on a key
// this package does not care about.
//
// A document that is absent, empty, or not an object at all yields ok=false
// rather than an error: those are all "this actor said nothing about
// preflight", and none of them is this package's business to police.
func blockOf(document json.RawMessage, key string) (json.RawMessage, bool, error) {
	if len(document) == 0 {
		return nil, false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(document, &fields); err != nil {
		return nil, false, nil
	}
	block, ok := fields[key]
	if !ok || len(block) == 0 || string(block) == "null" {
		return nil, false, nil
	}
	return block, true, nil
}
