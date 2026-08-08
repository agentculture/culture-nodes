package devague

import (
	"encoding/json"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// MapDeliverables maps `devague plan deliverables --json` onto ledger
// success_signal records — one per confirmed success_signal claim text the
// deliverables preview surfaces.
//
// Unlike MapFrameClaims and MapPlanWaves, these records are authority
// derived, origin engine, not proposed/agent-or-human: `plan deliverables`
// is itself a deterministic, read-only computation over the live frame and
// plan state ("Evaluates the plan gate read-only... never persists"; devague
// never asks a human to sign off on the deliverables *view*, only on the
// claims and tasks it summarises). That is exactly what
// ledger.AuthorityDerived + ledger.OriginEngine mean (PRD §10.4: "a value
// deterministically computed from referenced confirmed or observed
// records"), and unlike the confirmed-review records MapFrameClaims emits,
// ledger.CheckAuthority accepts a derived/engine record unconditionally — no
// review transaction required, because nothing here claims a human decided
// anything devague did not already record as confirmed on the frame side.
//
// The `mechanical: false` on every emitted record is deliberate, not a
// placeholder: `plan deliverables` hands back the claim's prose text, not a
// check spec, and success_signal.schema.json's own doc for the field is "False
// means the signal is not yet machine-checkable, and says so instead of
// implying it is" — which is precisely this case.
//
// provenance_refs is intentionally empty: the deliverables view gives only
// bare success_signal text, with no claim id to correlate it back to a
// MapFrameClaims record, and guessing that correlation by matching text
// would be exactly the kind of unstated inference this codebase's ledger
// rules exist to avoid.
func MapDeliverables(deliverablesJSON []byte) ([]ledger.Record, error) {
	var d planDeliverables
	if err := json.Unmarshal(deliverablesJSON, &d); err != nil {
		return nil, fmt.Errorf("devague: decode plan deliverables json: %w", err)
	}
	if d.Plan == "" {
		return nil, fmt.Errorf("devague: plan deliverables json has no plan slug")
	}
	runID := runIDForSlug(d.Plan)

	out := make([]ledger.Record, 0, len(d.SuccessSignal))
	for i, text := range d.SuccessSignal {
		data := map[string]any{
			"statement":  text,
			"mechanical": false,
			"devague": map[string]any{
				"plan":      d.Plan,
				"converged": d.Converged,
				"index":     i,
			},
		}

		rec, err := newRecord(
			recordIDForSignal(d.Plan, i+1),
			ledger.RecordSuccessSignal,
			runID,
			ledger.Origin{Kind: ledger.OriginEngine, ActorID: actorIDFor(ledger.OriginEngine)},
			ledger.AuthorityDerived,
			"",
			data,
			nil,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}

	sortRecords(out)
	return out, nil
}
