package engine_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// These tests need no database: PlanHumanTaskFanOut is a pure function of the
// task, its run's subject, and the deployment's UI origin. That is the point
// of the split — what an operator eventually reads in Jira and Discord is
// decided here, so it is assertable without provisioning anything.

func approvalTask(id, runID string, outcomes ...string) engine.HumanTask {
	request, err := json.Marshal(map[string]any{
		"approver_ref":     "group/platform-maintainers",
		"allowed_outcomes": outcomes,
		"audit":            map[string]any{"node_id": "human-merges-pr"},
	})
	if err != nil {
		panic(err)
	}
	return engine.HumanTask{ID: id, RunID: runID, Kind: "approval", Request: request}
}

func channelsOf(plan []engine.HumanTaskFanOut) []string {
	channels := make([]string, 0, len(plan))
	for _, intent := range plan {
		channels = append(channels, intent.Channel)
	}
	return channels
}

func payloadFor(t *testing.T, plan []engine.HumanTaskFanOut, channel string) map[string]any {
	t.Helper()
	for _, intent := range plan {
		if intent.Channel != channel {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal(intent.Payload, &decoded); err != nil {
			t.Fatalf("decode %s payload: %v", channel, err)
		}
		return decoded
	}
	t.Fatalf("no %s intent in plan %v", channel, channelsOf(plan))
	return nil
}

// A Jira-keyed run owes exactly three messages, and no more: the comment that
// tells a reader what is being asked, the board transition that makes the wait
// visible where the work is tracked, and the Discord post.
func TestJiraKeyedRunFansOutCommentTransitionAndNotify(t *testing.T) {
	task := approvalTask("01TASK", "01RUN", "approved", "expired", "rejected")
	subject := engine.SubjectOfRunInput(json.RawMessage(`{"source":"jira","id":"SCRUM-6","title":"x"}`))

	plan := engine.PlanHumanTaskFanOut(task, subject, "http://thor:18080")

	want := []string{
		engine.FanOutChannelJiraComment,
		engine.FanOutChannelJiraTransition,
		engine.FanOutChannelNotify,
	}
	if got := channelsOf(plan); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("channels = %v, want %v", got, want)
	}

	comment := payloadFor(t, plan, engine.FanOutChannelJiraComment)
	if comment["verb"] != "post_comment" || comment["issue"] != "SCRUM-6" {
		t.Errorf("jira comment payload = %v, want post_comment on SCRUM-6", comment)
	}
	body, _ := comment["comment"].(string)
	for _, must := range []string{"options: approved, rejected", "node: human-merges-pr", "http://thor:18080/tickets/SCRUM-6", "01TASK", "01RUN"} {
		if !strings.Contains(body, must) {
			t.Errorf("jira comment %q does not contain %q", body, must)
		}
	}

	transition := payloadFor(t, plan, engine.FanOutChannelJiraTransition)
	if transition["verb"] != "transition_issue" || transition["issue"] != "SCRUM-6" {
		t.Errorf("transition payload = %v, want transition_issue on SCRUM-6", transition)
	}
	if transition["target"] != engine.JiraDecisionStatus {
		t.Errorf("transition target = %v, want %q", transition["target"], engine.JiraDecisionStatus)
	}

	notify := payloadFor(t, plan, engine.FanOutChannelNotify)
	rendered, _ := json.Marshal(notify)
	for _, must := range []string{"approved, rejected", "http://thor:18080/tickets/SCRUM-6", "SCRUM-6"} {
		if !strings.Contains(string(rendered), must) {
			t.Errorf("notify payload %s does not contain %q", rendered, must)
		}
	}
	// The notify bridge requires at least one of content/title/description
	// (adapters/notify mapping.parse_message), so an intent with none would be
	// refused at the bridge rather than here.
	if title, _ := notify["title"].(string); strings.TrimSpace(title) == "" {
		t.Errorf("notify payload has no title: %s", rendered)
	}
}

