package api

// Declaration sets (task t35, #328; spec c86, honesty h59): the one shape
// both automation-producing lanes -- workflow generation
// (workflow_generations.go) and devague plan import (planimports.go) --
// emit when their caller selects declaration output. A set is N
// declaration sources plus the must/can links between them (where the
// workflow had edges, or the plan had dependency edges).
//
// Validation runs the REAL declaration parser (decl.Parse, the same one
// POST /v1alpha1/declarations/validate runs) over every member, and
// publishing goes through declengine.Publish (the t15/t19 choke point) and
// Store.LinkDeclarations -- exactly what POST /v1alpha1/declarations and
// POST /v1alpha1/declarations/{name}/links do, so a lane can never publish
// something the declaration routes would refuse. Three rules hold here
// because they hold there:
//
//   - the recorded author is the authenticated principal
//     (declarationPrincipal), never anything a request body claims;
//   - publishing never activates: a generated or imported declaration is a
//     proposal until a human activates it (ADR 0014 §3);
//   - warnings (unresolvable template references, and t30's sensitivity
//     widenings) never refuse, and are always surfaced in the lane's output.
//
// A set is validated as a whole BEFORE anything is written, so an invalid
// member or a dangling link refuses the entire set with nothing published.
// Publishing itself is per declaration (each declengine.Publish is its own
// transaction), and repeat publication of an unchanged declaration returns
// the existing version by digest, so retrying a set whose publish failed
// part-way is safe.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/agentculture/culture-nodes/internal/decl"
	"github.com/agentculture/culture-nodes/internal/declengine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// setActivationLookup answers IsActivationDeclaration from the set's own
// members first (a sibling may not be published yet), then from the store.
type setActivationLookup struct {
	members map[string]*decl.Declaration
	store   declengine.StoreActivationLookup
}

func (l setActivationLookup) IsActivationDeclaration(ctx context.Context, namespaceID, name string) (bool, error) {
	if d, ok := l.members[name]; ok {
		return declengine.IsActivationDeclaration(*d), nil
	}
	return l.store.IsActivationDeclaration(ctx, namespaceID, name)
}

// declarationSetItem is one member's source. Format defaults to yaml.
type declarationSetItem struct {
	Format string `json:"format,omitempty"`
	Source string `json:"source"`
}

// declarationSetLink is components.schemas.DeclarationSetLink: From depends
// on To (the declaration links route's from/to direction).
type declarationSetLink struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// declarationSetEntryOut is components.schemas.DeclarationSetEntry.
type declarationSetEntryOut struct {
	Name        string   `json:"name,omitempty"`
	Format      string   `json:"format"`
	Source      string   `json:"source"`
	Valid       bool     `json:"valid"`
	Digest      string   `json:"digest,omitempty"`
	Diagnostics []string `json:"diagnostics"`
	Warnings    []string `json:"warnings"`
	// BaseDigest/Diff: a member whose name is already published is an edit
	// of that declaration, and says so against the version it would replace.
	BaseDigest string `json:"base_digest,omitempty"`
	Diff       string `json:"diff,omitempty"`
	// Set only once published.
	VersionID string `json:"version_id,omitempty"`
	Version   int    `json:"version,omitempty"`
	Author    string `json:"author,omitempty"`
}

// declarationSetOut is components.schemas.DeclarationSet.
type declarationSetOut struct {
	Declarations []declarationSetEntryOut `json:"declarations"`
	Links        []declarationSetLink     `json:"links"`
	Valid        bool                     `json:"valid"`
	Published    bool                     `json:"published"`
	// Diagnostics are set-level problems (duplicate names, bad links).
	Diagnostics []string `json:"diagnostics"`
	// Warnings aggregates every member's warnings as "<name>: <warning>".
	Warnings []string `json:"warnings"`
}

// pendingDeclarationID is the stand-in id a not-yet-published set member
// carries while its proposed links are evaluated for warnings.
func pendingDeclarationID(name string) string { return "pending:" + name }

