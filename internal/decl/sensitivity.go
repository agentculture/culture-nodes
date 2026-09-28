package decl

// Variable sensitivity (task t30, #328; spec owner decision q22, ADR 0014's
// loop/safety limits): "Variables carry a sensitivity marking; a variable's
// sensitivity cannot be elevated (exposed more widely, e.g. rendered from
// Jira into a wider Discord audience) without the owner's approval."
//
// The whole model is ONE table, kindSystem below, sitting next to the kind
// vocabulary it classifies (internal/decl/kinds): every registered trigger and
// action kind names the external system it reads from or writes to, and every
// system has an audience rank. A variable's mark is the audience of the system
// that produced it (its SOURCE kind); an action's target audience is the
// audience of the system it renders into. Rendering a variable into a target
// whose audience is wider than its mark is a widening, and only the variable's
// owner may approve one (internal/declengine/sensitivity.go enforces that at
// firing time; publish only warns).
//
// Everything fails closed: a kind absent from the table marks its variables
// AudienceRestricted (narrower than any real target, so every render of them
// is a widening), and an action kind absent from the table targets
// AudiencePublic (wider than any real source). TestSensitivityTableCoversEveryKind
// pins that every registered kind is classified, so a new kind cannot land
// without a deliberate row here.

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/agentculture/culture-nodes/internal/decl/template"
)

// Audience ranks how widely a system exposes what is written to it. Higher
// is wider. The zero value is the fail-closed restricted mark.
type Audience int

const (
	// AudienceRestricted marks a variable whose source is unknown: rendering
	// it anywhere is a widening.
	AudienceRestricted Audience = iota
	// AudienceOperators is a person's own answer or a runner's raw result:
	// seen by the control plane's operators and nobody else.
	AudienceOperators
	// AudienceTeam is the working team: the tracker (Jira), agent sessions,
	// and the control plane's own engine records.
	AudienceTeam
	// AudienceOrg is the code host (GitHub): the organization and, for a
	// public repository, anyone who reads it.
	AudienceOrg
	// AudiencePublic is a community chat (Discord): the widest audience.
	AudiencePublic
)

func (a Audience) String() string {
	switch a {
	case AudienceRestricted:
		return "restricted"
	case AudienceOperators:
		return "operators"
	case AudienceTeam:
		return "team"
	case AudienceOrg:
		return "org"
	case AudiencePublic:
		return "public"
	}
	return "audience(" + strconv.Itoa(int(a)) + ")"
}

// System names the external system a kind reads from or writes to.
type System string

const (
	SystemUnknown System = "unknown"
	SystemHuman   System = "human"
	SystemCode    System = "code"
	SystemEngine  System = "engine"
	SystemJira    System = "jira"
	SystemAgent   System = "agent"
	SystemGitHub  System = "github"
	SystemDiscord System = "discord"
)

var systemAudience = map[System]Audience{
	SystemUnknown: AudienceRestricted,
	SystemHuman:   AudienceOperators,
	SystemCode:    AudienceOperators,
	SystemEngine:  AudienceTeam,
	SystemJira:    AudienceTeam,
	SystemAgent:   AudienceTeam,
	SystemGitHub:  AudienceOrg,
	SystemDiscord: AudiencePublic,
}

// kindSystem is the ranking table: every registered trigger and action kind
// (a trigger and an action sharing a name, e.g. jira.comment, share a system).
var kindSystem = map[string]System{
	// actions
	"agent.work":          SystemAgent,
	"discord.post":        SystemDiscord,
	"github.comment":      SystemGitHub,
	"github.review_reply": SystemGitHub,
	"jira.comment":        SystemJira,
	"jira.transition":     SystemJira,
	"jira.create":         SystemJira,
	"code.run":            SystemCode,
	"human.ask":           SystemHuman,
	"activate":            SystemEngine,
	// triggers
	"github.pr.approved":        SystemGitHub,
	"github.pr.created":         SystemGitHub,
	"jira.issue.created":        SystemJira,
	"human.decision":            SystemHuman,
	"code.result":               SystemCode,
	"pr-upkeep.pr":              SystemGitHub,
	"agent.result":              SystemAgent,
	"jira.issue.transitioned":   SystemJira,
	"node.expired":              SystemEngine,
	"action.failed":             SystemEngine,
	"action.timed_out":          SystemEngine,
	"action.rejected":           SystemEngine,
	"action.capacity_exhausted": SystemEngine,
	"action.budget_exhausted":   SystemEngine,
	"timer":                     SystemEngine,
	"declaration.proposed":      SystemEngine,
	"declaration.overlap":       SystemEngine,
}

// Sensitivity is a variable's mark, or an action's target: a system and its
// audience rank.
type Sensitivity struct {
	System   System
	Audience Audience
}

func (s Sensitivity) String() string {
	return fmt.Sprintf("%s (%s audience)", s.System, s.Audience)
}