// A PR-sourced run gets the notify post and nothing else, because nothing in
// this repo can post a GitHub PR comment. The absence is asserted, not
// assumed: if a GitHub-writing bridge ever lands, this test is what says the
// fan-out was never widened to use it.
func TestGitHubPRSourcedRunFansOutToNotifyOnlyAndSaysWhy(t *testing.T) {
	task := approvalTask("01TASK", "01RUN", "approved", "expired", "rejected")
	subject := engine.SubjectOfRunInput(json.RawMessage(
		`{"source":"github_pr","repository":"agentculture/culture-nodes","number":236,"head_sha":"abc","findings":[]}`))
	if !subject.IsGitHubPR() {
		t.Fatalf("subject %+v is not recognised as a pull request", subject)
	}

	plan := engine.PlanHumanTaskFanOut(task, subject, "http://thor:18080")

	if got := channelsOf(plan); strings.Join(got, ",") != engine.FanOutChannelNotify {
		t.Fatalf("channels = %v, want only %s", got, engine.FanOutChannelNotify)
	}
	notify, _ := json.Marshal(payloadFor(t, plan, engine.FanOutChannelNotify))
	for _, must := range []string{"agentculture/culture-nodes#236", "http://thor:18080/runs/01RUN", "approved, rejected", "human-merges-pr"} {
		if !strings.Contains(string(notify), must) {
			t.Errorf("notify payload %s does not contain %q", notify, must)
		}
	}
	if engine.NoGitHubPRCommentReason == "" {
		t.Error("the documented absence of a GitHub PR comment channel has no stated reason")
	}
}

// The acceptance criterion: nothing derived from the shared decision secret
// may appear in anything this fan-out posts. A Jira comment and a Discord
// message are readable by everyone who can read the board or the channel; a
// token in one would hand that whole audience the authority to decide.
func TestFanOutPayloadsCarryNothingDerivedFromTheDecisionTokenSecret(t *testing.T) {
	const secret = "s3cr3t-decision-token-value-for-this-test"
	t.Setenv("NODES_HUMAN_DECISION_TOKEN_SECRET", secret)
	t.Setenv(engine.UIBaseURLEnv, "http://thor:18080")

	sum := sha256.Sum256([]byte(secret))
	forbidden := []string{
		secret,
		hex.EncodeToString(sum[:]),
		base64.StdEncoding.EncodeToString(sum[:]),
		base64.RawURLEncoding.EncodeToString(sum[:]),
		base64.StdEncoding.EncodeToString([]byte(secret)),
		// The variable's own NAME is forbidden too: a payload that names the
		// knob tells a reader where to go looking for its value.
		"NODES_HUMAN_DECISION_TOKEN_SECRET",
	}

	subjects := []engine.RunSubject{
		engine.SubjectOfRunInput(json.RawMessage(`{"source":"jira","id":"SCRUM-6"}`)),
		engine.SubjectOfRunInput(json.RawMessage(`{"source":"github_pr","repository":"o/r","number":236}`)),
		engine.SubjectOfRunInput(nil),
	}
	for _, subject := range subjects {
		task := approvalTask("01TASK", "01RUN", "approved", "expired", "rejected")
		for _, intent := range engine.PlanHumanTaskFanOut(task, subject, engine.UIBaseURL()) {
			for _, needle := range forbidden {
				if strings.Contains(string(intent.Payload), needle) {
					t.Errorf("%s payload leaks a decision-token-derived value %q: %s",
						intent.Channel, needle, intent.Payload)
				}
			}
		}
	}
}

func TestSubjectOfRunInputReadsBothFactShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		input string
		want  engine.RunSubject
	}{
		"jira work item": {
			`{"source":"jira","id":"SCRUM-6","details_url":"https://x/browse/SCRUM-6"}`,
			engine.RunSubject{Source: "jira", JiraIssue: "SCRUM-6"},
		},
		"merged pr fact": {
			`{"source":"github_pr","repository":"o/r","number":236,"issue_key":"SCRUM-9"}`,
			engine.RunSubject{Source: "github_pr", JiraIssue: "SCRUM-9", Repository: "o/r", PRNumber: 236},
		},
		"pr upkeep finding": {
			`{"source":"github_pr","repository":"o/r","number":236,"head_sha":"a"}`,
			engine.RunSubject{Source: "github_pr", Repository: "o/r", PRNumber: 236},
		},
		"id that is not an issue key": {
			`{"source":"internal","id":"not-a-key"}`,
			engine.RunSubject{Source: "internal"},
		},
		"malformed input is not an error": {`{`, engine.RunSubject{}},
		"absent input is not an error":    {``, engine.RunSubject{}},
	} {
		t.Run(name, func(t *testing.T) {
			got := engine.SubjectOfRunInput(json.RawMessage(tc.input))
			if got != tc.want {
				t.Errorf("SubjectOfRunInput(%s) = %+v, want %+v", tc.input, got, tc.want)
			}
		})
	}
}

// The page link is the difference between a decision a reader can act on and
// one they cannot. An origin with a trailing slash or stray whitespace is what
// a hand-edited prod.env line actually looks like.
func TestUIBaseURLTrimsAndAnUnsetOriginRendersTheBarePath(t *testing.T) {
	subject := engine.SubjectOfRunInput(json.RawMessage(`{"source":"jira","id":"SCRUM-6"}`))

	t.Setenv(engine.UIBaseURLEnv, "  http://thor:18080/  ")
	if got := engine.DecisionPageURL(engine.UIBaseURL(), subject, "01RUN"); got != "http://thor:18080/tickets/SCRUM-6" {
		t.Errorf("page URL = %q, want the trimmed absolute URL", got)
	}

	t.Setenv(engine.UIBaseURLEnv, "")
	if got := engine.DecisionPageURL(engine.UIBaseURL(), subject, "01RUN"); got != "/tickets/SCRUM-6" {
		t.Errorf("page URL with no configured origin = %q, want the bare path", got)
	}
}

// A task whose kind asks for no choice (the schedule_failing alert t9 raises)
// still fans out, and says so rather than offering an empty option list.
func TestTaskWithNoDeclaredOutcomesStillFansOutAndSaysSo(t *testing.T) {
	task := engine.HumanTask{ID: "01TASK", RunID: "01RUN", Kind: "schedule_failing", Request: json.RawMessage(`{}`)}
	plan := engine.PlanHumanTaskFanOut(task, engine.RunSubject{}, "")
	if got := channelsOf(plan); strings.Join(got, ",") != engine.FanOutChannelNotify {
		t.Fatalf("channels = %v, want only %s", got, engine.FanOutChannelNotify)
	}
	rendered, _ := json.Marshal(payloadFor(t, plan, engine.FanOutChannelNotify))
	if !strings.Contains(string(rendered), "no declared outcomes") {
		t.Errorf("notify payload %s does not say the task declares no outcomes", rendered)
	}
}

// Issue #265, both halves, over every payload the fan-out can produce for an
// approval: the node is named by its ID (`human-merges-pr`) and not by its
// kind (`approval`), and `expired` is nowhere in the offered options.
//
// `expired` is the deadline/merged-PR outcome the compiler implies for every
// approval node. It stays in the task's own allowed_outcomes — the expiry
// path validates against that set — but a person cannot select it, and
// docs/drive-from-jira.md promises these options are "exactly the answers the
// engine will accept, not a menu someone wrote by hand". Observed on SCRUM-7:
// `node: approval` and `options: approved, expired, rejected`.
func TestFanOutNamesTheNodeIDAndOffersNoEngineOnlyOutcome(t *testing.T) {
	task := approvalTask("01TASK", "01RUN", "approved", "expired", "rejected")
	for name, subject := range map[string]engine.RunSubject{
		"jira": engine.SubjectOfRunInput(json.RawMessage(`{"source":"jira","id":"SCRUM-7"}`)),
		"pr":   engine.SubjectOfRunInput(json.RawMessage(`{"source":"github_pr","repository":"o/r","number":267}`)),
		"none": {},
	} {
		t.Run(name, func(t *testing.T) {
			for _, intent := range engine.PlanHumanTaskFanOut(task, subject, "http://thor:18080") {
				payload := string(intent.Payload)
				if strings.Contains(payload, engine.OutcomeExpired) {
					t.Errorf("%s payload offers the engine-only outcome %q: %s",
						intent.Channel, engine.OutcomeExpired, payload)
				}
				if intent.Channel == engine.FanOutChannelJiraTransition {
					continue // carries the board target, not the task's identity
				}
				if !strings.Contains(payload, "human-merges-pr") {
					t.Errorf("%s payload does not name the node id: %s", intent.Channel, payload)
				}
				if strings.Contains(payload, `"approval"`) || strings.Contains(payload, "node: approval") {
					t.Errorf("%s payload names the node by kind: %s", intent.Channel, payload)
				}
			}
		})
	}
}

