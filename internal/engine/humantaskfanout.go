package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Fan-out: making a pending human task reach the person who can answer it
// (task t11, spec c6, plan decisions q1/q4/q5).
//
// A human task used to be discoverable on exactly two surfaces a person has
// to go and look at, /inbox and /decisions, and nobody is paged to either.
// This file is the pure half of the fix: given a task, the subject its run
// names, and the deployment's UI origin, it decides WHICH messages that task
// owes and what each one says. It performs no I/O and knows nothing about a
// database — internal/store/postgres/humantaskfanout.go queues what this
// returns, and internal/humanfanout delivers it.
//
// Three rules hold this file's shape:
//
//   - One fan-out per task, not per surface-refresh. The planner is a pure
//     function of the task, so calling it twice yields the same list; the
//     UNIQUE (human_task_id, channel) index in migration 0051 is what turns
//     that into "the same task twice emits nothing more".
//   - Only what the decider needs to act: the options the engine will
//     actually accept, and a link to the page that accepts them. Every
//     rendered string here is assembled from the task's own request, its run,
//     and the configured origin — never from process configuration.
//   - NOTHING derived from NODES_HUMAN_DECISION_TOKEN_SECRET, ever. A Jira
//     comment and a Discord post are readable by anyone who can read the
//     board or the channel; a decision token pasted into one would be an
//     authority handed to that whole audience. The decider brings their own
//     token to the page. humantaskfanout_test.go asserts this over every
//     payload this file can produce.

// UIBaseURLEnv names the origin the decision page is served from — the value
// that decides whether the link posted to Jira and Discord is clickable at
// all (task t16 wires it into both compose files and both prod.env).
//
// An empty or absent value renders the bare path deliberately: a default
// invented in Go would be a second, unmanaged opinion about where this
// deployment is served from, sitting where no deploy could correct it.
const UIBaseURLEnv = "NODES_UI_BASE_URL"

// The fan-out channels migration 0051's CHECK constraint admits. There is no
// github_pr_comment channel: no actor registered in this deployment advertises
// a verb that writes to a GitHub pull-request thread, so a PR-sourced run fans
// out to notify alone rather than queueing a row nothing could drain. The
// audit of which bridges were checked, and what each one does instead, is in
// migration 0051's header and internal/humanfanout's package doc -- naming
// them here would put provider names in the engine (PRD §9.5).
const (
	FanOutChannelJiraComment    = "jira_comment"
	FanOutChannelJiraTransition = "jira_transition"
	FanOutChannelNotify         = "notify"
)

// NoGitHubPRCommentReason is the documented absence spec c6 asks this task to
// state rather than silently omit. It is a constant so the test that pins the
// absence names the same reason the code does.
const NoGitHubPRCommentReason = "no actor registered in this deployment advertises a verb that writes to a " +
	"GitHub pull-request thread: the one bridge that reads a PR thread writes only to its own submit " +
	"surface, and the agent bridges expose no GitHub write capability at all. A PR-sourced run therefore " +
	"fans out to notify only (task t11); see internal/humanfanout's package doc for the per-bridge audit"

// JiraDecisionStatus is the board status a ticket moves to while culture-nodes
// waits on a decision (plan decision q4). It is the SCRUM board's existing
// 'Pending' rather than a new status, because adding a status is a board
// change a human owns and this cycle does not make one.
//
// It is not enforced here. The narrow Jira bridge's own allowlist
// (adapters/jira, JIRA_TRANSITION_TARGET) is the enforcement point, and it
// must name this target for the transition to land at all — which is why the
// bridge's allowlist became a list in this task.
const JiraDecisionStatus = "Pending"

// HumanTaskFanOut is one queued message a human task owes: which channel it
// goes out on, and the bridge input that carries it.
type HumanTaskFanOut struct {
	Channel string
	Payload json.RawMessage
}

// RunSubject is what a run's input says its work is ABOUT, as far as the
// fan-out is concerned. It is read from the run's own input document rather
// than from a workflow-specific binding, because the two producers that
// matter both put it there: the sweep's jira_work_items facts
// (source=jira, id=SCRUM-N) and its pr-upkeep.pr facts (source=github_pr,
// repository, number).
//
// A run whose input matches neither shape yields a zero RunSubject, which is
// not an error: it fans out to notify alone.
type RunSubject struct {
	Source     string
	JiraIssue  string
	Repository string
	PRNumber   int
}