// validateDeclarationSet parses every member with the real declaration
// parser, checks the links, and computes each member's warnings as if the
// set's links were already recorded. It writes nothing.
func (s *Server) validateDeclarationSet(ctx context.Context, items []declarationSetItem, links []declarationSetLink) (declarationSetOut, []*decl.Declaration, error) {
	out := declarationSetOut{
		Declarations: make([]declarationSetEntryOut, len(items)),
		Links:        append([]declarationSetLink{}, links...),
		Diagnostics:  []string{},
		Warnings:     []string{},
	}
	parsed := make([]*decl.Declaration, len(items))
	if len(items) == 0 {
		out.Diagnostics = append(out.Diagnostics, "the set has no declarations")
	}
	// ids maps each member name (and each already-published name a link
	// reaches) to the id its links resolve to.
	ids := map[string]string{}
	members := map[string]*decl.Declaration{}
	allValid := len(items) > 0
	for i, item := range items {
		format := item.Format
		if format == "" {
			format = string(decl.FormatYAML)
		}
		entry := declarationSetEntryOut{Format: format, Source: item.Source, Diagnostics: []string{}, Warnings: []string{}}
		d, err := decl.Parse([]byte(item.Source), decl.Format(format))
		if err != nil {
			entry.Diagnostics = append(entry.Diagnostics, err.Error())
			allValid = false
			out.Declarations[i] = entry
			continue
		}
		entry.Name, entry.Valid = d.Name, true
		if entry.Digest, err = d.Digest(); err != nil {
			return out, nil, err
		}
		if members[d.Name] != nil {
			out.Diagnostics = append(out.Diagnostics, fmt.Sprintf("declaration %q appears more than once in the set", d.Name))
		}
		members[d.Name], parsed[i] = d, d
		latest, err := s.Store.LatestDeclarationVersion(ctx, s.NamespaceID, d.Name)
		switch {
		case err == nil:
			ids[d.Name] = latest.DeclarationID
			if latest.Digest != entry.Digest {
				canonical, cerr := d.CanonicalJSON()
				if cerr != nil {
					return out, nil, cerr
				}
				entry.BaseDigest = latest.Digest
				entry.Diff = declarationDiff(latest.Digest, latest.Body, canonical)
			}
		case errors.Is(err, postgres.ErrNotFound):
			ids[d.Name] = pendingDeclarationID(d.Name)
		default:
			return out, nil, err
		}
		out.Declarations[i] = entry
	}
	for _, l := range links {
		switch {
		case l.Kind != "must" && l.Kind != "can":
			out.Diagnostics = append(out.Diagnostics, fmt.Sprintf("link %s -> %s: kind must be \"must\" or \"can\", not %q", l.From, l.To, l.Kind))
		case members[l.From] == nil:
			out.Diagnostics = append(out.Diagnostics, fmt.Sprintf("link %s -> %s: %q is not a declaration in this set", l.From, l.To, l.From))
		case l.From == l.To:
			out.Diagnostics = append(out.Diagnostics, fmt.Sprintf("link %s -> %s: a declaration cannot depend on itself", l.From, l.To))
		case members[l.To] == nil:
			v, err := s.Store.LatestDeclarationVersion(ctx, s.NamespaceID, l.To)
			if errors.Is(err, postgres.ErrNotFound) {
				out.Diagnostics = append(out.Diagnostics, fmt.Sprintf("link %s -> %s: %q is neither in this set nor published", l.From, l.To, l.To))
				continue
			}
			if err != nil {
				return out, nil, err
			}
			ids[l.To] = v.DeclarationID
		}
	}
	// Run Publish's own static activation gate (one level deep, c33/h64) on
	// every member now, answering "is the target an activation declaration"
	// from the set first, so a member that Publish would refuse fails
	// validation before anything is written rather than halfway through.
	lookup := setActivationLookup{members: members, store: declengine.StoreActivationLookup{Store: s.Store}}
	for i, d := range parsed {
		if d == nil {
			continue
		}
		if err := declengine.ValidatePublish(ctx, s.NamespaceID, *d, lookup); err != nil {
			out.Declarations[i].Diagnostics = append(out.Declarations[i].Diagnostics, err.Error())
			out.Declarations[i].Valid = false
			allValid = false
		}
	}
	out.Valid = allValid && len(out.Diagnostics) == 0
	if !out.Valid {
		return out, parsed, nil
	}
	resolve := func(name string) (decl.Declaration, bool) {
		if d := members[name]; d != nil {
			return *d, true
		}
		return decl.Declaration{}, false
	}
	for i, d := range parsed {
		var existing []postgres.DeclarationLink
		if id := ids[d.Name]; id != pendingDeclarationID(d.Name) {
			var err error
			if existing, err = s.Store.ListDeclarationLinks(ctx, s.NamespaceID, id); err != nil {
				return out, nil, err
			}
		}
		for _, l := range links {
			if l.From == d.Name {
				existing = append(existing, postgres.DeclarationLink{FromDeclarationID: ids[d.Name], ToDeclarationID: ids[l.To], Kind: l.Kind})
			}
		}
		warnings, err := s.declarationReferenceWarningsWith(ctx, *d, existing, ids, resolve)
		if err != nil {
			return out, nil, err
		}
		out.Declarations[i].Warnings = warnings
	}
	out.Warnings = aggregateSetWarnings(out.Declarations)
	return out, parsed, nil
}

