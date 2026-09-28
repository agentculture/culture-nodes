// Variable sensitivity at firing time (task t30, #328; spec owner decision
// q22; reworked by task t30b, owner decision d4): a firing whose action would
// render a variable into a system with a wider audience than the variable
// came from (internal/decl/sensitivity.go's one ranking table) is recorded as
// OutcomeSensitivityBlocked and is not dispatched unless the declaration
// lists that variable in its `exposes` list AND the variable's owner has a
// standing approval of that entry.
//
// Decisions this file makes, all failing closed:
//
//   - GitHub ranks per repository (d4). The TARGET repository is the rendered
//     action's input.repository; a SOURCE's repository is the `repository`
//     variable of the event or lineage firing that produced it. Visibility is
//     read from the namespace's repository_visibility record (exposure_store.go),
//     never from GitHub at firing time. Unknown is public as a target and
//     org as a source.
//   - The variable's OWNER is the author of the declaration version that
//     produced it: for a step-0 reference, the firing declaration's own
//     version (its trigger event supplied the value); for a lineage
//     reference, the version the ancestor firing pinned. declaration_versions
//     .author is always the authenticated publishing principal (c89/h62).
//   - Exposure is approved PER VARIABLE (d4): one approval task per
//     (declaration NAME, exposes entry, owner). Republishing the declaration
//     keeps its approved entries; a new author of the producing declaration
//     is a different owner and needs a new approval; removing an entry from
//     the list withdraws the approval (a later relisting needs the owner
//     again). A widening reference the list does NOT carry blocks without
//     opening any task: the author must opt in by listing it.
//   - The block happens BEFORE the firing is claimed, beside the other
//     pre-claim refusals (loop-limited, deferred): nothing is dispatched, no
//     marker minted, no landing node opened, and a later event can still fire
//     once the approval exists. Only a reference whose value is actually
//     present is checked -- a missing value renders its default or the
//     placeholder, which exposes nothing.
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

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// OutcomeSensitivityBlocked: the firing would widen a variable's audience
// and the declaration does not list it in exposes, or the owner has not
// approved (or has refused, or the list has withdrawn) that exposure.
const OutcomeSensitivityBlocked = "sensitivity-blocked"