// jiraIssueKeyPattern-ish validation, kept deliberately loose: the narrow
// Jira bridge re-validates the key against its own ^[A-Z][A-Z0-9_]*-[1-9][0-9]*$
// regex and its configured project prefix before it will post anything, so
// this only has to avoid queueing obvious nonsense.
func looksLikeJiraIssueKey(s string) bool {
	dash := strings.IndexByte(s, '-')
	if dash <= 0 || dash == len(s)-1 {
		return false
	}
	for _, r := range s[:dash] {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	for _, r := range s[dash+1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// SubjectOfRunInput reads the run's input document for the two subject shapes
// the fan-out can address. Malformed or absent input is a zero RunSubject,
// never an error: a run that cannot say what it is about still deserves its
// notify post.
func SubjectOfRunInput(input json.RawMessage) RunSubject {
	var doc struct {
		Source     string          `json:"source"`
		ID         string          `json:"id"`
		IssueKey   string          `json:"issue_key"`
		Repository string          `json:"repository"`
		Number     json.RawMessage `json:"number"`
	}
	if len(input) == 0 {
		return RunSubject{}
	}
	if err := json.Unmarshal(input, &doc); err != nil {
		return RunSubject{}
	}
	subject := RunSubject{Source: doc.Source, Repository: doc.Repository}
	// Both spellings are accepted because both exist in facts this control
	// plane already stores: jira_work_items writes the key as `id`, and
	// merged_pr_fact writes it as `issue_key`.
	for _, candidate := range []string{doc.IssueKey, doc.ID} {
		if looksLikeJiraIssueKey(candidate) {
			subject.JiraIssue = candidate
			break
		}
	}
	if n, err := strconv.Atoi(strings.Trim(string(doc.Number), `"`)); err == nil && n > 0 {
		subject.PRNumber = n
	}
	return subject
}

// IsGitHubPR reports whether this subject is a pull request, which is what
// the pr.merged expiry consumer matches on.
func (s RunSubject) IsGitHubPR() bool {
	return s.Source == "github_pr" && s.Repository != "" && s.PRNumber > 0
}

// UIBaseURL is the deployment's UI origin, trimmed. Trimming is not cosmetic:
// this value is read from ~/.culture-nodes/prod.env, where a trailing slash is
// what an operator types and surrounding whitespace is what a hand-edited line
// leaves behind — either one concatenated naively gives a URL that is wrong in
// a way nothing downstream reports.
func UIBaseURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv(UIBaseURLEnv)), "/")
}

// DecisionPageURL is where the decider goes to answer. A Jira-keyed run gets
// its ticket page, which task t18 renders the task's own option buttons on; a
// run with no ticket gets its run page, which is the only page that names it.
func DecisionPageURL(base string, subject RunSubject, runID string) string {
	if subject.JiraIssue != "" {
		return base + "/tickets/" + subject.JiraIssue
	}
	return base + "/runs/" + runID
}

// HumanTaskOptions is the decision's allowed outcomes, read back from the
// task's own request — what the decider was actually shown, and exactly the
// set DecideHumanTask's checkOutcome will accept. A task whose request
// declares none (a kind that is an alert rather than a choice, e.g.
// schedule_failing) returns nil, and the rendered message says so rather than
// offering an empty list.
func HumanTaskOptions(task HumanTask) []string {
	var request humanTaskRequest
	if len(task.Request) == 0 {
		return nil
	}
	if err := json.Unmarshal(task.Request, &request); err != nil {
		return nil
	}
	return request.AllowedOutcomes
}

// HumanTaskNodeID is the workflow node this task is FOR — `needs-human`, not
// `approval` (issue #265). The kind says what sort of thing the node is; the
// id says which node, and a message that names only the kind cannot be traced
// back to a line in the workflow by the person reading it. It is already on
// the task, in the request's audit block (humantask.go), which is why this
// needs no extra column or join.
//
// A request that carries no audit falls back to the kind: an older row is
// better described imprecisely than not at all.
func HumanTaskNodeID(task HumanTask) string {
	var request humanTaskRequest
	if len(task.Request) > 0 {
		if err := json.Unmarshal(task.Request, &request); err == nil && request.Audit.NodeID != "" {
			return request.Audit.NodeID
		}
	}
	return task.Kind
}

