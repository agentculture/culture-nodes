package devague

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// idPrefix names every record and run id this package derives, so a mapped
// record is recognisable as devague-sourced at a glance.
const idPrefix = "dv_"

// runIDForSlug is the ledger run every record mapped from one devague slug
// belongs to. A devague plan's slug always equals its source frame's slug
// (the devague CLI assigns plan.slug = frame.slug at `plan new`), so
// MapFrameClaims, MapPlanWaves, and MapDeliverables — reading the frame's
// slug and the plan's slug respectively — land their records in the same
// run without this package threading a shared identifier between them.
func runIDForSlug(slug string) string { return idPrefix + slug }

// recordIDForClaim is the deterministic id of the record a frame claim maps
// to (see MapFrameClaims).
func recordIDForClaim(frameSlug, claimID string) string {
	return idPrefix + frameSlug + "_" + claimID
}

// reviewIDForClaim is the deterministic id of the review record confirming
// or rejecting a claim's record, when devague recorded a decision.
func reviewIDForClaim(frameSlug, claimID string) string {
	return recordIDForClaim(frameSlug, claimID) + "_review"
}

// recordIDForTask is the deterministic id of the record a plan task maps to
// (see MapPlanWaves and MapPlanShow). Both functions key a task the same
// way deliberately: they describe the SAME logical task from two different
// devague views, so importing both over the same plan is a genuine id
// collision -- InsertRecord's uniqueness (records are immutable and never
// rewritten) is what refuses that, rather than either mapping silently
// drifting from the other.
func recordIDForTask(planSlug, taskID string) string {
	return idPrefix + planSlug + "_" + taskID
}

// recordIDForDeviation is the deterministic id of the record a delivery
// deviation maps to (see MapDeviations).
func recordIDForDeviation(planSlug, deviationID string) string {
	return idPrefix + planSlug + "_" + deviationID
}

// reviewIDForTask is the deterministic id of the review record confirming
// or rejecting a task's record, when devague recorded a decision (see
// MapPlanShow, the plan_show.go twin of reviewIDForClaim).
func reviewIDForTask(planSlug, taskID string) string {
	return recordIDForTask(planSlug, taskID) + "_review"
}

// reviewIDForDeviation is the deterministic id of the review record
// confirming or rejecting a deviation's record, when devague recorded a
// decision (see MapDeviations).
func reviewIDForDeviation(planSlug, deviationID string) string {
	return recordIDForDeviation(planSlug, deviationID) + "_review"
}

// recordIDForSignal is the deterministic id of the record the nth (1-based)
// deliverables success signal maps to (see MapDeliverables).
func recordIDForSignal(planSlug string, oneBasedIndex int) string {
	return fmt.Sprintf("%s%s_signal_%d", idPrefix, planSlug, oneBasedIndex)
}

// actorIDFor names the producer this package stamps on a mapped record's
// origin, by kind. There is exactly one of each because this package speaks
// for devague as a whole, not for an individual devague user account.
func actorIDFor(kind ledger.OriginKind) string {
	switch kind {
	case ledger.OriginAgent:
		return "devague-llm"
	case ledger.OriginHuman:
		return "devague-user"
	case ledger.OriginEngine:
		return "devague-plan-deliverables"
	default:
		return "devague"
	}
}

// newRecord builds one ledger record with every envelope field this package
// owns filled in, and stamps its content digest.
//
// created_at is deliberately left at its zero value. None of the three
// devague --json views this package reads carries a per-claim or per-task
// timestamp (only frameShow carries frame-level created/updated, which no
// single claim or task can honestly claim as its own) — see
// testdata/README.md. A zero created_at is not a fabrication: it is exactly
// what internal/ledger.Ledger.normalize does for a caller-supplied record
// that has not gone through Append yet, so a mapped record is normalize-ready
// rather than carrying a wall-clock time this call happened to run at, which
// would make two mappings of the same fixture bytes diverge on the one field
// determinism most needs to hold.
func newRecord(
	id string,
	recordType ledger.RecordType,
	runID string,
	origin ledger.Origin,
	authority ledger.Authority,
	subjectRef ledger.NullableID,
	data map[string]any,
	provenanceRefs []string,
) (ledger.Record, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return ledger.Record{}, fmt.Errorf("devague: encode data for %s: %w", id, err)
	}
	if provenanceRefs == nil {
		provenanceRefs = []string{}
	} else {
		provenanceRefs = append([]string(nil), provenanceRefs...)
		sort.Strings(provenanceRefs)
	}

	rec := ledger.Record{
		ID:             id,
		SchemaVersion:  ledger.SchemaVersion,
		RecordType:     recordType,
		RunID:          runID,
		Origin:         origin,
		Authority:      authority,
		SubjectRef:     subjectRef,
		Data:           raw,
		ProvenanceRefs: provenanceRefs,
	}

	digest, err := rec.ComputeDigest()
	if err != nil {
		return ledger.Record{}, fmt.Errorf("devague: digest %s: %w", id, err)
	}
	rec.ContentDigest = digest
	return rec, nil
}

// sortRecords orders records by id. Every Map* function returns its result
// this way: it is a total order over deterministic ids, so the slice a
// caller sees does not depend on map-iteration order or on the order devague
// happened to list claims/tasks in.
func sortRecords(records []ledger.Record) {
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
}
