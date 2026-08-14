package devague

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// Deviation is one validated execution-time deviation from a confirmed plan
// -- the typed shape ParseDeviations returns, matching PlanTask's role for
// ParsePlanShow: parse+validate happens once, here, regardless of whether
// the caller wants ledger records (MapDeviations) or a durable-store row
// (internal/store/postgres, internal/api).
type Deviation struct {
	ID      string
	What    string
	TaskRef string
	Reason  string
	Affects []string
	// Origin is the ledger producer kind devague's own user|llm origin maps
	// to -- human or agent. RawOrigin keeps devague's own word alongside it
	// (see PlanTask.RawOrigin for why both are kept).
	Origin    ledger.OriginKind
	RawOrigin string
	// SourceStatus is devague's own deviation status verbatim -- proposed,
	// approved, or rejected (devague/delivery.py DEVIATION_STATUSES). Note
	// "approved", not "confirmed" -- see reviewForDeviationStatus.
	SourceStatus   string
	Classification string // acceptable | risky | needs-follow-up | ""
}

// Deviations is the validated result of ParseDeviations.
type Deviations struct {
	PlanSlug   string
	Deviations []Deviation
}

// ParseDeviations decodes and validates `.devague/deliveries/<plan
// slug>.json` (see deliveryFile's doc comment for exactly which shape).
// Like ParsePlanShow, it never returns a partially valid result: a missing
// plan_slug, a deviation with no id or no task_ref, a duplicate deviation
// id, or an unrecognised origin/status is refused with an error naming what
// is wrong, before any caller (MapDeviations, the plan-import API route,
// the plan-import CLI verb) sees a single Deviation.
func ParseDeviations(deliveryJSON []byte) (Deviations, error) {
	var raw deliveryFile
	if err := json.Unmarshal(deliveryJSON, &raw); err != nil {
		return Deviations{}, fmt.Errorf("devague: decode delivery json: %w", err)
	}
	if raw.PlanSlug == "" {
		return Deviations{}, fmt.Errorf("devague: delivery json has no plan_slug")
	}

	seen := make(map[string]bool, len(raw.Deviations))
	out := make([]Deviation, 0, len(raw.Deviations))
	for _, d := range raw.Deviations {
		if d.ID == "" {
			return Deviations{}, fmt.Errorf("devague: delivery %s: a deviation has no id", raw.PlanSlug)
		}
		if seen[d.ID] {
			return Deviations{}, fmt.Errorf("devague: delivery %s: deviation id %s appears more than once", raw.PlanSlug, d.ID)
		}
		seen[d.ID] = true
		if d.TaskRef == "" {
			return Deviations{}, fmt.Errorf(
				"devague: delivery %s deviation %s has no task_ref; run 'devague deviate --list --json' and check it names the plan task the deviation relates to",
				raw.PlanSlug, d.ID)
		}
		origin, err := claimOriginKind(d.Origin)
		if err != nil {
			return Deviations{}, fmt.Errorf("devague: delivery %s deviation %s: %w", raw.PlanSlug, d.ID, err)
		}
		if _, _, _, err := reviewForDeviationStatus(d.Status); err != nil {
			return Deviations{}, fmt.Errorf("devague: delivery %s deviation %s: %w", raw.PlanSlug, d.ID, err)
		}

		out = append(out, Deviation{
			ID:             d.ID,
			What:           d.What,
			TaskRef:        d.TaskRef,
			Reason:         d.Reason,
			Affects:        append([]string(nil), d.Affects...),
			Origin:         origin,
			RawOrigin:      d.Origin,
			SourceStatus:   d.Status,
			Classification: d.Classification,
		})
	}

	return Deviations{PlanSlug: raw.PlanSlug, Deviations: out}, nil
}

