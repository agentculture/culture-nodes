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
//
// GitHub is ranked per repository (task t30b, owner decision d4): a public
// repository is AudiencePublic, a private one AudienceOrg. The engine reads a
// repository's visibility from a namespace-scoped table an operator sets
// (never from GitHub at firing time), through the RepositoryVisibility
// lookup below. An unrecorded repository fails closed in whichever direction
// protects data: as a TARGET it is public (the widest), as a SOURCE it is
// org (the narrower), so neither an unknown destination nor an unknown
// origin can open a flow the owner has not seen.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

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
	"human.requested":           SystemEngine,
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

// Visibility is what the namespace knows about a GitHub repository.
type Visibility string

const (
	// VisibilityUnknown: no row records the repository.
	VisibilityUnknown Visibility = ""
	VisibilityPublic  Visibility = "public"
	VisibilityPrivate Visibility = "private"
)

// RepositoryVisibility looks a repository up (lower-case owner/name). A nil
// lookup knows no repository.
type RepositoryVisibility func(repository string) Visibility

func (f RepositoryVisibility) of(repository string) Visibility {
	if f == nil || repository == "" {
		return VisibilityUnknown
	}
	return f(strings.ToLower(repository))
}

// Sensitivity is a variable's mark, or an action's target: a system, its
// audience rank, and for GitHub the repository the rank was decided for.
type Sensitivity struct {
	System     System
	Audience   Audience
	Repository string
}

