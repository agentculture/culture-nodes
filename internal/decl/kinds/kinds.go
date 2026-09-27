// Package kinds defines the closed, versioned action and trigger vocabularies.
package kinds

// ArtifactType is a member of the versioned declaration artifact vocabulary.
type ArtifactType string

const (
	ArtifactNone           ArtifactType = "none"
	ArtifactAgentWork      ArtifactType = "agent.work"
	ArtifactCodeResult     ArtifactType = "code.result"
	ArtifactDiscordMessage ArtifactType = "discord.message"
	ArtifactGitHubPR       ArtifactType = "github.pr"
	ArtifactGitHubComment  ArtifactType = "github.comment"
	ArtifactJiraIssue      ArtifactType = "jira.issue"
	ArtifactJiraComment    ArtifactType = "jira.comment"
	ArtifactHumanDecision  ArtifactType = "human.decision"
	ArtifactTimer          ArtifactType = "timer"
	ArtifactNode           ArtifactType = "node"
	ArtifactActionResult   ArtifactType = "action.result"
	ArtifactDeclaration    ArtifactType = "declaration"
)

// Kind is one registered action or trigger. Version versions both its contract
// and its signature; consumers must match on both Name and Version.
type Kind struct {
	Name     string
	Version  uint32
	Consumes []ArtifactType
	Produces []ArtifactType
}

var actions = []Kind{
	{Name: "agent.work", Version: 1, Consumes: []ArtifactType{ArtifactNone}, Produces: []ArtifactType{ArtifactGitHubPR}},
	{Name: "discord.post", Version: 1, Consumes: []ArtifactType{ArtifactNone}, Produces: []ArtifactType{ArtifactDiscordMessage}},
	{Name: "github.comment", Version: 1, Consumes: []ArtifactType{ArtifactGitHubPR}, Produces: []ArtifactType{ArtifactGitHubComment}},
	{Name: "github.review_reply", Version: 1, Consumes: []ArtifactType{ArtifactGitHubPR}, Produces: []ArtifactType{ArtifactGitHubComment}},
	{Name: "jira.comment", Version: 1, Consumes: []ArtifactType{ArtifactJiraIssue}, Produces: []ArtifactType{ArtifactJiraComment}},
	{Name: "jira.transition", Version: 1, Consumes: []ArtifactType{ArtifactJiraIssue}, Produces: []ArtifactType{ArtifactJiraIssue}},
	{Name: "jira.create", Version: 1, Consumes: []ArtifactType{ArtifactNone}, Produces: []ArtifactType{ArtifactJiraIssue}},
	{Name: "code.run", Version: 1, Consumes: []ArtifactType{ArtifactNone}, Produces: []ArtifactType{ArtifactCodeResult}},
	{Name: "human.ask", Version: 1, Consumes: []ArtifactType{ArtifactNone}, Produces: []ArtifactType{ArtifactHumanDecision}},
	// activate is the action of an activation declaration (internal/declengine/activation.go):
	// it names another declaration to make active. Its own authorization is a
	// root-of-trust rule (only a human principal, one level deep), not the
	// produces/consumes signature below, which exists only so the kind is a
	// registered, versioned citizen of this vocabulary like every other action.
	{Name: "activate", Version: 1, Consumes: []ArtifactType{ArtifactDeclaration}, Produces: []ArtifactType{ArtifactDeclaration}},
}

var triggers = []Kind{
	{Name: "github.pr.approved", Version: 1, Consumes: []ArtifactType{ArtifactGitHubPR}, Produces: []ArtifactType{ArtifactGitHubPR}},
	{Name: "github.pr.created", Version: 1, Consumes: []ArtifactType{ArtifactGitHubPR}, Produces: []ArtifactType{ArtifactGitHubPR}},
	{Name: "jira.issue.created", Version: 1, Consumes: []ArtifactType{ArtifactJiraIssue}, Produces: []ArtifactType{ArtifactJiraIssue}},
	{Name: "jira.comment", Version: 1, Consumes: []ArtifactType{ArtifactJiraComment}, Produces: []ArtifactType{ArtifactJiraComment}},
	{Name: "human.decision", Version: 1, Consumes: []ArtifactType{ArtifactHumanDecision}, Produces: []ArtifactType{ArtifactHumanDecision}},
	{Name: "node.expired", Version: 1, Consumes: []ArtifactType{ArtifactNode}, Produces: []ArtifactType{ArtifactNode}},
	{Name: "action.failed", Version: 1, Consumes: []ArtifactType{ArtifactActionResult}, Produces: []ArtifactType{ArtifactActionResult}},
	{Name: "action.timed_out", Version: 1, Consumes: []ArtifactType{ArtifactActionResult}, Produces: []ArtifactType{ArtifactActionResult}},
	{Name: "action.rejected", Version: 1, Consumes: []ArtifactType{ArtifactActionResult}, Produces: []ArtifactType{ArtifactActionResult}},
	{Name: "action.capacity_exhausted", Version: 1, Consumes: []ArtifactType{ArtifactActionResult}, Produces: []ArtifactType{ArtifactActionResult}},
	// action.budget_exhausted (task t11, #328): the trigger a declaration
	// reacting to a blocked dispatch takes. Unlike the four action.* kinds
	// above, no run and no actor invocation ever happened -- the firing's
	// own node/machine/declaration/alias budget check (internal/declengine
	// /budget.go) refused it before dispatch -- but it shares the family's
	// signature so a declaration can route it exactly like a technical
	// action failure.
	{Name: "action.budget_exhausted", Version: 1, Consumes: []ArtifactType{ArtifactActionResult}, Produces: []ArtifactType{ArtifactActionResult}},
	{Name: "timer", Version: 1, Consumes: []ArtifactType{ArtifactTimer}, Produces: []ArtifactType{ArtifactTimer}},
	{Name: "declaration.proposed", Version: 1, Consumes: []ArtifactType{ArtifactDeclaration}, Produces: []ArtifactType{ArtifactDeclaration}},
	{Name: "declaration.overlap", Version: 1, Consumes: []ArtifactType{ArtifactDeclaration}, Produces: []ArtifactType{ArtifactDeclaration}},
}

// Actions returns a copy of the registered action kinds.
func Actions() []Kind { return clone(actions) }

// Triggers returns a copy of the registered trigger kinds.
func Triggers() []Kind { return clone(triggers) }

func clone(src []Kind) []Kind {
	out := make([]Kind, len(src))
	for i, kind := range src {
		out[i] = kind
		out[i].Consumes = append([]ArtifactType(nil), kind.Consumes...)
		out[i].Produces = append([]ArtifactType(nil), kind.Produces...)
	}
	return out
}

// Action returns the registered action kind with this name.
func Action(name string) (Kind, bool) { return find(actions, name) }

// Trigger returns the registered trigger kind with this name.
func Trigger(name string) (Kind, bool) { return find(triggers, name) }

func find(src []Kind, name string) (Kind, bool) {
	for _, kind := range src {
		if kind.Name == name {
			return clone([]Kind{kind})[0], true
		}
	}
	return Kind{}, false
}
