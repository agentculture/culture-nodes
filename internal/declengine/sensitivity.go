// Variable sensitivity at firing time (task t30, #328; spec owner decision
// q22): a firing whose action would render a variable into a system with a
// wider audience than the variable came from (internal/decl/sensitivity.go's
// one ranking table) is recorded as OutcomeSensitivityBlocked and is not
// dispatched until the variable's owner approves the widening.
//
// Decisions this file makes, all failing closed:
//
//   - The variable's OWNER is the author of the declaration version that
//     produced it: for a step-0 reference, the firing declaration's own
//     version (its trigger event supplied the value); for a lineage
//     reference, the version the ancestor firing pinned. declaration_versions
//     .author is always the authenticated publishing principal (c89/h62), so
//     it is the one owner identity the codebase can resolve for every
//     variable without inventing a new ownership model.
//   - The block happens BEFORE the firing is claimed, beside the other
//     pre-claim refusals (loop-limited, deferred): nothing is dispatched, no
//     marker minted, no landing node opened, and a later event can still fire
//     once the approval exists. Only a reference whose value is actually
//     present is checked -- a missing value renders its default or the
//     placeholder, which exposes nothing.
//   - One approval covers exactly (declaration version, source version,
//     variable, target system): a new version of either declaration is a new
//     question and needs a new approval. Repeated blocked events reuse the
//     one open task (the table's unique key), never one task per event.
//   - Approval and refusal are append-only decisions; the newest one is the
//     task's state, and a refusal keeps blocking exactly like no answer.
//   - Only a HUMAN principal who IS the owner may decide (the same
//     ActivationPrincipal classification activation uses). An agent is
//     refused; so is a different human. A widening of a variable whose owner
//     is an agent therefore stays blocked until a human republishes the
//     producing declaration -- no agent approves exposing data.
package declengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/jackc/pgx/v5"
)

// OutcomeSensitivityBlocked: the firing would widen a variable's audience
// and the owner has not approved (or has refused) that widening.
const OutcomeSensitivityBlocked = "sensitivity-blocked"

// Sensitivity approval states: the newest decision, or pending when none.
const (
	SensitivityPending  = "pending"
	SensitivityApproved = "approved"
	SensitivityRefused  = "refused"
)

var (
	// ErrSensitivityNotHuman refuses a decision from an agent principal.
	ErrSensitivityNotHuman = errors.New("declengine: only a human principal may approve or refuse a sensitivity widening")
	// ErrSensitivityNotOwner refuses a decision from anyone but the owner.
	ErrSensitivityNotOwner = errors.New("declengine: only the variable's owner may approve or refuse its widening")
)

// SensitivitySource is the declaration version that produced a variable,
// and the mark its variables carry.
type SensitivitySource struct {
	DeclarationID, VersionID, Author string
	Declaration                      decl.Declaration
}

// SensitivityApprovalRequest names one widening that needs its owner.
type SensitivityApprovalRequest struct {
	NamespaceID, DeclarationID, DeclarationVersionID string
	SourceDeclarationID, SourceVersionID, Variable   string
	Owner, EventID                                   string
	Source, Target                                   decl.Sensitivity
}

