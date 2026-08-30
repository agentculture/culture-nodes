// Task t16 (SCRUM-6, spec c10 / finding s10): the page-link comment's
// rendering, unit-tested without a database.
//
// This file is `package postgres` rather than `package postgres_test` on
// purpose. The thing under test is one pure string function, and the
// external test package's TestMain provisions a PostgreSQL instance before
// anything runs -- a cost this assertion has no need of, and a dependency
// that would make "does the comment carry an absolute URL" unanswerable on
// a machine with neither NODES_TEST_DATABASE_URL nor Docker. The rendering
// is the half of the defect an operator sees in Jira; the enqueue path
// around it is already covered by the database-backed
// internal/engine/trigger_test.go.
package postgres

import (
	"os"
	"strings"
	"testing"
)

// TestPageCommentIsAbsoluteWhenTheBaseURLIsSet is the acceptance criterion:
// with NODES_UI_BASE_URL set, the comment culture-nodes posts to a Jira
// ticket is a link a reader can click.
func TestPageCommentIsAbsoluteWhenTheBaseURLIsSet(t *testing.T) {
	t.Setenv(UIBaseURLEnv, "http://thor:18080")

	got := jiraTicketPageComment("SCRUM-6")

	const want = "culture-nodes page: http://thor:18080/tickets/SCRUM-6 [culture-nodes:ticket-page-link]"
	if got != want {
		t.Fatalf("page-link comment = %q, want %q", got, want)
	}
	if !strings.Contains(got, "://") {
		t.Errorf("page-link comment %q carries no scheme, so Jira renders text rather than a link", got)
	}
}

// TestPageCommentTrimsWhatAnEnvFileCanCarry pins the two shapes a value read
// from ~/.culture-nodes/prod.env really arrives in. A trailing slash is what
// an operator types; surrounding whitespace is what a hand-edited env line
// leaves behind. Either one, concatenated naively, produces a URL that is
// wrong in a way nothing downstream reports.
func TestPageCommentTrimsWhatAnEnvFileCanCarry(t *testing.T) {
	for _, base := range []string{"https://nodes.example/", "  https://nodes.example  ", "https://nodes.example//"} {
		t.Setenv(UIBaseURLEnv, base)
		got := jiraTicketPageComment("SCRUM-6")
		const want = "culture-nodes page: https://nodes.example/tickets/SCRUM-6 [culture-nodes:ticket-page-link]"
		if got != want {
			t.Errorf("NODES_UI_BASE_URL=%q rendered %q, want %q", base, got, want)
		}
	}
}

// TestPageCommentFallsBackToTheBarePathWhenTheBaseURLIsEmpty is the
// before-state the task exists to fix, kept as a test rather than deleted:
// the bare path is what SCRUM-5 got, and it must remain the ONLY thing an
// empty (or absent) variable can produce. A default invented here would put
// a second, unmanaged opinion about the deployment's origin in the code
// path, where no deploy could correct it.
func TestPageCommentFallsBackToTheBarePathWhenTheBaseURLIsEmpty(t *testing.T) {
	const want = "culture-nodes page: /tickets/SCRUM-6 [culture-nodes:ticket-page-link]"

	t.Setenv(UIBaseURLEnv, "")
	if got := jiraTicketPageComment("SCRUM-6"); got != want {
		t.Errorf("with an empty %s the comment = %q, want the bare path %q", UIBaseURLEnv, got, want)
	}

	// Present-but-empty and absent must agree: prod.env can produce either.
	t.Setenv(UIBaseURLEnv, "unset-below")
	if err := os.Unsetenv(UIBaseURLEnv); err != nil {
		t.Fatalf("unset %s: %v", UIBaseURLEnv, err)
	}
	if got := jiraTicketPageComment("SCRUM-6"); got != want {
		t.Errorf("with %s absent the comment = %q, want the bare path %q", UIBaseURLEnv, got, want)
	}
}
