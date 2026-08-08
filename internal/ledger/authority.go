package ledger

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// RunnerManifest declares what a trusted runner directly measures. PRD §10.4
// lets a runner create observed evidence "only for fields they directly
// measured"; this is the caller-supplied statement of which fields those are.
//
// Phase 0 takes the manifest as an append-time parameter. A later task binds
// manifests to registered runner revisions so the runtime resolves the
// allowlist from the actor registry instead of trusting the caller for it;
// until then, the trust boundary is the caller's authentication of the
// runner, and this type is where that trust is made explicit rather than
// assumed.
type RunnerManifest struct {
	// ActorID is the runner this manifest was issued to. It must equal the
	// record's Origin.ActorID: a manifest is a statement about one
	// producer, not a general licence.
	ActorID string
	// ObservableFields lists what the runner directly measures, as RFC 6901
	// JSON Pointers into the evidence record's `data` object — for example
	// "/collection_method", "/measurements/exit_code". A declared pointer
	// covers itself and everything beneath it, so "/measurements" admits
	// the whole measurements object while "/measurements/exit_code" admits
	// only that one reading.
	//
	// An empty list declares that the runner measures nothing, which makes
	// every non-empty payload a refusal. That is the intended default: a
	// runner with no declared coverage cannot observe anything.
	ObservableFields []string
}

// covers reports whether the manifest declares pointer, i.e. whether pointer
// is a declared field or lives beneath one.
func (m RunnerManifest) covers(pointer string) bool {
	for _, declared := range m.ObservableFields {
		if declared == "" {
			// The empty pointer is the whole data object: declaring it
			// means the runner claims to measure the entire payload.
			return true
		}
		if pointer == declared || strings.HasPrefix(pointer, declared+"/") {
			return true
		}
	}
	return false
}

// appendOptions carries the per-append context the authority matrix needs.
//
// reviewTransaction is deliberately unexported and has no exported setter:
// CommitReview is the only code that can set it, which is what makes
// "confirmed authority is reachable only through a review transaction" a
// property of the API surface rather than a convention callers could ignore.
type appendOptions struct {
	manifest          *RunnerManifest
	reviewTransaction bool
}

// AppendOption configures a single append.
type AppendOption func(*appendOptions)

// WithRunnerManifest supplies the manifest that declares what a
// runner-origin record's producer directly measured. Observed evidence
// without one is refused.
func WithRunnerManifest(m RunnerManifest) AppendOption {
	return func(o *appendOptions) {
		copied := m
		copied.ObservableFields = append([]string(nil), m.ObservableFields...)
		o.manifest = &copied
	}
}

