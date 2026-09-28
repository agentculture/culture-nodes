package declengine

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/decl"
)

// sensitivityMemory adds the SensitivityBackend view to memoryBackend: one
// upstream firing produced by a Jira declaration authored by "upstream".
type sensitivityMemory struct {
	memoryBackend
	requests []SensitivityApprovalRequest
	status   string
	repos    map[string]decl.Visibility
}

func (m *sensitivityMemory) RepositoryVisibility(_ context.Context, _, repo string) (decl.Visibility, error) {
	return m.repos[repo], nil
}

func (m *sensitivityMemory) VersionSource(_ context.Context, _, versionID string) (SensitivitySource, error) {
	for _, a := range m.active {
		if a.VersionID == versionID {
			return SensitivitySource{DeclarationID: a.ID, VersionID: versionID, Author: "downstream", Declaration: a.Declaration}, nil
		}
	}
	return SensitivitySource{}, nil
}

func (m *sensitivityMemory) FiringSource(_ context.Context, _, firingID string) (SensitivitySource, error) {
	return SensitivitySource{DeclarationID: "jira-intake", VersionID: "jira-intake-v1", Author: "upstream",
		Declaration: decl.Declaration{Name: "jira-intake", Trigger: decl.Trigger{Kind: "jira.issue.created"}, Action: decl.Action{Kind: "jira.comment"}}}, nil
}

func (m *sensitivityMemory) RequestSensitivityApproval(_ context.Context, in SensitivityApprovalRequest) (SensitivityApproval, error) {
	m.requests = append(m.requests, in)
	return SensitivityApproval{ID: "ap1", Owner: in.Owner, Status: m.status}, nil
}

// A lineage variable's owner is the author of the version that produced it,
// not the author of the declaration rendering it; the mark comes from that
// version's kinds. Approved widenings dispatch with the variable rendered.
func TestSensitivityLineageOwnerAndApproval(t *testing.T) {
	a := active("announce")
	a.Declaration.Condition = "true"
	a.Declaration.Action = decl.Action{Kind: "discord.post", With: json.RawMessage(`{"uses":"actor://discord","input":{"text":"{jira-intake:owner} / {jira-intake:owner}"}}`)}
	a.Declaration.Exposes = []string{"jira-intake:owner"}
	m := &sensitivityMemory{status: SensitivityPending}
	m.active = []ActiveDeclaration{a}
	m.ancestors = []Ancestor{{FiringID: "f1", CanonicalID: "f1", DeclarationID: "jira-intake", Name: "jira-intake", Variables: map[string]any{"owner": "carol"}}}
	var rendered string
	e := newTestEngine(t, &m.memoryBackend, dispatchFunc(func(_ context.Context, r DispatchRequest) (DispatchResult, error) {
		rendered = string(r.Action.With)
		return DispatchResult{}, nil
	}))
	e.backend = m
	ev := Event{NamespaceID: "ns", ID: "e1", Kind: "timer", Node: "ready", Origin: OriginEvent{}}
	if err := e.evaluate(context.Background(), ev, a, "", m.ancestors); err != nil {
		t.Fatal(err)
	}
	last := m.steps[len(m.steps)-1]
	if last.Outcome != OutcomeSensitivityBlocked || !strings.Contains(last.Reason, `"upstream"`) {
		t.Fatalf("outcome %q %q, want blocked naming owner upstream", last.Outcome, last.Reason)
	}
	// The same reference twice is one entry: one request, keyed on the
	// declaration's name and the entry, addressed to the upstream author.
	if len(m.requests) != 1 || m.requests[0].Owner != "upstream" || m.requests[0].Variable != "jira-intake:owner" || m.requests[0].DeclarationName != "announce" ||
		m.requests[0].SourceVersionID != "jira-intake-v1" || m.requests[0].Source.System != decl.SystemJira || m.requests[0].Target.System != decl.SystemDiscord {
		t.Fatalf("requests = %+v", m.requests)
	}

	m.status = SensitivityApproved
	if err := e.evaluate(context.Background(), Event{NamespaceID: "ns", ID: "e2", Kind: "timer", Node: "ready"}, a, "", m.ancestors); err != nil {
		t.Fatal(err)
	}
	if last := m.steps[len(m.steps)-1]; last.Outcome != OutcomeFired || !strings.Contains(rendered, "carol / carol") {
		t.Fatalf("approved widening: outcome %q, rendered %s", last.Outcome, rendered)
	}
}

