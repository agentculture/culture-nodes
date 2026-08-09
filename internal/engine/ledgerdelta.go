package engine

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
)

// prepareDelta is §12.5 step 3: validate the proposed ledger delta and its
// producer authority, before a single record is written.
//
// It answers three questions in order, and any "no" rejects the whole delta:
//
//  1. is the delta within the node's record budget (PRD §10.7)?
//  2. does the node's contract permit this record type at this authority —
//     `propose` for proposed records, `observe` for observed ones?
//  3. does the producer have the authority it claims (PRD §10.4)?
//
// Question 2 is the *contract* and question 3 is the *matrix*, and they are
// genuinely different: a node may declare `propose: [result]` while the actor
// that ran it is an agent claiming `observed` — permitted by the workflow,
// refused by the matrix. Declaring `observe` does not grant an agent the
// right to issue observed evidence.
//
// The whole delta is rejected together rather than partially applied, because
// a delta is one actor's account of what it did; keeping half of it would
// leave a record of work nobody claimed.
//
// Records are fully normalized and schema-validated here, mirroring
// ledger.Ledger's own normalization, so that ledger.Append — which re-checks
// everything — cannot fail for a reason this pre-flight did not already
// catch. That is what lets a bad delta be *recorded* as contract_rejected
// instead of aborting the transaction and leaving the work to be redelivered
// forever.
func (e *Engine) prepareDelta(
	wf *Workflow,
	node *Node,
	run Run,
	nodeRun NodeRun,
	attemptID string,
	req CompletionRequest,
	now time.Time,
) ([]ledger.Record, *Rejection) {
	if len(req.LedgerDelta) == 0 {
		return nil, nil
	}

	if budget := recordBudget(wf, node); budget > 0 && len(req.LedgerDelta) > budget {
		return nil, &Rejection{
			Kind: RejectionLedger,
			Detail: fmt.Sprintf("delta carries %d records; node %q may write at most %d",
				len(req.LedgerDelta), node.ID, budget),
		}
	}

	prepared := make([]ledger.Record, 0, len(req.LedgerDelta))
	for i, record := range req.LedgerDelta {
		normalized, rejection := e.prepareRecord(node, run, nodeRun, attemptID, record, req.RunnerManifest, now)
		if rejection != nil {
			rejection.Detail = fmt.Sprintf("ledger delta record %d: %s", i, rejection.Detail)
			return nil, rejection
		}
		prepared = append(prepared, normalized)
	}
	return prepared, nil
}

func recordBudget(wf *Workflow, node *Node) int {
	if node.MaxRecords > 0 {
		return node.MaxRecords
	}
	return wf.Ledger.MaxRecordsPerNode
}

func (e *Engine) prepareRecord(
	node *Node,
	run Run,
	nodeRun NodeRun,
	attemptID string,
	record ledger.Record,
	manifest *ledger.RunnerManifest,
	now time.Time,
) (ledger.Record, *Rejection) {
	record = record.Clone()

	// Run, node run, and attempt are the engine's facts about where this
	// record came from, not the actor's claims about it. They are stamped
	// unconditionally, so a record cannot attribute itself to a different
	// attempt than the one that produced it.
	record.RunID = run.ID
	record.NodeRunID = ledger.NullableID(nodeRun.ID)
	record.AttemptID = ledger.NullableID(attemptID)

	if record.Authority == "" {
		record.Authority = ledger.AuthorityProposed
	}

	// The node's declared ledger delta contract (§10.7).
	switch record.Authority {
	case ledger.AuthorityProposed:
		if !node.permits(node.Propose, string(record.RecordType)) {
			return ledger.Record{}, &Rejection{
				Kind: RejectionLedger,
				Detail: fmt.Sprintf("node %q does not declare propose of record type %q (it declares %v)",
					node.ID, record.RecordType, node.Propose),
			}
		}
	case ledger.AuthorityObserved:
		if !node.permits(node.Observe, string(record.RecordType)) {
			return ledger.Record{}, &Rejection{
				Kind: RejectionLedger,
				Detail: fmt.Sprintf("node %q does not declare observe of record type %q (it declares %v)",
					node.ID, record.RecordType, node.Observe),
			}
		}
	default:
		return ledger.Record{}, &Rejection{
			Kind: RejectionLedger,
			Detail: fmt.Sprintf("a node completion may propose or observe; authority %q is not available to it — "+
				"confirmed and rejected are review transactions (PRD §10.8) and derived belongs to deterministic producers",
				record.Authority),
		}
	}

	// Normalization mirrors ledger.Ledger.normalize: fill only what the
	// runtime owns, never rewrite a caller's statement, and truncate the
	// timestamp to the resolution PostgreSQL stores so the digest survives a
	// round trip.
	if record.ID == "" {
		record.ID = ledger.IDPrefix + store.NewULID()
	}
	if record.SchemaVersion == "" {
		record.SchemaVersion = ledger.SchemaVersion
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	record.CreatedAt = record.CreatedAt.UTC().Truncate(time.Microsecond)
	if len(record.Data) == 0 {
		record.Data = json.RawMessage(`{}`)
	}
	if record.ProvenanceRefs == nil {
		record.ProvenanceRefs = []string{}
	}

	digest, err := record.ComputeDigest()
	if err != nil {
		return ledger.Record{}, &Rejection{Kind: RejectionLedger, Detail: err.Error()}
	}
	record.ContentDigest = digest

	if err := e.validator.Validate(contracts.SchemaLedgerRecord, record); err != nil {
		return ledger.Record{}, &Rejection{Kind: RejectionLedger, Detail: err.Error()}
	}
	// PRD §10.4's producer matrix, the same check ledger.Append applies.
	if err := ledger.CheckAuthority(record, manifest); err != nil {
		return ledger.Record{}, &Rejection{Kind: RejectionLedger, Detail: err.Error()}
	}

	return record, nil
}