// renderOptions states what the reader may choose. It takes both sets because
// the two empty cases mean different things and a message that conflates them
// would send someone to a page to look for buttons that are not there:
// `declared` empty is a task that is an alert rather than a choice
// (schedule_failing), while `decidable` empty with `declared` non-empty is a
// task whose only declared outcome is one the engine reaches by itself.
func renderOptions(declared, decidable []string) string {
	switch {
	case len(declared) == 0:
		return "(no declared outcomes; open the page to see what this task asks for)"
	case len(decidable) == 0:
		return "(no outcome a person may select; this task resolves when the control plane expires it)"
	}
	return strings.Join(decidable, ", ")
}

// PlanHumanTaskFanOut is the whole decision: which messages this task owes.
//
// Order is stable (Jira comment, Jira transition, notify) so a test reading
// the queued rows reads them in a fixed order, and so a drain that publishes
// in id order posts the comment before moving the board — a reader who sees
// the status change has already had the comment explaining it.
func PlanHumanTaskFanOut(task HumanTask, subject RunSubject, base string) []HumanTaskFanOut {
	declared := HumanTaskOptions(task)
	// What this message offers is what a person may actually choose, which is
	// not the whole declared set: `expired` is the engine's own outcome
	// (DecidableOutcomes). docs/drive-from-jira.md promises these options are
	// "exactly the answers the engine will accept — not a menu someone wrote
	// by hand", and an `expired` a decider cannot pick broke that promise
	// (issue #265).
	options := DecidableOutcomes(declared)
	nodeID := HumanTaskNodeID(task)
	page := DecisionPageURL(base, subject, task.RunID)
	plan := make([]HumanTaskFanOut, 0, 3)

	if subject.JiraIssue != "" {
		comment := fmt.Sprintf(
			"culture-nodes is waiting on a decision.\n\n"+
				"task: %s\nrun: %s\nnode: %s\noptions: %s\ndecide: %s",
			task.ID, task.RunID, nodeID, renderOptions(declared, options), page)
		plan = append(plan,
			fanOut(FanOutChannelJiraComment, map[string]any{
				"verb": "post_comment", "issue": subject.JiraIssue, "comment": comment,
			}),
			fanOut(FanOutChannelJiraTransition, map[string]any{
				"verb": "transition_issue", "issue": subject.JiraIssue, "target": JiraDecisionStatus,
			}),
		)
	}

	// Always. Discord is the one channel that does not depend on the run
	// naming a subject, which is why it is the fan-out's floor rather than
	// one more conditional branch.
	fields := []map[string]any{
		{"name": "Options", "value": renderOptions(declared, options)},
		{"name": "Decide", "value": page},
		{"name": "Run", "value": task.RunID},
		{"name": "Node", "value": nodeID},
	}
	if subject.JiraIssue != "" {
		fields = append(fields, map[string]any{"name": "Ticket", "value": subject.JiraIssue})
	}
	if subject.IsGitHubPR() {
		fields = append(fields, map[string]any{
			"name": "Pull request", "value": fmt.Sprintf("%s#%d", subject.Repository, subject.PRNumber),
		})
	}
	plan = append(plan, fanOut(FanOutChannelNotify, map[string]any{
		"title": "Decision needed: " + task.Kind,
		"description": fmt.Sprintf("Human task %s on run %s is waiting for an answer.",
			task.ID, task.RunID),
		"fields": fields,
	}))
	return plan
}

// fanOut marshals one intent. json.Marshal of a map[string]any built from
// strings and ints cannot fail, so the error is discarded here rather than
// propagated through a pure planner as an impossible branch.
func fanOut(channel string, payload map[string]any) HumanTaskFanOut {
	raw, _ := json.Marshal(payload)
	return HumanTaskFanOut{Channel: channel, Payload: raw}
}