// A refusal blocks exactly like a pending task.
func TestSensitivityRefusalKeepsBlocking(t *testing.T) {
	a := active("announce")
	a.Declaration.Condition = "true"
	a.Declaration.Trigger.Kind = "human.decision"
	a.Declaration.Action = decl.Action{Kind: "agent.work", With: json.RawMessage(`{"uses":"actor://a","input":{"text":"{note}"}}`)}
	a.Declaration.Exposes = []string{"note"}
	m := &sensitivityMemory{status: SensitivityRefused}
	m.active = []ActiveDeclaration{a}
	e := newTestEngine(t, &m.memoryBackend, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		t.Fatal("a refused widening dispatched")
		return DispatchResult{}, nil
	}))
	e.backend = m
	if err := e.evaluate(context.Background(), Event{NamespaceID: "ns", ID: "e1", Kind: "human.decision", Node: "ready", Variables: map[string]any{"note": "private"}}, a, "", nil); err != nil {
		t.Fatal(err)
	}
	last := m.steps[len(m.steps)-1]
	if last.Outcome != OutcomeSensitivityBlocked || !strings.Contains(last.Reason, "refused") || m.requests[0].Owner != "downstream" {
		t.Fatalf("outcome %q %q requests %+v", last.Outcome, last.Reason, m.requests)
	}
}

// Task t30b (d4): a widening reference the declaration does not list in
// exposes blocks WITHOUT opening a task, and the reason tells the author
// what to add. {1:owner} and {jira-intake:owner} are different entries, so
// listing one does not cover the other.
func TestSensitivityUnlistedBlocksWithoutTask(t *testing.T) {
	a := active("announce")
	a.Declaration.Condition = "true"
	a.Declaration.Action = decl.Action{Kind: "discord.post", With: json.RawMessage(`{"uses":"actor://discord","input":{"text":"{jira-intake:owner} {1:owner}"}}`)}
	a.Declaration.Exposes = []string{"jira-intake:owner"}
	m := &sensitivityMemory{status: SensitivityApproved}
	m.active = []ActiveDeclaration{a}
	m.ancestors = []Ancestor{{FiringID: "f1", CanonicalID: "f1", DeclarationID: "jira-intake", Name: "jira-intake", Variables: map[string]any{"owner": "carol"}}}
	e := newTestEngine(t, &m.memoryBackend, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		t.Fatal("an unlisted widening dispatched")
		return DispatchResult{}, nil
	}))
	e.backend = m
	if err := e.evaluate(context.Background(), Event{NamespaceID: "ns", ID: "e1", Kind: "timer", Node: "ready"}, a, "", m.ancestors); err != nil {
		t.Fatal(err)
	}
	last := m.steps[len(m.steps)-1]
	if last.Outcome != OutcomeSensitivityBlocked || !strings.Contains(last.Reason, `add "1:owner" to exposes`) {
		t.Fatalf("outcome %q %q, want blocked telling the author to list 1:owner", last.Outcome, last.Reason)
	}
	if len(m.requests) != 1 || m.requests[0].Variable != "jira-intake:owner" {
		t.Fatalf("requests = %+v, want only the listed entry's", m.requests)
	}
}

// Task t30b (d4): the target audience of a github action is decided by the
// repository its RENDERED input names; the source's by the event's own
// repository variable.
func TestSensitivityGitHubTargetFromRenderedRepository(t *testing.T) {
	a := active("reply")
	a.Declaration.Condition = "true"
	a.Declaration.Trigger.Kind = "github.pr.created"
	a.Declaration.Action = decl.Action{Kind: "github.comment", With: json.RawMessage(`{"uses":"actor://gh","input":{"repository":"{target}","number":"1","comment":"{title}"}}`)}
	m := &sensitivityMemory{repos: map[string]decl.Visibility{"acme/secret": decl.VisibilityPrivate, "acme/secret2": decl.VisibilityPrivate, "acme/open": decl.VisibilityPublic}}
	m.active = []ActiveDeclaration{a}
	calls := 0
	e := newTestEngine(t, &m.memoryBackend, dispatchFunc(func(context.Context, DispatchRequest) (DispatchResult, error) {
		calls++
		return DispatchResult{}, nil
	}))
	e.backend = m
	for i, c := range []struct {
		source, target string
		blocked        bool
	}{
		{"acme/secret", "acme/secret2", false}, // org -> org
		{"acme/secret", "acme/open", true},     // org -> public
		{"acme/secret", "acme/nobody", true},   // org -> unknown (public)
		{"acme/open", "acme/nobody", false},    // public data is public already
	} {
		ev := Event{NamespaceID: "ns", ID: "e" + strconv.Itoa(i), Kind: "github.pr.created", Node: "ready",
			Variables: map[string]any{"repository": c.source, "target": c.target, "title": "t"}}
		if err := e.evaluate(context.Background(), ev, a, "", nil); err != nil {
			t.Fatal(err)
		}
		last := m.steps[len(m.steps)-1]
		if got := last.Outcome == OutcomeSensitivityBlocked; got != c.blocked {
			t.Errorf("%s -> %s: outcome %q %q, want blocked=%v", c.source, c.target, last.Outcome, last.Reason, c.blocked)
		}
	}
	if len(m.requests) != 0 {
		t.Fatalf("unlisted references opened tasks: %+v", m.requests)
	}
}
