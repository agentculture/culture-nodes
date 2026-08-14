package devague

import (
	"encoding/json"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// MapFrameClaims maps `devague show --json` (a frame's claims) onto ledger
// records.
//
// Every claim maps to one base record at authority proposed, whose ledger
// record_type follows devague's claim kind:
//
//	announcement                                    -> announcement
//	audience, after_state, before_state,
//	why_it_matters, boundary, non_goal,
//	success_signal, requirement                     -> claim
//	open_question                                   -> question
//	decision                                         -> decision
//	assumption                                       -> assumption
//
// (devague's frame-level success_signal claims land as generic claim
// records, not ledger success_signal records: MapDeliverables is this
// package's sole source of success_signal records, mapped from the plan's
// deliverables preview rather than from the frame.)
//
// If devague recorded the claim as confirmed or rejected, MapFrameClaims
// also emits a second record: a review record, origin always human,
// authority confirmed/rejected, referencing the base record's id in both
// subject_ref and provenance_refs. See the package doc for why this split
// exists and TestAuthorityHonestyMatchesLedgerRules for the proof that the
// split is what the real ledger review path requires.
func MapFrameClaims(showJSON []byte) ([]ledger.Record, error) {
	var frame frameShow
	if err := json.Unmarshal(showJSON, &frame); err != nil {
		return nil, fmt.Errorf("devague: decode frame show json: %w", err)
	}
	if frame.Slug == "" {
		return nil, fmt.Errorf("devague: frame show json has no slug")
	}
	runID := runIDForSlug(frame.Slug)

	out := make([]ledger.Record, 0, len(frame.Claims)*2)
	for _, claim := range frame.Claims {
		if claim.ID == "" {
			return nil, fmt.Errorf("devague: frame %s: a claim has no id", frame.Slug)
		}

		recordType, err := claimRecordType(claim.Kind)
		if err != nil {
			return nil, fmt.Errorf("devague: frame %s claim %s: %w", frame.Slug, claim.ID, err)
		}
		originKind, err := claimOriginKind(claim.Origin)
		if err != nil {
			return nil, fmt.Errorf("devague: frame %s claim %s: %w", frame.Slug, claim.ID, err)
		}

		recID := recordIDForClaim(frame.Slug, claim.ID)
		base, err := newRecord(
			recID,
			recordType,
			runID,
			ledger.Origin{Kind: originKind, ActorID: actorIDFor(originKind)},
			ledger.AuthorityProposed,
			"",
			claimData(frame.Slug, claim),
			nil,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, base)

		verdict, authority, hasDecision, err := reviewForClaimStatus(claim.Status)
		if err != nil {
			return nil, fmt.Errorf("devague: frame %s claim %s: %w", frame.Slug, claim.ID, err)
		}
		if !hasDecision {
			continue
		}

		review, err := newRecord(
			reviewIDForClaim(frame.Slug, claim.ID),
			ledger.RecordReview,
			runID,
			// Confirmation/rejection is always a human review record here,
			// regardless of who authored the underlying claim (PRD §10.4:
			// "no actor promotes its own proposal"). An llm-origin claim's
			// base record above stays origin agent / authority proposed no
			// matter what devague's status field says; only this second,
			// separate record carries confirmed/rejected authority.
			ledger.Origin{Kind: ledger.OriginHuman, ActorID: actorIDFor(ledger.OriginHuman)},
			authority,
			ledger.NullableID(recID),
			reviewData("claim", verdict, recID, claim.Status),
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

// claimRecordType maps a devague claim kind to a ledger record_type. It is
// exhaustive over devague's CLAIM_KINDS (12, as of devague schema v5); an
// unrecognised kind is a decode error, not a silent default, because
// guessing a record_type for a kind this package has never seen would be
// exactly the kind of quiet misclassification the ledger's evidence rules
// exist to rule out elsewhere.
func claimRecordType(kind string) (ledger.RecordType, error) {
	switch kind {
	case "announcement":
		return ledger.RecordAnnouncement, nil
	case "audience", "after_state", "before_state", "why_it_matters",
		"boundary", "non_goal", "success_signal", "requirement":
		return ledger.RecordClaim, nil
	case "open_question":
		return ledger.RecordQuestion, nil
	case "decision":
		return ledger.RecordDecision, nil
	case "assumption":
		return ledger.RecordAssumption, nil
	default:
		return "", fmt.Errorf("unrecognised devague claim kind %q", kind)
	}
}

// claimOriginKind maps devague's claim origin (user/llm) to a ledger
// producer kind. devague's "llm" origin is an agent producer in ledger
// vocabulary: an LLM proposed the claim, which is exactly what
// ledger.OriginAgent means.
func claimOriginKind(origin string) (ledger.OriginKind, error) {
	switch origin {
	case "user":
		return ledger.OriginHuman, nil
	case "llm":
		return ledger.OriginAgent, nil
	default:
		return "", fmt.Errorf("unrecognised devague claim origin %q", origin)
	}
}

// reviewForClaimStatus maps devague's claim status to the review this
// package should emit alongside the base record. A still-proposed claim
// gets no review record: nobody has decided it yet, so there is nothing to
// record beyond the proposal itself.
func reviewForClaimStatus(status string) (verdict string, authority ledger.Authority, hasDecision bool, err error) {
	switch status {
	case "proposed":
		return "", "", false, nil
	case "confirmed":
		return "confirm", ledger.AuthorityConfirmed, true, nil
	case "rejected":
		return "reject", ledger.AuthorityRejected, true, nil
	default:
		return "", "", false, fmt.Errorf("unrecognised devague claim status %q", status)
	}
}

// claimData is the base record's payload: the claim's own text under the
// claim.schema.json-documented `statement` property (used for every record
// type here, not only claim — the payload schemas are all Phase-0 loose:
// named properties are optional and documentary), plus everything else
// devague recorded about the claim, grouped under `devague` so a reader can
// tell this package's own fields from devague's provenance at a glance.
func claimData(frameSlug string, c frameClaim) map[string]any {
	devague := map[string]any{
		"frame":    frameSlug,
		"claim_id": c.ID,
		"kind":     c.Kind,
		"origin":   c.Origin,
		"status":   c.Status,
	}
	if c.Instruction != "" {
		devague["instruction"] = c.Instruction
	}
	if len(c.HonestyConditions) > 0 {
		conditions := make([]map[string]any, 0, len(c.HonestyConditions))
		for _, h := range c.HonestyConditions {
			conditions = append(conditions, map[string]any{
				"id":     h.ID,
				"text":   h.Text,
				"status": h.Status,
			})
		}
		devague["honesty_conditions"] = conditions
	}
	return map[string]any{
		"statement": c.Text,
		"devague":   devague,
	}
}

// reviewData is the review record's payload, shared by every Map* function
// in this package that emits the base-record/review-record split (claims,
// plan tasks, deviations). Its property names follow review.schema.json's
// PRD §10.8 vocabulary (verdict, reviewed_refs) so the record reads like any
// other review the ledger runtime would append. kind names what was
// reviewed ("claim", "task", "deviation") so the comment reads correctly
// regardless of which Map* function called it.
func reviewData(kind, verdict, targetID, devagueStatus string) map[string]any {
	return map[string]any{
		"verdict":       verdict,
		"reviewed_refs": []string{targetID},
		"comment":       "imported from a devague " + kind + " recorded as " + devagueStatus,
		"devague": map[string]any{
			"source": "devague",
			"status": devagueStatus,
		},
	}
}