// SensitivityApproval is the human inbox task and its current state.
type SensitivityApproval struct {
	ID                   string    `json:"id"`
	DeclarationID        string    `json:"declaration_id"`
	DeclarationVersionID string    `json:"declaration_version"`
	SourceDeclarationID  string    `json:"source_declaration_id"`
	SourceVersionID      string    `json:"source_version"`
	Variable             string    `json:"variable"`
	SourceSystem         string    `json:"source_system"`
	SourceAudience       string    `json:"source_audience"`
	TargetSystem         string    `json:"target_system"`
	TargetAudience       string    `json:"target_audience"`
	Owner                string    `json:"owner"`
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

// SensitivityBackend is the firing loop's view of sources and approvals. A
// Backend that does not implement it performs no sensitivity check; the
// production PostgresBackend does (asserted below), so every real engine
// enforces it.
type SensitivityBackend interface {
	VersionSource(ctx context.Context, namespaceID, versionID string) (SensitivitySource, error)
	FiringSource(ctx context.Context, namespaceID, firingID string) (SensitivitySource, error)
	// RequestSensitivityApproval opens the task for this widening, or returns
	// the one already open for the same key, with its current state.
	RequestSensitivityApproval(ctx context.Context, in SensitivityApprovalRequest) (SensitivityApproval, error)
}

var _ SensitivityBackend = PostgresBackend{}

// stepIndex resolves a template step exactly as rendering does: -1 is the
// current firing, otherwise an index into lineage. ok is false when the step
// names nothing in this lineage.
func stepIndex(step string, lineage []Ancestor) (int, bool) {
	if step == "0" {
		return -1, true
	}
	if n, err := strconv.Atoi(step); err == nil {
		return n - 1, n > 0 && n <= len(lineage)
	}
	for i, a := range lineage {
		if a.Name == step {
			return i, true
		}
	}
	return 0, false
}

// checkSensitivity returns a non-empty reason when a present variable the
// action renders would widen its audience without an approval.
func (e *Engine) checkSensitivity(ctx context.Context, event Event, a ActiveDeclaration, lineage []Ancestor) (string, error) {
	sb, ok := e.backend.(SensitivityBackend)
	if !ok {
		return "", nil
	}
	refs, err := decl.ActionReferences(a.Declaration.Action)
	if err != nil || len(refs) == 0 {
		return "", err
	}
	target := decl.TargetSensitivity(a.Declaration.Action.Kind)
	var blocked []string
	seen := map[string]bool{}
	for _, ref := range refs {
		idx, found := stepIndex(ref.Step, lineage)
		if !found {
			continue
		}
		vars, key := event.Variables, "0"
		if idx >= 0 {
			vars, key = lineage[idx].Variables, lineage[idx].FiringID
		}
		if v, present := vars[ref.Name]; !present || v == nil || seen[key+"\x00"+ref.Name] {
			continue
		}
		seen[key+"\x00"+ref.Name] = true
		var src SensitivitySource
		var mark decl.Sensitivity
		if idx < 0 {
			src, err = sb.VersionSource(ctx, event.NamespaceID, a.VersionID)
			mark = decl.TriggerSensitivity(a.Declaration)
		} else {
			src, err = sb.FiringSource(ctx, event.NamespaceID, lineage[idx].FiringID)
			mark = decl.FiringSensitivity(src.Declaration)
		}
		if err != nil {
			return "", err
		}
		if !decl.Widens(mark, target) {
			continue
		}
		approval, err := sb.RequestSensitivityApproval(ctx, SensitivityApprovalRequest{
			NamespaceID: event.NamespaceID, DeclarationID: a.ID, DeclarationVersionID: a.VersionID,
			SourceDeclarationID: src.DeclarationID, SourceVersionID: src.VersionID, Variable: ref.Name,
			Owner: src.Author, EventID: event.ID, Source: mark, Target: target,
		})
		if err != nil {
			return "", err
		}
		if approval.Status == SensitivityApproved {
			continue
		}
		blocked = append(blocked, fmt.Sprintf("variable %q (step %s) from %s would render into the wider %s; owner %q must approve (approval %s is %s)",
			ref.Name, ref.Step, mark, target, approval.Owner, approval.ID, approval.Status))
	}
	return strings.Join(blocked, "; "), nil
}

func parseSource(declarationID, versionID, author string, body []byte) (SensitivitySource, error) {
	var d decl.Declaration
	if err := json.Unmarshal(body, &d); err != nil {
		return SensitivitySource{}, fmt.Errorf("declengine: sensitivity source version %s: %w", versionID, err)
	}
	return SensitivitySource{DeclarationID: declarationID, VersionID: versionID, Author: author, Declaration: d}, nil
}

// VersionSource reads the declaration version itself.
func (p PostgresBackend) VersionSource(ctx context.Context, ns, versionID string) (SensitivitySource, error) {
	var declarationID, author string
	var body []byte
	if err := p.Store.Pool().QueryRow(ctx, `SELECT declaration_id,author,body FROM declaration_versions WHERE namespace_id=$1 AND id=$2`, ns, versionID).Scan(&declarationID, &author, &body); err != nil {
		return SensitivitySource{}, err
	}
	return parseSource(declarationID, versionID, author, body)
}

// FiringSource reads the version a firing pinned.
func (p PostgresBackend) FiringSource(ctx context.Context, ns, firingID string) (SensitivitySource, error) {
	var declarationID, versionID, author string
	var body []byte
	if err := p.Store.Pool().QueryRow(ctx, `SELECT v.declaration_id,v.id,v.author,v.body FROM declaration_firings f
 JOIN declaration_versions v ON v.namespace_id=f.namespace_id AND v.id=f.declaration_version WHERE f.namespace_id=$1 AND f.id=$2`, ns, firingID).Scan(&declarationID, &versionID, &author, &body); err != nil {
		return SensitivitySource{}, err
	}
	return parseSource(declarationID, versionID, author, body)
}

const approvalColumns = `a.id,a.declaration_id,a.declaration_version,a.source_declaration_id,a.source_version,a.variable,
 a.source_system,a.source_audience,a.target_system,a.target_audience,a.owner,a.first_event_id,a.created_at,
 COALESCE(d.decision,'pending'),COALESCE(d.id,'')`

const approvalFrom = ` FROM declaration_sensitivity_approvals a LEFT JOIN LATERAL (SELECT id,decision FROM declaration_sensitivity_decisions
 WHERE namespace_id=a.namespace_id AND approval_id=a.id ORDER BY created_at DESC,id DESC LIMIT 1) d ON true`

func scanApproval(row pgx.Row) (SensitivityApproval, error) {
	var a SensitivityApproval
	err := row.Scan(&a.ID, &a.DeclarationID, &a.DeclarationVersionID, &a.SourceDeclarationID, &a.SourceVersionID, &a.Variable,
		&a.SourceSystem, &a.SourceAudience, &a.TargetSystem, &a.TargetAudience, &a.Owner, &a.FirstEventID, &a.CreatedAt, &a.Status, &a.DecisionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, postgres.ErrNotFound
	}
	return a, err
}

// RequestSensitivityApproval inserts the task at most once per key.
func (p PostgresBackend) RequestSensitivityApproval(ctx context.Context, in SensitivityApprovalRequest) (SensitivityApproval, error) {
	if _, err := p.Store.Pool().Exec(ctx, `INSERT INTO declaration_sensitivity_approvals(id,namespace_id,declaration_id,declaration_version,source_declaration_id,source_version,variable,source_system,source_audience,target_system,target_audience,owner,first_event_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
 ON CONFLICT (namespace_id,declaration_version,source_version,variable,target_system) DO NOTHING`,
		store.NewULID(), in.NamespaceID, in.DeclarationID, in.DeclarationVersionID, in.SourceDeclarationID, in.SourceVersionID, in.Variable,
		string(in.Source.System), in.Source.Audience.String(), string(in.Target.System), in.Target.Audience.String(), in.Owner, in.EventID); err != nil {
		return SensitivityApproval{}, err
	}
	return scanApproval(p.Store.Pool().QueryRow(ctx, `SELECT `+approvalColumns+approvalFrom+`
 WHERE a.namespace_id=$1 AND a.declaration_version=$2 AND a.source_version=$3 AND a.variable=$4 AND a.target_system=$5`,
		in.NamespaceID, in.DeclarationVersionID, in.SourceVersionID, in.Variable, string(in.Target.System)))
}

// GetSensitivityApproval reads one task with its current state.
func GetSensitivityApproval(ctx context.Context, db *postgres.Store, ns, id string) (SensitivityApproval, error) {
	return scanApproval(db.Pool().QueryRow(ctx, `SELECT `+approvalColumns+approvalFrom+` WHERE a.namespace_id=$1 AND a.id=$2`, ns, id))
}

// ListSensitivityApprovals is the owner's inbox: every task, oldest first,
// optionally narrowed to one owner and/or one state.
func ListSensitivityApprovals(ctx context.Context, db *postgres.Store, ns, owner, status string) ([]SensitivityApproval, error) {
	rows, err := db.Pool().Query(ctx, `SELECT `+approvalColumns+approvalFrom+`
 WHERE a.namespace_id=$1 AND ($2='' OR a.owner=$2) AND ($3='' OR COALESCE(d.decision,'pending')=$3) ORDER BY a.created_at,a.id`, ns, owner, status)
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

// DecideSensitivityApproval appends the owner's answer. The decider is always
// the authenticated principal; it must be human and must be the owner. The
// new decision supersedes the current head, so the record is a linear,
// append-only history whose newest entry is the task's state.
func DecideSensitivityApproval(ctx context.Context, db *postgres.Store, ns, approvalID string, principal ActivationPrincipal, decision, note string) (SensitivityDecision, error) {
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
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "decl-sensitivity:"+ns+":"+approvalID); err != nil {
		return SensitivityDecision{}, err
	}
	approval, err := scanApproval(tx.QueryRow(ctx, `SELECT `+approvalColumns+approvalFrom+` WHERE a.namespace_id=$1 AND a.id=$2`, ns, approvalID))
	if err != nil {
		return SensitivityDecision{}, err
	}
	if approval.Owner != ResolveAuthor(principal) {
		return SensitivityDecision{}, fmt.Errorf("%w: approval %s belongs to %q, not %q", ErrSensitivityNotOwner, approvalID, approval.Owner, principal.Author)
	}
	d := SensitivityDecision{ID: store.NewULID(), ApprovalID: approvalID, Decision: decision, Decider: ResolveAuthor(principal), Note: note, SupersedesID: approval.DecisionID}
	if err := tx.QueryRow(ctx, `INSERT INTO declaration_sensitivity_decisions(id,namespace_id,approval_id,decision,decider,note,supersedes_id)
 VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,'')) RETURNING created_at`, d.ID, ns, approvalID, decision, d.Decider, note, d.SupersedesID).Scan(&d.CreatedAt); err != nil {
		return SensitivityDecision{}, err
	}
	return d, tx.Commit(ctx)
}

// ListSensitivityDecisions returns an approval's whole decision history,
// oldest first.
func ListSensitivityDecisions(ctx context.Context, db *postgres.Store, ns, approvalID string) ([]SensitivityDecision, error) {
	rows, err := db.Pool().Query(ctx, `SELECT id,approval_id,decision,decider,note,COALESCE(supersedes_id,''),created_at FROM declaration_sensitivity_decisions
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

// SensitivityWarnings is publish's half (it warns, never refuses): one
// warning per action.with reference that widens, or may widen, its
// variable's audience. Named steps resolve against each name's newest
// published version.
func SensitivityWarnings(ctx context.Context, db *postgres.Store, ns string, d decl.Declaration) ([]string, error) {
	var lookupErr error
	resolve := func(name string) (decl.Declaration, bool) {
		v, err := db.LatestDeclarationVersion(ctx, ns, name)
		if err != nil {
			if !errors.Is(err, postgres.ErrNotFound) && lookupErr == nil {
				lookupErr = err
			}
			return decl.Declaration{}, false
		}
		var named decl.Declaration
		if json.Unmarshal(v.Body, &named) != nil {
			return decl.Declaration{}, false
		}
		return named, true
	}
	ws, err := decl.WideningReferences(d, resolve)
	if err != nil {
		return nil, err
	}
	if lookupErr != nil {
		return nil, lookupErr
	}
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Warning())
	}
	return out, nil
}