// KindSystem returns the system a registered kind belongs to.
func KindSystem(kind string) (System, bool) {
	s, ok := kindSystem[kind]
	return s, ok
}

func sensitivityOf(s System) Sensitivity {
	return Sensitivity{System: s, Audience: systemAudience[s]}
}

// SourceSensitivity marks a variable that may have come from any of the named
// kinds: the NARROWEST of their audiences, since the mark must protect the
// most sensitive possible source. An unknown kind, or no kind at all, is
// restricted.
func SourceSensitivity(kindNames ...string) Sensitivity {
	out := sensitivityOf(SystemUnknown)
	for i, k := range kindNames {
		s, ok := kindSystem[k]
		if !ok {
			return sensitivityOf(SystemUnknown)
		}
		if c := sensitivityOf(s); i == 0 || c.Audience < out.Audience {
			out = c
		}
	}
	return out
}

// TargetSensitivity is the audience an action of this kind renders into. An
// unregistered action kind is treated as public, the widest audience.
func TargetSensitivity(actionKind string) Sensitivity {
	s, ok := kindSystem[actionKind]
	if !ok {
		return Sensitivity{System: SystemUnknown, Audience: AudiencePublic}
	}
	return sensitivityOf(s)
}

// Widens reports whether rendering a variable marked source into target
// exposes it to a wider audience than it came from.
func Widens(source, target Sensitivity) bool {
	return target.Audience > source.Audience
}

// TriggerSensitivity marks the variables of the event that triggered d: step
// 0, the current firing's own variables.
func TriggerSensitivity(d Declaration) Sensitivity {
	return SourceSensitivity(d.Trigger.Kind)
}

// FiringSensitivity marks the variables a fired declaration exposes to later
// declarations in its lineage: its trigger event's variables overlaid by its
// action's result, so the narrower of the two systems.
func FiringSensitivity(d Declaration) Sensitivity {
	return SourceSensitivity(d.Trigger.Kind, d.Action.Kind)
}

// ActionReferences returns every template reference in an action's `with`
// payload, in document order for arrays and unspecified order across object
// keys -- every string value, at any depth, is a template (the same walk the
// engine's renderAction performs).
func ActionReferences(a Action) ([]template.Reference, error) {
	if len(a.With) == 0 {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(a.With, &value); err != nil {
		return nil, err
	}
	var refs []template.Reference
	var walk func(any) error
	walk = func(v any) error {
		switch x := v.(type) {
		case string:
			t, err := template.Parse(x)
			if err != nil {
				return err
			}
			refs = append(refs, t.References()...)
		case []any:
			for _, e := range x {
				if err := walk(e); err != nil {
					return err
				}
			}
		case map[string]any:
			for _, e := range x {
				if err := walk(e); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(value); err != nil {
		return nil, err
	}
	return refs, nil
}

// Widening is one action.with reference whose variable, if present, would
// reach a wider audience than it came from. SourceKnown is false when the
// source cannot be resolved without a firing's lineage (a numeric step, or a
// named step naming no published declaration): the mark is then restricted.
type Widening struct {
	Reference   template.Reference
	Source      Sensitivity
	Target      Sensitivity
	SourceKnown bool
}

// Warning renders w as the publish-time warning text.
func (w Widening) Warning() string {
	ref := fmt.Sprintf("step %s variable %q", w.Reference.Step, w.Reference.Name)
	if !w.SourceKnown {
		return fmt.Sprintf("sensitivity: action.with reference to %s has a source known only at firing time and renders into %s; if it widens its audience the firing is blocked until the variable's owner approves", ref, w.Target)
	}
	return fmt.Sprintf("sensitivity: action.with reference to %s comes from %s and renders into the wider %s; firing is blocked until the variable's owner approves", ref, w.Source, w.Target)
}

// WideningReferences is the static, publish-time analysis: every action.with
// reference of d that widens (or, with an unresolvable source, may widen) its
// variable's audience. resolve maps a named step to that declaration's
// current published body. Duplicate references are reported once.
func WideningReferences(d Declaration, resolve func(name string) (Declaration, bool)) ([]Widening, error) {
	refs, err := ActionReferences(d.Action)
	if err != nil {
		return nil, err
	}
	target := TargetSensitivity(d.Action.Kind)
	seen := map[string]bool{}
	var out []Widening
	for _, ref := range refs {
		key := ref.Step + "\x00" + ref.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		w := Widening{Reference: ref, Target: target, Source: sensitivityOf(SystemUnknown)}
		if ref.Step == "0" {
			w.Source, w.SourceKnown = TriggerSensitivity(d), true
		} else if _, numeric := strconv.Atoi(ref.Step); numeric != nil && resolve != nil {
			if named, ok := resolve(ref.Step); ok {
				w.Source, w.SourceKnown = FiringSensitivity(named), true
			}
		}
		if Widens(w.Source, w.Target) {
			out = append(out, w)
		}
	}
	return out, nil
}