// Exposure approval states. The newest decision is the state, or pending
// when there is none; an approval whose entry a later version of the
// declaration dropped is withdrawn and blocks until the owner approves again.
const (
	SensitivityPending   = "pending"
	SensitivityApproved  = "approved"
	SensitivityRefused   = "refused"
	SensitivityWithdrawn = "withdrawn"
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

// SensitivityApprovalRequest names one listed widening that needs its owner.
// The key is (NamespaceID, DeclarationName, Variable, Owner); the rest
// describes the first firing that asked, for the owner to read.
type SensitivityApprovalRequest struct {
	NamespaceID, DeclarationName, Variable, Owner string
	DeclarationID, DeclarationVersionID           string
	SourceDeclarationID, SourceVersionID, EventID string
	Source, Target                                decl.Sensitivity
}

// SensitivityBackend is the firing loop's view of sources, repository
// visibility and approvals. A Backend that does not implement it performs no
// sensitivity check; the production PostgresBackend does (asserted below), so
// every real engine enforces it.
type SensitivityBackend interface {
	VersionSource(ctx context.Context, namespaceID, versionID string) (SensitivitySource, error)
	FiringSource(ctx context.Context, namespaceID, firingID string) (SensitivitySource, error)
	// RepositoryVisibility is the namespace's newest record for a
	// lower-case owner/name, or VisibilityUnknown.
	RepositoryVisibility(ctx context.Context, namespaceID, repository string) (decl.Visibility, error)
	DestinationAudience(ctx context.Context, namespaceID, actor string) (decl.Audience, bool, error)
	// RequestSensitivityApproval opens the task for this key, or returns
	// the one already open, with its current state.
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

// visibilityLookup adapts a backend read to decl's lookup, caching per
// firing; the first read error is kept for the caller to return.
func visibilityLookup(ctx context.Context, read func(context.Context, string, string) (decl.Visibility, error), ns string) (decl.RepositoryVisibility, *error) {
	var firstErr error
	cache := map[string]decl.Visibility{}
	return func(repo string) decl.Visibility {
		if v, ok := cache[repo]; ok {
			return v
		}
		v, err := read(ctx, ns, repo)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			v = decl.VisibilityUnknown
		}
		cache[repo] = v
		return v
	}, &firstErr
}

func destinationLookup(ctx context.Context, read func(context.Context, string, string) (decl.Audience, bool, error), ns string) (decl.DestinationAudience, *error) {
	var firstErr error
	cache := map[string]decl.Audience{}
	known := map[string]bool{}
	return func(actor string) (decl.Audience, bool) {
		if v, ok := cache[actor]; ok {
			return v, known[actor]
		}
		v, ok, err := read(ctx, ns, actor)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		cache[actor], known[actor] = v, ok
		return v, ok
	}, &firstErr
}

// checkSensitivity returns a non-empty reason when a present variable the
// action renders would widen its audience without a listed, approved
// exposure.
func (e *Engine) checkSensitivity(ctx context.Context, event Event, a ActiveDeclaration, lineage []Ancestor) (string, decl.Sensitivity, error) {
	sb, ok := e.backend.(SensitivityBackend)
	if !ok {
		return "", decl.Sensitivity{}, nil
	}
	refs, err := decl.ActionReferences(a.Declaration.Action)
	if err != nil {
		return "", decl.Sensitivity{}, err
	}
	vis, visErr := visibilityLookup(ctx, sb.RepositoryVisibility, event.NamespaceID)
	dest, destErr := destinationLookup(ctx, sb.DestinationAudience, event.NamespaceID)
	rendered, err := renderAction(a.Declaration.Action, event.Variables, lineage)
	if err != nil {
		return "", decl.Sensitivity{}, err
	}
	target := decl.TargetSensitivityForAction(rendered, vis, dest)
	if *destErr != nil {
		return "", target, *destErr
	}
	if *visErr != nil {
		return "", target, *visErr
	}
	if len(refs) == 0 {
		return "", target, nil
	}
	var blocked []string
	seen := map[string]bool{}
	for _, ref := range refs {
		idx, found := stepIndex(ref.Step, lineage)
		if !found {
			continue
		}
		vars := event.Variables
		if idx >= 0 {
			vars = lineage[idx].Variables
		}
		entry := decl.ExposureEntry(ref)
		if v, present := vars[ref.Name]; !present || v == nil || seen[entry] {
			continue
		}
		seen[entry] = true
		var src SensitivitySource
		var mark decl.Sensitivity
		if idx < 0 {
			src, err = sb.VersionSource(ctx, event.NamespaceID, a.VersionID)
			mark = decl.TriggerSensitivity(a.Declaration, decl.VariableRepository(vars), vis)
		} else {
			src, err = sb.FiringSource(ctx, event.NamespaceID, lineage[idx].FiringID)
			mark = decl.FiringSensitivity(src.Declaration, strings.ToLower(lineage[idx].EventRepository), decl.VariableRepository(vars), vis, dest)
		}
		if err != nil {
			return "", target, err
		}
		if *visErr != nil {
			return "", target, *visErr
		}
		if *destErr != nil {
			return "", target, *destErr
		}
		if !decl.Widens(mark, target) {
			continue
		}
		flow := fmt.Sprintf("variable {%s} from %s would render into the wider %s", entry, mark, target)
		if !a.Declaration.Lists(entry) {
			blocked = append(blocked, fmt.Sprintf("%s and is not in exposes; add %q to exposes to request owner %q's approval", flow, entry, src.Author))
			continue
		}
		approval, err := sb.RequestSensitivityApproval(ctx, SensitivityApprovalRequest{
			NamespaceID: event.NamespaceID, DeclarationName: a.Declaration.Name, Variable: entry, Owner: src.Author,
			DeclarationID: a.ID, DeclarationVersionID: a.VersionID,
			SourceDeclarationID: src.DeclarationID, SourceVersionID: src.VersionID, EventID: event.ID,
			Source: mark, Target: target,
		})
		if err != nil {
			return "", target, err
		}
		if approval.Status == SensitivityApproved {
			continue
		}
		blocked = append(blocked, fmt.Sprintf("%s; exposes lists it and owner %q must approve (approval %s is %s)", flow, approval.Owner, approval.ID, approval.Status))
	}
	if *visErr != nil {
		return "", target, *visErr
	}
	return strings.Join(blocked, "; "), target, nil
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

// SensitivityWarnings is publish's half (it warns, never refuses): one
// warning per action.with reference that widens, or may widen, its
// variable's audience, saying whether its exposure is unlisted, or listed
// and approved, pending, refused, withdrawn or not yet asked. members
// resolves a declaration set's own not-yet-published bodies first (nil for a
// single declaration); every other named step resolves against that name's
// newest published version, whose author is the variable's owner. A step-0
// variable's owner is the author of d's own newest published version.
func SensitivityWarnings(ctx context.Context, db *postgres.Store, ns string, d decl.Declaration, members func(string) (decl.Declaration, bool)) ([]string, error) {
	var lookupErr error
	note := func(err error) {
		if err != nil && !errors.Is(err, postgres.ErrNotFound) && lookupErr == nil {
			lookupErr = err
		}
	}
	owners := map[string]string{}
	latest := func(name string) (postgres.DeclarationVersion, bool) {
		v, err := db.LatestDeclarationVersion(ctx, ns, name)
		note(err)
		return v, err == nil
	}
	resolve := func(name string) (decl.Declaration, bool) {
		if members != nil {
			if m, ok := members(name); ok {
				return m, true
			}
		}
		v, ok := latest(name)
		if !ok {
			return decl.Declaration{}, false
		}
		var named decl.Declaration
		if json.Unmarshal(v.Body, &named) != nil {
			return decl.Declaration{}, false
		}
		owners[name] = v.Author
		return named, true
	}
	vis, visErr := visibilityLookup(ctx, PostgresBackend{Store: db}.RepositoryVisibility, ns)
	dest, destErr := destinationLookup(ctx, PostgresBackend{Store: db}.DestinationAudience, ns)
	ws, err := decl.WideningReferences(d, resolve, vis, dest)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		if !w.Listed {
			out = append(out, w.Warning(""))
			continue
		}
		owner := ""
		switch _, numeric := strconv.Atoi(w.Reference.Step); {
		case w.Reference.Step == "0":
			if v, ok := latest(d.Name); ok {
				owner = v.Author
			}
		case numeric != nil:
			owner = owners[w.Reference.Step]
		}
		if owner == "" {
			out = append(out, w.Warning("owner known only at firing time; the first blocked firing opens the approval task"))
			continue
		}
		a, err := FindSensitivityApproval(ctx, db, ns, d.Name, w.Entry, owner)
		switch {
		case errors.Is(err, postgres.ErrNotFound):
			out = append(out, w.Warning(fmt.Sprintf("no approval task yet; the first blocked firing opens one for owner %q", owner)))
		case err != nil:
			return nil, err
		default:
			out = append(out, w.Warning(fmt.Sprintf("%s (owner %q, approval %s)", a.Status, owner, a.ID)))
		}
	}
	if lookupErr != nil {
		return nil, lookupErr
	}
	if *visErr != nil {
		return nil, *visErr
	}
	if *destErr != nil {
		return nil, *destErr
	}
	return out, nil
}
