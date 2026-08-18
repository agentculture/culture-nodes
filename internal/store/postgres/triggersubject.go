package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// The store half of task t15's one-active-run-per-subject guard (spec
// c31/h16): "at most one active run per originating Jira issue -- a second
// state change or comment while a flow is mid-flight must resume or queue
// against the existing run, never spawn a parallel run on the same
// subject". Split out of engine_store.go (which shares this file's
// engineQueries receiver) purely to keep that file under the repo's 1000-
// line hard limit (tests/lint/filelength_test.go) -- there is nothing here
// that could not have lived there.
//
// See internal/engine/trigger.go's TriggerEvent for the caller: it takes an
// advisory lock keyed on (namespace, workflow key, subject) BEFORE calling
// ActiveRunBySubject, so two concurrent deliveries for the same subject
// cannot both observe "no active run" and both create one.

// selectActiveRunBySubjectSQL is ActiveRunBySubject's query: the oldest
// non-terminal run for (workflow_key, subject), joined through
// workflow_versions the same way engine_store.go's selectRunSQL is (runs
// carries only workflow_version_id). ORDER BY created_at ASC picks the run
// that has been in flight longest when more than one somehow matches —
// defense in depth; the advisory lock TriggerEvent takes before calling this
// is what is actually supposed to make that impossible.
const selectActiveRunBySubjectSQL = `
SELECT r.id, r.namespace_id, r.workflow_version_id, wv.content_digest, r.status,
       r.input, r.output, r.created_at, r.updated_at, r.completed_at, r.actor_affinity, r.subject
FROM runs AS r
JOIN workflow_versions AS wv ON wv.id = r.workflow_version_id
WHERE r.namespace_id = $1 AND wv.workflow_key = $2 AND r.subject = $3
  AND r.status NOT IN ('completed', 'failed', 'cancelled')
ORDER BY r.created_at ASC
LIMIT 1
`

// ActiveRunBySubject is engine.Tx's ActiveRunBySubject. See that method's
// doc comment for the contract: (zero, false, nil) means no such run exists,
// which is the ordinary "first event for this subject" case, not an error.
func (eq engineQueries) ActiveRunBySubject(ctx context.Context, workflowKey, subject string) (engine.Run, bool, error) {
	if workflowKey == "" || subject == "" {
		return engine.Run{}, false, errors.New("postgres: engine: ActiveRunBySubject requires workflowKey and subject")
	}
	var (
		run         engine.Run
		status      string
		input       []byte
		output      []byte
		createdAt   pgtype.Timestamptz
		updatedAt   pgtype.Timestamptz
		completedAt pgtype.Timestamptz
		affinity    []byte
		subjectCol  pgtype.Text
	)
	err := eq.q.QueryRow(ctx, selectActiveRunBySubjectSQL, eq.namespaceID, workflowKey, subject).Scan(
		&run.ID, &run.NamespaceID, &run.WorkflowVersionID, &run.WorkflowDigest, &status,
		&input, &output, &createdAt, &updatedAt, &completedAt, &affinity, &subjectCol,
	)
	if err != nil {
		if isNoRows(err) {
			return engine.Run{}, false, nil
		}
		return engine.Run{}, false, fmt.Errorf("postgres: engine: ActiveRunBySubject: %w", err)
	}
	run.State = engine.RunState(status)
	run.Input = json.RawMessage(input)
	run.Output = json.RawMessage(output)
	run.CreatedAt = tsValue(createdAt)
	run.UpdatedAt = tsValue(updatedAt)
	run.CompletedAt = tsValue(completedAt)
	run.ActorAffinity = jsonOrNil(affinity)
	run.Subject = textOrEmpty(subjectCol)
	return run, true, nil
}