// reviewForDeviationStatus is deviations.go's twin of claims.go's
// reviewForClaimStatus: it cannot reuse that function directly because
// devague spells a deviation's ratified state "approved", not "confirmed"
// (deliveryDeviation's doc comment). The three-way shape --
// no-decision-yet / ratified / rejected -- is otherwise identical.
func reviewForDeviationStatus(status string) (verdict string, authority ledger.Authority, hasDecision bool, err error) {
	switch status {
	case "proposed":
		return "", "", false, nil
	case "approved":
		return "confirm", ledger.AuthorityConfirmed, true, nil
	case "rejected":
		return "reject", ledger.AuthorityRejected, true, nil
	default:
		return "", "", false, fmt.Errorf("unrecognised devague deviation status %q", status)
	}
}

// MapDeviations maps `.devague/deliveries/<plan slug>.json` onto ledger
// records — one per deviation, carrying the origin distinction (user vs
// llm, i.e. "the system knows" vs "the user reports": issue #45's split)
// that is the entire point of importing deviations at all.
//
// # Record type
//
// A deviation is mapped to ledger.RecordDecision. PRD §10.2's MVP record
// set has no dedicated "deviation" type, and this task does not add one
// (that would need a new schemas/ledger/*.schema.json file and RecordTypes()
// registration — real scope, deliberately left for whichever task next
// needs deviations to be more than a decision-shaped record). decision.
// schema.json's own description — "a selected option and the authority
// that selected it" — already fits: a deviation IS a recorded choice
// (depart from the plan this way) with a reason, which is exactly what a
// decision record states. See claimData/claims.go's precedent for putting
// data.statement on a record type whose schema does not name that property
// itself (Phase-0 payload schemas are loose, not additionalProperties:
// false) — the same convention this function's data payload uses.
//
// # Authority: evaluating and (mostly) adopting the prior reasoning
//
// A prior pass at this task argued the ledger envelope should stay
// `proposed` for every mapped deviation record, preserving origin (human vs
// agent) and any devague-recorded approval only as SOURCE status in the
// payload — never a second record carrying ledger-confirmed authority —
// on the grounds that a deterministic importer translating an external
// system's own decision is not the same thing as this ledger confirming
// anything, and manufacturing authority the import did not earn is exactly
// what PRD §10.4 / CLAUDE.md's ledger authority model forbids.
//
// The guardrail half of that argument is correct and is kept here without
// qualification: this function never stamps the ledger's `derived`
// authority value (per PRD §10.4, that value means "deterministically
// computed from referenced confirmed or observed ledger records" —
// translating devague's own already-decided fact is not that), and the
// base record below is unconditionally origin-mapped + proposed authority,
// exactly as the prior pass wanted — an agent-origin (llm) deviation's own
// record can never itself carry anything but proposed, matching PRD
// §10.4's "no actor promotes its own proposal".
//
// Where this function departs is the second half: whether devague's own
// approved/rejected decision should be dropped to payload-only metadata, or
// preserved as a companion ledger.RecordReview the way MapFrameClaims
// (claims.go) and MapPlanShow (plan_show.go) already do for claims and
// tasks. Three reasons favour keeping the review-record split here too,
// rather than treating deviations as a special case:
//
//  1. It does not manufacture anything CheckAuthority would honor. A
//     mapped review record is proposed as human-origin/confirmed and is
//     proved (authority_test.go's TestConfirmedReviewRecordsRequireA-
//     RealReviewTransaction, and this package's deviations_test.go
//     extension of the same property) to be REFUSED by a bare
//     ledger.CheckAuthority call — confirmed/rejected authority is
//     reachable only through a real CommitReview transaction. The mapped
//     record is therefore inert data shaped like a review, not usable
//     ledger-confirmed authority; "manufactured authority" would mean this
//     shape bypassing that check, and it provably does not.
//  2. Devague's own model makes a user-origin deviation auto-approve and an
//     llm-origin one require an explicit human --confirm
//     (devague/delivery.py add_deviation), structurally identical to how a
//     user-origin claim auto-confirms and an llm-origin one needs
//     `devague confirm` (devague/frame.py). A deviation's "approved" is not
//     a different kind of fact than a claim's "confirmed" — it is the same
//     kind of fact (a human ratified an agent's proposal, or authored it
//     themselves), spelled differently by devague's own vocabulary
//     (deliveryDeviation's doc comment). Treating it differently from
//     claims/tasks for that reason alone would be an arbitrary
//     inconsistency, not a more careful reading.
//  3. Dropping it loses real information a ledger-shaped projection
//     (ledger.ConfirmedClaims's review-record-reading pattern, or a future
//     equivalent for decisions) would otherwise be able to see: that a
//     human really did ratify this deviation inside devague, not merely
//     that devague's payload SAYS "approved". MapDeliverables (deliverables.go)
//     accepts a real information loss (no claim-id correlation) because the
//     source genuinely does not carry enough to do better without guessing;
//     that is not the situation here — devague's origin/status fields are
//     exactly as precise for a deviation as for a claim.
//
// What IS narrowed relative to claims/tasks, taking the prior pass's
// caution seriously rather than dismissing it: the review record's origin
// actor id is the same synthetic "devague-user" identity actorIDFor already
// stamps for claims and tasks — explicitly not a specific registered
// culture-nodes human actor and never presented as one. This function
// states a decision devague's own model already recorded; it does not
// attribute that decision to any particular reviewer this ledger could
// authenticate.
func MapDeviations(deliveryJSON []byte) ([]ledger.Record, error) {
	deliveries, err := ParseDeviations(deliveryJSON)
	if err != nil {
		return nil, err
	}
	runID := runIDForSlug(deliveries.PlanSlug)

	out := make([]ledger.Record, 0, len(deliveries.Deviations)*2)
	for _, d := range deliveries.Deviations {
		subjectRef := recordIDForTask(deliveries.PlanSlug, d.TaskRef)

		provenance := []string{subjectRef}
		for _, ref := range d.Affects {
			// Only a task-shaped affects ref is resolved to a real
			// provenance ref: deliveryFile carries no frame_slug, so a
			// claim/honesty-shaped ref (c*/h*) cannot be turned into
			// recordIDForClaim's id without guessing which frame this
			// plan's deviations belong to — the same "do not correlate
			// what the source did not hand us" discipline MapDeliverables
			// states for its own inability to link a success_signal back
			// to its claim (deliverables.go).
			if isTaskID(ref) && ref != d.TaskRef {
				provenance = append(provenance, recordIDForTask(deliveries.PlanSlug, ref))
			}
		}
		sort.Strings(provenance)

		data := map[string]any{
			"statement": d.What,
			"rationale": d.Reason,
			"devague": map[string]any{
				"plan":           deliveries.PlanSlug,
				"deviation_id":   d.ID,
				"task_ref":       d.TaskRef,
				"reason":         d.Reason,
				"affects":        d.Affects,
				"origin":         d.RawOrigin,
				"status":         d.SourceStatus,
				"classification": d.Classification,
			},
		}

		recID := recordIDForDeviation(deliveries.PlanSlug, d.ID)
		base, err := newRecord(
			recID,
			ledger.RecordDecision,
			runID,
			ledger.Origin{Kind: d.Origin, ActorID: actorIDFor(d.Origin)},
			ledger.AuthorityProposed,
			ledger.NullableID(subjectRef),
			data,
			provenance,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, base)

		verdict, authority, hasDecision, err := reviewForDeviationStatus(d.SourceStatus)
		if err != nil {
			return nil, fmt.Errorf("devague: delivery %s deviation %s: %w", deliveries.PlanSlug, d.ID, err)
		}
		if !hasDecision {
			continue
		}

		review, err := newRecord(
			reviewIDForDeviation(deliveries.PlanSlug, d.ID),
			ledger.RecordReview,
			runID,
			ledger.Origin{Kind: ledger.OriginHuman, ActorID: actorIDFor(ledger.OriginHuman)},
			authority,
			ledger.NullableID(recID),
			reviewData("deviation", verdict, recID, d.SourceStatus),
			[]string{recID},
		)
		if err != nil {
			return nil, err
		}
		out = append(out, review)
	}

	sortRecords(out)
	return out, nil
}
