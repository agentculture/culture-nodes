package ledger

import "sort"

// VerifyClaimChain checks a run's decompose chain, generically (task t24,
// issue #45): every live claim carries at least one source, and every live
// decision or task traces back — through its own provenance_refs — to at
// least one claim that motivates it.
//
// This is the "verification checks the chain" surface the generic
// decompose-pipeline honesty condition (h19) asks for: document -> claims
// (with sources) -> connected decisions and actions -> verified in the end.
// It reads nothing but the generic ledger envelope this package already
// defines (record_type, data.sources, provenance_refs) — no code-repo, no
// devague, no newsletter concept appears anywhere in this file. The same
// function verifies a devague-imported plan's records (internal/devague's
// MapPlanShow already connects a task to the claims it `covers` through
// ProvenanceRefs — see coveredClaimRefs there) and a newsletter run's
// records without a single branch distinguishing the two: genericity is
// proven by reuse, not asserted by a comment.
//
// A record's `sources` and the caller's own linking discipline are both
// conventions this package documents (claim.schema.json's `sources`
// property, decision/task's use of ProvenanceRefs) rather than requirements
// the append path enforces — exactly the way every other Phase-0 payload
// property is documentary, not schema-required. VerifyClaimChain is how an
// author or a validator checks whether a given run actually followed the
// convention, after the fact.
//
// It is a pure function: the same records always produce the same
// ChainVerification, which is what lets a caller record the verdict as a
// deterministic, computed fact (PRD §10.4's `derived` authority) rather than
// a live judgment call. Whether that verdict is actually appended, and under
// what authority, is entirely the caller's decision — this function never
// touches a store or the ledger's own Append path, mirroring
// internal/devague's own "pure function from records to a result; feeding it
// through Append is a caller concern" discipline (see devague's package
// doc).
func VerifyClaimChain(records []Record) ChainVerification {
	live := Live(records)

	claimIDs := make(map[string]bool)
	var claims []ClaimSourceCheck
	for _, rec := range live {
		if rec.RecordType != RecordClaim {
			continue
		}
		claimIDs[rec.ID] = true
		claims = append(claims, ClaimSourceCheck{
			ClaimID:     rec.ID,
			Sourced:     claimSourceCount(rec) > 0,
			SourceCount: claimSourceCount(rec),
		})
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].ClaimID < claims[j].ClaimID })

	var motivated []MotivatedCheck
	for _, rec := range live {
		if rec.RecordType != RecordDecision && rec.RecordType != RecordTask {
			continue
		}
		var refs []string
		for _, ref := range rec.ProvenanceRefs {
			if claimIDs[ref] {
				refs = append(refs, ref)
			}
		}
		sort.Strings(refs)
		motivated = append(motivated, MotivatedCheck{
			RecordID:   rec.ID,
			RecordType: rec.RecordType,
			Motivated:  len(refs) > 0,
			ClaimRefs:  refs,
		})
	}
	sort.Slice(motivated, func(i, j int) bool { return motivated[i].RecordID < motivated[j].RecordID })

	passed := true
	for _, c := range claims {
		if !c.Sourced {
			passed = false
		}
	}
	for _, m := range motivated {
		if !m.Motivated {
			passed = false
		}
	}

	return ChainVerification{
		Passed:    passed,
		Claims:    claims,
		Motivated: motivated,
	}
}

// claimSourceCount reads the claim's declared `sources` array length,
// defaulting to 0 when the payload cannot be decoded or carries no such
// property — the same defensive-read discipline record.go's own dataString/
// dataBool helpers use for a loose Phase-0 payload.
func claimSourceCount(rec Record) int {
	data, err := rec.DataMap()
	if err != nil {
		return 0
	}
	sources, ok := data["sources"].([]any)
	if !ok {
		return 0
	}
	return len(sources)
}

// ChainVerification is the domain-agnostic result of VerifyClaimChain.
type ChainVerification struct {
	// Passed is true only when every live claim is sourced and every live
	// decision/task is motivated by at least one of them. A run with no
	// claims, decisions, or tasks at all passes vacuously — there is nothing
	// to fail — matching runners.AcceptanceVerdict's own vacuous-pass
	// convention for an empty requirement list.
	Passed    bool               `json:"passed"`
	Claims    []ClaimSourceCheck `json:"claims"`
	Motivated []MotivatedCheck   `json:"motivated"`
}

// ClaimSourceCheck is one live claim's sourcing check.
type ClaimSourceCheck struct {
	ClaimID     string `json:"claim_id"`
	Sourced     bool   `json:"sourced"`
	SourceCount int    `json:"source_count"`
}

// MotivatedCheck is one live decision or task's connection check: whether
// its ProvenanceRefs name at least one live claim.
type MotivatedCheck struct {
	RecordID   string     `json:"record_id"`
	RecordType RecordType `json:"record_type"`
	Motivated  bool       `json:"motivated"`
	// ClaimRefs is the subset of the record's ProvenanceRefs that actually
	// resolve to a live claim in this same record set — never the whole
	// ProvenanceRefs list, which may also name evidence or other decisions.
	ClaimRefs []string `json:"claim_refs"`
}
