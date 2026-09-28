package declengine

// The Postgres half of per-variable exposure and per-repository audience
// (task t30b, #328, owner decision d4; migrations/0068): the owner's approval
// inbox (declaration_exposure_approvals / _decisions) and the namespace's
// repository visibility record (repository_visibility).

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/jackc/pgx/v5"
)

// SensitivityApproval is one exposure approval task -- (declaration name,
// exposes entry, owner) -- and its current state. The version, source and
// systems are those of the first blocked firing that opened it.
type SensitivityApproval struct {
	ID                   string    `json:"id"`
	DeclarationName      string    `json:"declaration_name"`
	Variable             string    `json:"variable"`
	Owner                string    `json:"owner"`
	DeclarationID        string    `json:"declaration_id"`
	DeclarationVersionID string    `json:"declaration_version"`
	SourceDeclarationID  string    `json:"source_declaration_id"`
	SourceVersionID      string    `json:"source_version"`
	SourceSystem         string    `json:"source_system"`
	SourceAudience       string    `json:"source_audience"`
	TargetSystem         string    `json:"target_system"`
	TargetAudience       string    `json:"target_audience"`
	FirstEventID         string    `json:"first_event_id"`
	Status               string    `json:"status"`
	DecisionID           string    `json:"decision_id,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}

// SensitivityDecision is one append-only answer to an approval task.
type SensitivityDecision struct {
	ID           string    `json:"id"`
	ApprovalID   string    `json:"approval_id"`
	Decision     string    `json:"decision"`
	Decider      string    `json:"decider"`
	Note         string    `json:"note"`
	SupersedesID string    `json:"supersedes,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// approvalSelect derives each task's state. An approval is WITHDRAWN when a
// version of the declaration published after the approving decision no
// longer lists the entry: removing an entry withdraws it, and relisting it
// needs the owner again (a newer approving decision).
const approvalSelect = `SELECT * FROM (SELECT a.namespace_id,a.id,a.declaration_name,a.variable,a.owner,a.declaration_id,a.declaration_version,
 a.source_declaration_id,a.source_version,a.source_system,a.source_audience,a.target_system,a.target_audience,a.first_event_id,a.created_at,
 CASE WHEN d.id IS NULL THEN 'pending'
  WHEN d.decision='approved' AND EXISTS (SELECT 1 FROM declaration_versions v JOIN declarations n ON n.namespace_id=v.namespace_id AND n.id=v.declaration_id
   WHERE n.namespace_id=a.namespace_id AND n.name=a.declaration_name AND v.created_at>d.created_at
   AND NOT (COALESCE(v.body->'exposes','[]'::jsonb) ? a.variable)) THEN 'withdrawn'
  ELSE d.decision END AS status,
 COALESCE(d.id,'') AS decision_id
 FROM declaration_exposure_approvals a LEFT JOIN LATERAL (SELECT id,decision,created_at FROM declaration_exposure_decisions
 WHERE namespace_id=a.namespace_id AND approval_id=a.id ORDER BY created_at DESC,id DESC LIMIT 1) d ON true) x`

func scanApproval(row pgx.Row) (SensitivityApproval, error) {
	var a SensitivityApproval
	var ns string
	err := row.Scan(&ns, &a.ID, &a.DeclarationName, &a.Variable, &a.Owner, &a.DeclarationID, &a.DeclarationVersionID,
		&a.SourceDeclarationID, &a.SourceVersionID, &a.SourceSystem, &a.SourceAudience, &a.TargetSystem, &a.TargetAudience,
		&a.FirstEventID, &a.CreatedAt, &a.Status, &a.DecisionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, postgres.ErrNotFound
	}
	return a, err
}

// RequestSensitivityApproval inserts the task at most once per key.
func (p PostgresBackend) RequestSensitivityApproval(ctx context.Context, in SensitivityApprovalRequest) (SensitivityApproval, error) {
	if _, err := p.Store.Pool().Exec(ctx, `INSERT INTO declaration_exposure_approvals(id,namespace_id,declaration_name,variable,owner,declaration_id,declaration_version,
 source_declaration_id,source_version,source_system,source_audience,target_system,target_audience,first_event_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
 ON CONFLICT (namespace_id,declaration_name,variable,owner) DO NOTHING`,
		store.NewULID(), in.NamespaceID, in.DeclarationName, in.Variable, in.Owner, in.DeclarationID, in.DeclarationVersionID,
		in.SourceDeclarationID, in.SourceVersionID, string(in.Source.System), in.Source.Audience.String(),
		string(in.Target.System), in.Target.Audience.String(), in.EventID); err != nil {
		return SensitivityApproval{}, err
	}
	return FindSensitivityApproval(ctx, p.Store, in.NamespaceID, in.DeclarationName, in.Variable, in.Owner)
}

// FindSensitivityApproval reads the task for one key, or ErrNotFound.
func FindSensitivityApproval(ctx context.Context, db *postgres.Store, ns, declarationName, variable, owner string) (SensitivityApproval, error) {
	return scanApproval(db.Pool().QueryRow(ctx, approvalSelect+` WHERE namespace_id=$1 AND declaration_name=$2 AND variable=$3 AND owner=$4`,
		ns, declarationName, variable, owner))
}

// GetSensitivityApproval reads one task with its current state.
func GetSensitivityApproval(ctx context.Context, db *postgres.Store, ns, id string) (SensitivityApproval, error) {
	return scanApproval(db.Pool().QueryRow(ctx, approvalSelect+` WHERE namespace_id=$1 AND id=$2`, ns, id))
}

// ListSensitivityApprovals is the owner's inbox: every task, oldest first,
// optionally narrowed to one owner and/or one state.
func ListSensitivityApprovals(ctx context.Context, db *postgres.Store, ns, owner, status string) ([]SensitivityApproval, error) {
	rows, err := db.Pool().Query(ctx, approvalSelect+` WHERE namespace_id=$1 AND ($2='' OR owner=$2) AND ($3='' OR status=$3) ORDER BY created_at,id`, ns, owner, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SensitivityApproval{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ErrSensitivityAlreadyDecided refuses a second, blind decision on a task
// whose newest decision still stands (approved or refused). A repeated POST
// -- a double click, a retry, a stale inbox -- must not append a second
// answer (t40b, #328). A deliberate correction names the decision it
// replaces (CorrectSensitivityApproval).
var ErrSensitivityAlreadyDecided = errors.New("declengine: this sensitivity approval task is already decided")

// ErrSensitivityStaleCorrection refuses a correction whose supersedes is not
// the task's current newest decision: whoever sent it was looking at an
// older state.
var ErrSensitivityStaleCorrection = errors.New("declengine: a sensitivity correction must supersede the task's current decision")

// DecideSensitivityApproval appends the owner's answer to a task that has
// none standing: a pending task, or a withdrawn one (a relisted entry needs
// the owner again). A task whose newest decision is approved or refused is
// refused with ErrSensitivityAlreadyDecided and nothing is appended; change
// such an answer with CorrectSensitivityApproval. The decider is always the
// authenticated principal; it must be human and must be the owner.
func DecideSensitivityApproval(ctx context.Context, db *postgres.Store, ns, approvalID string, principal ActivationPrincipal, decision, note string) (SensitivityDecision, error) {
	return appendSensitivityDecision(ctx, db, ns, approvalID, principal, decision, note, "")
}

// CorrectSensitivityApproval appends an answer that replaces the task's
// current newest decision, which supersedes must name exactly (optimistic
// concurrency: a correction made against an older state is refused with
// ErrSensitivityStaleCorrection). The record stays a linear, append-only
// history whose newest entry is the task's state.
func CorrectSensitivityApproval(ctx context.Context, db *postgres.Store, ns, approvalID string, principal ActivationPrincipal, decision, note, supersedes string) (SensitivityDecision, error) {
	if supersedes == "" {
		return SensitivityDecision{}, fmt.Errorf("%w: a correction names the decision it supersedes", ErrSensitivityStaleCorrection)
	}
	return appendSensitivityDecision(ctx, db, ns, approvalID, principal, decision, note, supersedes)
}

func appendSensitivityDecision(ctx context.Context, db *postgres.Store, ns, approvalID string, principal ActivationPrincipal, decision, note, supersedes string) (SensitivityDecision, error) {
	if decision != SensitivityApproved && decision != SensitivityRefused {
		return SensitivityDecision{}, fmt.Errorf("declengine: sensitivity decision must be %q or %q, got %q", SensitivityApproved, SensitivityRefused, decision)
	}
	if principal.Author == "" {
		return SensitivityDecision{}, errors.New("declengine: sensitivity decision principal is required")
	}
	if principal.Kind != PrincipalHuman {
		return SensitivityDecision{}, fmt.Errorf("%w; refused for %s principal %q", ErrSensitivityNotHuman, principal.Kind, principal.Author)
	}
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return SensitivityDecision{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "decl-exposure:"+ns+":"+approvalID); err != nil {
		return SensitivityDecision{}, err
	}
	approval, err := scanApproval(tx.QueryRow(ctx, approvalSelect+` WHERE namespace_id=$1 AND id=$2`, ns, approvalID))
	if err != nil {
		return SensitivityDecision{}, err
	}
	if approval.Owner != ResolveAuthor(principal) {
		return SensitivityDecision{}, fmt.Errorf("%w: approval %s belongs to %q, not %q", ErrSensitivityNotOwner, approvalID, approval.Owner, principal.Author)
	}
	switch {
	case supersedes != "" && supersedes != approval.DecisionID:
		return SensitivityDecision{}, fmt.Errorf("%w: approval %s is %s by decision %q, not %q", ErrSensitivityStaleCorrection, approvalID, approval.Status, approval.DecisionID, supersedes)
	case supersedes == "" && (approval.Status == SensitivityApproved || approval.Status == SensitivityRefused):
		return SensitivityDecision{}, fmt.Errorf("%w: approval %s is %s by decision %s", ErrSensitivityAlreadyDecided, approvalID, approval.Status, approval.DecisionID)
	}
	d := SensitivityDecision{ID: store.NewULID(), ApprovalID: approvalID, Decision: decision, Decider: ResolveAuthor(principal), Note: note, SupersedesID: approval.DecisionID}
	if err := tx.QueryRow(ctx, `INSERT INTO declaration_exposure_decisions(id,namespace_id,approval_id,decision,decider,note,supersedes_id)
 VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,'')) RETURNING created_at`, d.ID, ns, approvalID, decision, d.Decider, note, d.SupersedesID).Scan(&d.CreatedAt); err != nil {
		return SensitivityDecision{}, err
	}
	return d, tx.Commit(ctx)
}

// ListSensitivityDecisions returns an approval's whole decision history,
// oldest first.
func ListSensitivityDecisions(ctx context.Context, db *postgres.Store, ns, approvalID string) ([]SensitivityDecision, error) {
	rows, err := db.Pool().Query(ctx, `SELECT id,approval_id,decision,decider,note,COALESCE(supersedes_id,''),created_at FROM declaration_exposure_decisions
 WHERE namespace_id=$1 AND approval_id=$2 ORDER BY created_at,id`, ns, approvalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SensitivityDecision{}
	for rows.Next() {
		var d SensitivityDecision
		if err := rows.Scan(&d.ID, &d.ApprovalID, &d.Decision, &d.Decider, &d.Note, &d.SupersedesID, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RepositoryVisibilityRecord is one append-only visibility row; the newest
// per repository is the repository's visibility.
type RepositoryVisibilityRecord struct {
	ID         string    `json:"id"`
	Repository string    `json:"repository"`
	Visibility string    `json:"visibility"`
	SetBy      string    `json:"set_by"`
	Note       string    `json:"note"`
	CreatedAt  time.Time `json:"created_at"`
}

// ErrRepositoryVisibilityInvalid refuses a malformed repository or value.
var ErrRepositoryVisibilityInvalid = errors.New("declengine: repository visibility must name an owner/name repository and be public or private")

var repositoryPattern = regexp.MustCompile(`^[a-z0-9_.-]+/[a-z0-9_.-]+$`)

// NormalizeRepository lower-cases an owner/name pair (GitHub names are
// case-insensitive), or reports it malformed.
func NormalizeRepository(repository string) (string, bool) {
	r := strings.ToLower(strings.TrimSpace(repository))
	return r, repositoryPattern.MatchString(r)
}

// RepositoryVisibility reads a repository's newest record.
func (p PostgresBackend) RepositoryVisibility(ctx context.Context, ns, repository string) (decl.Visibility, error) {
	r, ok := NormalizeRepository(repository)
	if !ok {
		return decl.VisibilityUnknown, nil
	}
	var v string
	err := p.Store.Pool().QueryRow(ctx, `SELECT visibility FROM repository_visibility WHERE namespace_id=$1 AND repository=$2
 ORDER BY created_at DESC,id DESC LIMIT 1`, ns, r).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return decl.VisibilityUnknown, nil
	}
	return decl.Visibility(v), err
}

// SetRepositoryVisibility appends a record; setBy is the authenticated
// principal that set it.
func SetRepositoryVisibility(ctx context.Context, db *postgres.Store, ns, repository, visibility, setBy, note string) (RepositoryVisibilityRecord, error) {
	r, ok := NormalizeRepository(repository)
	if !ok || (visibility != string(decl.VisibilityPublic) && visibility != string(decl.VisibilityPrivate)) {
		return RepositoryVisibilityRecord{}, fmt.Errorf("%w: got repository %q, visibility %q", ErrRepositoryVisibilityInvalid, repository, visibility)
	}
	if setBy == "" {
		return RepositoryVisibilityRecord{}, errors.New("declengine: repository visibility needs an authenticated principal")
	}
	rec := RepositoryVisibilityRecord{ID: store.NewULID(), Repository: r, Visibility: visibility, SetBy: setBy, Note: note}
	err := db.Pool().QueryRow(ctx, `INSERT INTO repository_visibility(id,namespace_id,repository,visibility,set_by,note) VALUES($1,$2,$3,$4,$5,$6) RETURNING created_at`,
		rec.ID, ns, r, visibility, setBy, note).Scan(&rec.CreatedAt)
	return rec, err
}

// ListRepositoryVisibility returns each recorded repository's current row.
func ListRepositoryVisibility(ctx context.Context, db *postgres.Store, ns string) ([]RepositoryVisibilityRecord, error) {
	rows, err := db.Pool().Query(ctx, `SELECT DISTINCT ON (repository) id,repository,visibility,set_by,note,created_at FROM repository_visibility
 WHERE namespace_id=$1 ORDER BY repository,created_at DESC,id DESC`, ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RepositoryVisibilityRecord{}
	for rows.Next() {
		var r RepositoryVisibilityRecord
		if err := rows.Scan(&r.ID, &r.Repository, &r.Visibility, &r.SetBy, &r.Note, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DestinationAudienceRecord is the current or historical mark for one actor.
type DestinationAudienceRecord struct {
	ID        string    `json:"id"`
	Actor     string    `json:"actor"`
	Audience  string    `json:"audience"`
	SetBy     string    `json:"set_by"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

var ErrDestinationAudienceInvalid = errors.New("declengine: destination audience must name an actor key and be public, org, team or operators")

// DestinationAudience reads the newest actor mark, if recorded.
func (p PostgresBackend) DestinationAudience(ctx context.Context, ns, actor string) (decl.Audience, bool, error) {
	if !repositoryPattern.MatchString(actor) {
		return 0, false, nil
	}
	var value string
	err := p.Store.Pool().QueryRow(ctx, `SELECT audience FROM destination_audiences WHERE namespace_id=$1 AND actor=$2
 ORDER BY created_at DESC,id DESC LIMIT 1`, ns, actor).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	switch value {
	case "public":
		return decl.AudiencePublic, true, nil
	case "org":
		return decl.AudienceOrg, true, nil
	case "team":
		return decl.AudienceTeam, true, nil
	case "operators":
		return decl.AudienceOperators, true, nil
	}
	return 0, false, fmt.Errorf("invalid stored destination audience %q", value)
}

// SetDestinationAudience appends a human-set actor mark.
func SetDestinationAudience(ctx context.Context, db *postgres.Store, ns, actor, audience, setBy, note string) (DestinationAudienceRecord, error) {
	if !repositoryPattern.MatchString(actor) || (audience != "public" && audience != "org" && audience != "team" && audience != "operators") {
		return DestinationAudienceRecord{}, fmt.Errorf("%w: actor %q, audience %q", ErrDestinationAudienceInvalid, actor, audience)
	}
	if setBy == "" {
		return DestinationAudienceRecord{}, errors.New("declengine: destination audience needs an authenticated principal")
	}
	rec := DestinationAudienceRecord{ID: store.NewULID(), Actor: actor, Audience: audience, SetBy: setBy, Note: note}
	err := db.Pool().QueryRow(ctx, `INSERT INTO destination_audiences(id,namespace_id,actor,audience,set_by,note) VALUES($1,$2,$3,$4,$5,$6) RETURNING created_at`,
		rec.ID, ns, actor, audience, setBy, note).Scan(&rec.CreatedAt)
	return rec, err
}

// ListDestinationAudiences returns the newest row for every recorded actor.
func ListDestinationAudiences(ctx context.Context, db *postgres.Store, ns string) ([]DestinationAudienceRecord, error) {
	rows, err := db.Pool().Query(ctx, `SELECT DISTINCT ON (actor) id,actor,audience,set_by,note,created_at FROM destination_audiences
 WHERE namespace_id=$1 ORDER BY actor,created_at DESC,id DESC`, ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DestinationAudienceRecord{}
	for rows.Next() {
		var rec DestinationAudienceRecord
		if err := rows.Scan(&rec.ID, &rec.Actor, &rec.Audience, &rec.SetBy, &rec.Note, &rec.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