func (s Sensitivity) String() string {
	if s.Repository != "" {
		return fmt.Sprintf("%s %s (%s audience)", s.System, s.Repository, s.Audience)
	}
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

// githubSensitivity ranks one repository. target selects the fail-closed
// direction for an unknown repository: public as a target, org as a source.
func githubSensitivity(repository string, vis RepositoryVisibility, target bool) Sensitivity {
	s := Sensitivity{System: SystemGitHub, Audience: AudienceOrg, Repository: strings.ToLower(repository)}
	switch vis.of(repository) {
	case VisibilityPublic:
		s.Audience = AudiencePublic
	case VisibilityPrivate:
		s.Audience = AudienceOrg
	default:
		if target {
			s.Audience = AudiencePublic
		}
	}
	return s
}

// SourceSensitivity marks a variable that may have come from any of the named
// kinds, with no repository known: SourceSensitivityIn("", nil, ...).
func SourceSensitivity(kindNames ...string) Sensitivity {
	return SourceSensitivityIn("", nil, kindNames...)
}

// SourceSensitivityIn marks a variable that may have come from any of the
// named kinds: the NARROWEST of their audiences, since the mark must protect
// the most sensitive possible source. A GitHub kind ranks by repository (an
// unknown one is org). An unknown kind, or no kind at all, is restricted.
func SourceSensitivityIn(repository string, vis RepositoryVisibility, kindNames ...string) Sensitivity {
	out := sensitivityOf(SystemUnknown)
	for i, k := range kindNames {
		s, ok := kindSystem[k]
		if !ok {
			return sensitivityOf(SystemUnknown)
		}
		c := sensitivityOf(s)
		if s == SystemGitHub {
			c = githubSensitivity(repository, vis, false)
		}
		if i == 0 || c.Audience < out.Audience {
			out = c
		}
	}
	return out
}

// TargetSensitivity is the audience an action of this kind renders into, with
// no repository known: TargetSensitivityIn(kind, "", nil).
func TargetSensitivity(actionKind string) Sensitivity {
	return TargetSensitivityIn(actionKind, "", nil)
}

// TargetSensitivityIn is the audience an action of this kind renders into. A
// GitHub action ranks by the repository it writes to (an unknown one is
// public). An unregistered action kind is public, the widest audience.
func TargetSensitivityIn(actionKind, repository string, vis RepositoryVisibility) Sensitivity {
	s, ok := kindSystem[actionKind]
	if !ok {
		return Sensitivity{System: SystemUnknown, Audience: AudiencePublic}
	}
	if s == SystemGitHub {
		return githubSensitivity(repository, vis, true)
	}
	return sensitivityOf(s)
}

// RepositoryInputKey is the action-input field naming the owner/name
// repository a github.* action writes to: the key the github bridge requires
// (adapters/github/src/github_bridge/mapping.py), and the variable GitHub
// events carry (internal/api/githubwebhook.go).
const RepositoryInputKey = "repository"

// ActionRepository is the repository a RENDERED action writes to: its
// with.input.repository, lower-cased. A value still holding a template
// reference (an unrendered action, e.g. at publish) names no repository.
func ActionRepository(a Action) string {
	var with struct {
		Input map[string]any `json:"input"`
	}
	if len(a.With) == 0 || json.Unmarshal(a.With, &with) != nil {
		return ""
	}
	repo, _ := with.Input[RepositoryInputKey].(string)
	if t, err := template.Parse(repo); err != nil || len(t.References()) > 0 {
		return ""
	}
	return strings.ToLower(repo)
}

// VariableRepository is the repository a variable set (an event's payload, or
// a lineage firing's variables) names, if any.
func VariableRepository(vars map[string]any) string {
	repo, _ := vars[RepositoryInputKey].(string)
	return strings.ToLower(repo)
}

// Widens reports whether rendering a variable marked source into target
// exposes it to a wider audience than it came from.
func Widens(source, target Sensitivity) bool {
	return target.Audience > source.Audience
}

// TriggerSensitivity marks the variables of the event that triggered d: step
// 0, the current firing's own variables. repository is the event's own
// repository variable (VariableRepository), "" when it has none.
func TriggerSensitivity(d Declaration, repository string, vis RepositoryVisibility) Sensitivity {
	return SourceSensitivityIn(repository, vis, d.Trigger.Kind)
}

// FiringSensitivity marks the variables a fired declaration exposes to later
// declarations in its lineage: its trigger event's variables overlaid by its
// action's result, so the narrower of the two systems. Each is ranked by its
// own repository: eventRepository is the trigger event's repository
// variable, actionRepository the one the action's result names (a github
// bridge reports the repository it posted to). Ranking both by the overlaid
// value would let a public target mask a private source (t30b review F1).
func FiringSensitivity(d Declaration, eventRepository, actionRepository string, vis RepositoryVisibility) Sensitivity {
	trigger := SourceSensitivityIn(eventRepository, vis, d.Trigger.Kind)
	action := SourceSensitivityIn(actionRepository, vis, d.Action.Kind)
	if action.Audience < trigger.Audience {
		return action
	}
	return trigger
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
// Entry is the reference's exposes entry; Listed says whether d lists it.
type Widening struct {
	Reference   template.Reference
	Entry       string
	Listed      bool
	Source      Sensitivity
	Target      Sensitivity
	SourceKnown bool
}

// Warning renders w as the publish-time warning text. exposure is the
// entry's state as the caller resolved it: "unlisted", or for a listed entry
// its approval state ("listed" when no approval task exists yet, "pending",
// "approved", "refused", "withdrawn"), optionally followed by detail.
func (w Widening) Warning(exposure string) string {
	if w.Entry == "" {
		w.Entry = ExposureEntry(w.Reference)
	}
	// Written without braces, so it never reads as the reference warnings
	// (internal/api/declarations.go) that quote a template verbatim.
	ref := fmt.Sprintf("to step %s variable %q (exposes entry %q)", w.Reference.Step, w.Reference.Name, w.Entry)
	var flow string
	if !w.SourceKnown {
		flow = fmt.Sprintf("sensitivity: action.with reference %s has a source known only at firing time and renders into %s; if that widens its audience", ref, w.Target)
	} else {
		flow = fmt.Sprintf("sensitivity: action.with reference %s comes from %s and renders into the wider %s;", ref, w.Source, w.Target)
	}
	if !w.Listed {
		return fmt.Sprintf("%s exposure unlisted: firing is blocked; add %q to exposes to request the owner's approval", flow, w.Entry)
	}
	return fmt.Sprintf("%s exposure listed, %s", flow, exposure)
}

// WideningReferences is the static, publish-time analysis: every action.with
// reference of d that widens (or, with an unresolvable source, may widen) its
// variable's audience. resolve maps a named step to that declaration's
// current published body; vis ranks any GitHub repository the action names
// literally (a templated repository is unknown, so public as a target).
// Duplicate references are reported once.
func WideningReferences(d Declaration, resolve func(name string) (Declaration, bool), vis RepositoryVisibility) ([]Widening, error) {
	refs, err := ActionReferences(d.Action)
	if err != nil {
		return nil, err
	}
	target := TargetSensitivityIn(d.Action.Kind, ActionRepository(d.Action), vis)
	seen := map[string]bool{}
	var out []Widening
	for _, ref := range refs {
		entry := ExposureEntry(ref)
		if seen[entry] {
			continue
		}
		seen[entry] = true
		w := Widening{Reference: ref, Entry: entry, Listed: d.Lists(entry), Target: target, Source: sensitivityOf(SystemUnknown)}
		if ref.Step == "0" {
			w.Source, w.SourceKnown = TriggerSensitivity(d, "", vis), true
		} else if _, numeric := strconv.Atoi(ref.Step); numeric != nil && resolve != nil {
			if named, ok := resolve(ref.Step); ok {
				w.Source, w.SourceKnown = FiringSensitivity(named, "", "", vis), true
			}
		}
		if Widens(w.Source, w.Target) {
			out = append(out, w)
		}
	}
	return out, nil
}
