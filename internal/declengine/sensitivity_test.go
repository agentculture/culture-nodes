package declengine

import (
	"context"
	"encoding/json"
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
	a.Declaration.Action = decl.Action{Kind: "discord.post", With: json.RawMessage(`{"uses":"actor://discord","input":{"text":"{jira-intake:owner} / {1:owner}"}}`)}
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
	// {jira-intake:owner} and {1:owner} are the same variable of the same
	// firing: one request, not two.
	if len(m.requests) != 1 || m.requests[0].Owner != "upstream" || m.requests[0].SourceVersionID != "jira-intake-v1" || m.requests[0].Source.System != decl.SystemJira || m.requests[0].Target.System != decl.SystemDiscord {
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