func buildAppendOptions(opts []AppendOption) appendOptions {
	var o appendOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// CheckAuthority applies the PRD §10.4 producer/authority matrix to one
// record. It is exported so callers (an API layer, a conformance kit) can ask
// whether a proposed record would be admitted without attempting the append.
//
// The manifest argument is required for, and only consulted for,
// runner-origin records.
func CheckAuthority(rec Record, manifest *RunnerManifest) error {
	return checkAuthority(rec, appendOptions{manifest: manifest})
}

func checkAuthority(rec Record, o appendOptions) error {
	fail := func(rule, detail string) *AuthorityError {
		return &AuthorityError{
			Rule:       rule,
			RecordID:   rec.ID,
			Origin:     rec.Origin.Kind,
			ActorID:    rec.Origin.ActorID,
			Authority:  rec.Authority,
			RecordType: rec.RecordType,
			Detail:     detail,
		}
	}

	if !rec.Authority.Valid() {
		return fail(RuleInvalidAuthority, "authority is not one of the core values in PRD §10.4")
	}
	if rec.Authority == AuthoritySuperseded {
		return fail(RuleSupersededNotAppendable,
			"a record is superseded by a later record naming it in `supersedes`, never by declaring itself so")
	}

	switch rec.Origin.Kind {
	case OriginAgent:
		if rec.Authority != AuthorityProposed {
			return fail(RuleAgentProposesOnly,
				"agents may only propose; an agent saying it is done creates a completion claim, not verified evidence")
		}
		return nil

	case OriginHuman:
		return checkHumanAuthority(rec, o, fail)

	case OriginRunner:
		return checkRunnerAuthority(rec, o, fail)

	case OriginEngine, OriginValidator:
		if rec.Authority != AuthorityDerived {
			return fail(RuleDeterministicDerivedOnly,
				"deterministic producers compute derived records from referenced confirmed or observed records")
		}
		return nil

	default:
		detail := fmt.Sprintf("PRD §10.4 states no producer rule for origin kind %q", rec.Origin.Kind)
		if !rec.Origin.Kind.Valid() {
			detail = fmt.Sprintf("origin kind %q is not one the ledger envelope admits", rec.Origin.Kind)
		}
		return fail(RuleUnknownOrigin, detail)
	}
}

func checkHumanAuthority(rec Record, o appendOptions, fail func(rule, detail string) *AuthorityError) error {
	switch rec.Authority {
	case AuthorityProposed:
		return nil
	case AuthorityConfirmed, AuthorityRejected:
		if !o.reviewTransaction {
			return fail(RuleHumanReviewOnly,
				"confirmation and rejection are review transactions (PRD §10.8), not ordinary appends")
		}
		if rec.RecordType != RecordReview {
			return fail(RuleReviewRecordOnly,
				"a review transaction appends review records referencing their targets; it never rewrites a target")
		}
		return nil
	default:
		return fail(RuleHumanAuthorityLimited,
			"observed belongs to a measuring runner and derived to deterministic computation; a person asserting either would be claiming coverage nobody measured")
	}
}

func checkRunnerAuthority(rec Record, o appendOptions, fail func(rule, detail string) *AuthorityError) error {
	if rec.RecordType != RecordEvidence {
		return fail(RuleRunnerEvidenceOnly,
			"a runner reports observations; claims, results, and decisions about them belong to other producers")
	}
	if rec.Authority != AuthorityObserved {
		return fail(RuleRunnerObservedOnly,
			"a runner has no standing to propose, confirm, or derive")
	}
	if o.manifest == nil {
		return fail(RuleRunnerManifestRequired,
			"observed evidence must name the manifest declaring which fields the runner directly measured")
	}
	if o.manifest.ActorID == "" || o.manifest.ActorID != rec.Origin.ActorID {
		return fail(RuleRunnerActorMismatch,
			fmt.Sprintf("manifest belongs to actor %q, record origin is actor %q", o.manifest.ActorID, rec.Origin.ActorID))
	}

	pointers, err := dataLeafPointers(rec.Data)
	if err != nil {
		return fmt.Errorf("ledger: record %s: %w", rec.ID, err)
	}
	for _, pointer := range pointers {
		if o.manifest.covers(pointer) {
			continue
		}
		e := fail(RuleRunnerFieldNotDeclared,
			"the runner's manifest does not declare this field as directly measured, so it is process-reported content, not an observation")
		e.Field = pointer
		return e
	}
	return nil
}

// dataLeafPointers returns the RFC 6901 pointers of every leaf in a record
// payload, sorted, so a refusal names the same offending field every time.
//
// Objects are walked; arrays and scalars are leaves at the pointer of the
// property holding them. An array is a leaf because a runner declares that it
// measured "the artifact references", not each index separately.
func dataLeafPointers(data json.RawMessage) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("decode data payload: %w", err)
	}

	var out []string
	var walk func(prefix string, value any)
	walk = func(prefix string, value any) {
		object, ok := value.(map[string]any)
		if !ok || len(object) == 0 {
			if prefix != "" {
				out = append(out, prefix)
			}
			return
		}
		for key, child := range object {
			walk(prefix+"/"+escapeJSONPointerToken(key), child)
		}
	}
	walk("", decoded)

	sort.Strings(out)
	return out, nil
}

func escapeJSONPointerToken(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	return strings.ReplaceAll(token, "/", "~1")
}