// publishDeclarationSet validates the whole set, then publishes every member
// through declengine.Publish under the authenticated principal and records
// its links. It never activates anything. An invalid set returns the
// validation result together with a 422, having written nothing.
func (s *Server) publishDeclarationSet(ctx context.Context, principal declengine.ActivationPrincipal, items []declarationSetItem, links []declarationSetLink) (declarationSetOut, error) {
	out, parsed, err := s.validateDeclarationSet(ctx, items, links)
	if err != nil {
		return out, internalError(err)
	}
	if !out.Valid {
		return out, unprocessable("fix the diagnostics each declaration and the set report, then publish again",
			"the declaration set does not validate: %d set diagnostic(s)", len(out.Diagnostics))
	}
	ids := map[string]string{}
	var overlap, publishedNames []string
	for i, item := range items {
		v, err := declengine.Publish(ctx, s.Store, declengine.StoreActivationLookup{Store: s.Store}, declengine.PublishInput{
			NamespaceID: s.NamespaceID,
			Body:        []byte(item.Source),
			Format:      decl.Format(out.Declarations[i].Format),
			Principal:   principal,
		})
		if err != nil {
			if !errors.Is(err, declengine.ErrOverlapReportFailed) {
				sort.Strings(publishedNames)
				if len(publishedNames) > 0 {
					err = fmt.Errorf("%w (already published in this set, retry is safe: %s)", err, strings.Join(publishedNames, ", "))
				}
				return out, classifyDeclarationError(err)
			}
			overlap = append(overlap, "declaration published; the overlap report failed: "+err.Error())
		}
		publishedNames = append(publishedNames, v.Name)
		ids[v.Name] = v.DeclarationID
		e := &out.Declarations[i]
		e.VersionID, e.Version, e.Author, e.Digest = v.ID, v.Version, v.Author, v.Digest
	}
	for _, l := range links {
		to, ok := ids[l.To]
		if !ok {
			v, err := s.Store.LatestDeclarationVersion(ctx, s.NamespaceID, l.To)
			if err != nil {
				return out, internalError(err)
			}
			to = v.DeclarationID
		}
		if err := s.Store.LinkDeclarations(ctx, s.NamespaceID, ids[l.From], to, l.Kind); err != nil {
			return out, internalError(err)
		}
	}
	// With the links now recorded, recompute each member's warnings exactly
	// the way POST /v1alpha1/declarations reports them.
	for i, d := range parsed {
		warnings, err := s.declarationReferenceWarningsForID(ctx, ids[d.Name], *d)
		if err != nil {
			warnings = append(out.Declarations[i].Warnings, "reference warnings could not be computed: "+err.Error())
		}
		out.Declarations[i].Warnings = warnings
	}
	out.Warnings = append(aggregateSetWarnings(out.Declarations), overlap...)
	out.Published = true
	return out, nil
}

func aggregateSetWarnings(entries []declarationSetEntryOut) []string {
	all := []string{}
	for _, e := range entries {
		for _, w := range e.Warnings {
			all = append(all, e.Name+": "+w)
		}
	}
	return all
}

// declarationDiff renders a line diff between a published version's
// canonical body and a proposed one, both indented so a change reads per
// field rather than as one replaced line.
func declarationDiff(baseDigest string, base, proposed []byte) string {
	return sourceDiff(baseDigest, indentJSON(base), indentJSON(proposed))
}

func indentJSON(raw []byte) string {
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return string(raw)
	}
	return b.String()
}