// A task whose only declared outcome is the engine's own says so, rather than
// borrowing the "no declared outcomes" wording — the two are different facts
// and a reader sent to the page deserves to know which one they are meeting.
func TestTaskWhoseOnlyOutcomeIsEngineOnlySaysNobodyMaySelectOne(t *testing.T) {
	task := approvalTask("01TASK", "01RUN", "expired")
	rendered, _ := json.Marshal(payloadFor(t,
		engine.PlanHumanTaskFanOut(task, engine.RunSubject{}, ""), engine.FanOutChannelNotify))
	if !strings.Contains(string(rendered), "no outcome a person may select") {
		t.Errorf("notify payload %s does not say the task offers nothing selectable", rendered)
	}
	if strings.Contains(string(rendered), "no declared outcomes") {
		t.Errorf("notify payload %s conflates an engine-only outcome with none at all", rendered)
	}
}

// The node id comes from the request's audit block. A row written before that
// block existed still has to render something, and its kind is the honest
// fallback.
func TestHumanTaskNodeIDFallsBackToTheKindWithoutAnAudit(t *testing.T) {
	for name, tc := range map[string]struct {
		request string
		want    string
	}{
		"audit names the node": {`{"audit":{"node_id":"needs-human"}}`, "needs-human"},
		"no audit":             {`{"allowed_outcomes":["approved"]}`, "approval"},
		"empty request":        {``, "approval"},
		"malformed request":    {`{`, "approval"},
	} {
		t.Run(name, func(t *testing.T) {
			task := engine.HumanTask{ID: "01TASK", RunID: "01RUN", Kind: "approval",
				Request: json.RawMessage(tc.request)}
			if got := engine.HumanTaskNodeID(task); got != tc.want {
				t.Errorf("HumanTaskNodeID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecidableOutcomesDropsOnlyTheEngineOnlyOutcome(t *testing.T) {
	for name, tc := range map[string]struct {
		in, want []string
	}{
		"implied approval set": {[]string{"approved", "expired", "rejected"}, []string{"approved", "rejected"}},
		"nothing to drop":      {[]string{"approved", "changes_required"}, []string{"approved", "changes_required"}},
		"only the engine's":    {[]string{"expired"}, nil},
		"empty":                {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := engine.DecidableOutcomes(tc.in)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("DecidableOutcomes(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestTicketLifecycleTransitionPlansRequireTheRightEvidence(t *testing.T) {
	opened := engine.PlanPROpenedTransition("SCRUM-15")
	payload := payloadFor(t, []engine.HumanTaskFanOut{opened}, engine.FanOutChannelJiraTransition)
	if payload["target"] != engine.JiraReviewStatus {
		t.Fatalf("PR-opened target = %v, want %q", payload["target"], engine.JiraReviewStatus)
	}
	if got := engine.PlanTicketDoneTransition("SCRUM-15", "not_yet"); got != nil {
		t.Fatalf("not_yet planned transition %+v, want none", got)
	}
	done := engine.PlanTicketDoneTransition("SCRUM-15", "done")
	if done == nil {
		t.Fatal("done planned no transition")
	}
	payload = payloadFor(t, []engine.HumanTaskFanOut{*done}, engine.FanOutChannelJiraTransition)
	if payload["target"] != engine.JiraDoneStatus {
		t.Fatalf("done target = %v, want %q", payload["target"], engine.JiraDoneStatus)
	}
}
