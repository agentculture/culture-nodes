package compiler

import (
	"sort"

	"github.com/agentculture/culture-nodes/internal/contracts"
)

// normalize turns the validated authoring document into the IR the runtime
// executes (PRD §11.3): defaults expanded, owners resolved, outcomes resolved,
// edges in one canonical order, presentation lifted out of the executable
// spec, and inline schemas content-addressed.
//
// It mutates the decoded document, which is safe because Compile decodes a
// fresh one per call and the caller's bytes are kept verbatim in Source.
func (c *compilation) normalize() (*IR, error) {
	doc := c.doc

	ir := &IR{
		APIVersion: doc.APIVersion,
		Kind:       doc.Kind,
		Metadata:   doc.Metadata,
		Spec: irSpec{
			Entry:    doc.Spec.Entry,
			Contract: doc.Spec.Contract,
			Limits:   expandLimits(doc.Spec.Limits),
			Ledger:   expandLedgerLimits(doc.Spec.Ledger),
			Nodes:    doc.Spec.Nodes,
			Edges:    normalizeEdges(doc.Spec.Edges),
		},
	}

	if err := stampSchemaDigest(ir.Spec.Contract.Input); err != nil {
		return nil, err
	}
	if err := stampSchemaDigest(ir.Spec.Contract.Output); err != nil {
		return nil, err
	}

	nodePresentation := make(map[string]map[string]any)
	for _, id := range c.nodeIDs {
		n := doc.Spec.Nodes[id]
		n.OwnerRef = resolveOwner(n.OwnerRef, doc.Metadata.OwnerRef)
		n.Outcomes = declaredOutcomes(n)
		expandNodePolicy(n)
		expandOperation(n)
		expandHookOperations(n)
		if err := stampNodeSchemaDigests(n); err != nil {
			return nil, err
		}
		if len(n.Presentation) > 0 {
			nodePresentation[id] = n.Presentation
		}
		n.Presentation = nil
	}

	if len(doc.Spec.Presentation) > 0 || len(nodePresentation) > 0 {
		ir.Presentation = &presentation{Workflow: doc.Spec.Presentation, Nodes: nodePresentation}
	}
	doc.Spec.Presentation = nil

	return ir, nil
}

func expandLimits(authored *limits) limits {
	out := limits{
		MaxDuration:       DefaultMaxDuration,
		MaxTransitions:    intPtr(DefaultMaxTransitions),
		MaxVisitsPerNode:  intPtr(DefaultMaxVisitsPerNode),
		MaxParallelTokens: intPtr(DefaultMaxParallelTokens),
	}
	if authored == nil {
		return out
	}
	if authored.MaxDuration != "" {
		out.MaxDuration = authored.MaxDuration
	}
	if authored.MaxTransitions != nil {
		out.MaxTransitions = authored.MaxTransitions
	}
	if authored.MaxVisitsPerNode != nil {
		out.MaxVisitsPerNode = authored.MaxVisitsPerNode
	}
	if authored.MaxParallelTokens != nil {
		out.MaxParallelTokens = authored.MaxParallelTokens
	}
	return out
}

func expandLedgerLimits(authored *ledgerLimits) ledgerLimits {
	out := ledgerLimits{
		SchemaVersion:     DefaultLedgerSchemaVersion,
		MaxRecordsPerNode: intPtr(DefaultMaxRecordsPerNode),
		RequireProvenance: boolPtr(true),
	}
	if authored == nil {
		return out
	}
	if authored.SchemaVersion != "" {
		out.SchemaVersion = authored.SchemaVersion
	}
	if authored.MaxRecordsPerNode != nil {
		out.MaxRecordsPerNode = authored.MaxRecordsPerNode
	}
	if authored.RequireProvenance != nil {
		out.RequireProvenance = authored.RequireProvenance
	}
	// maxPayloadBytes has no PRD-stated default: an unstated bound is left
	// unstated rather than invented.
	out.MaxPayloadBytes = authored.MaxPayloadBytes
	return out
}

// expandNodePolicy gives every work-dispatching node an explicit timeout and
// retry policy, and every approval node an explicit deadline, so the runtime
// never has to consult a default table it might disagree with.
func expandNodePolicy(n *node) {
	if n.Kind == KindApproval && n.Deadline == "" {
		n.Deadline = DefaultApprovalDeadline
	}
	if !dispatchesWork(n.Kind) {
		return
	}
	if n.Policy == nil {
		n.Policy = &nodePolicy{}
	}
	if n.Policy.Timeout == "" {
		n.Policy.Timeout = DefaultNodeTimeout
	}
	if n.Policy.Retry == nil {
		n.Policy.Retry = &retryPolicy{}
	}
	if n.Policy.Retry.MaxAttempts == nil {
		n.Policy.Retry.MaxAttempts = intPtr(DefaultRetryMaxAttempts)
	}
	if n.Policy.Retry.Backoff == "" {
		if *n.Policy.Retry.MaxAttempts > 1 {
			n.Policy.Retry.Backoff = DefaultRetryBackoffExponential
		} else {
			n.Policy.Retry.Backoff = DefaultRetryBackoffNone
		}
	}
}

// expandOperation applies the PRD §13.7 safe defaults to a code node's own
// operation.
func expandOperation(n *node) {
	if n.Operation == nil {
		return
	}
	expandOperationValue(n.Operation)
}

// expandHookOperations applies the same §13.7 safe defaults to a node's
// pre_run/post_run hook operations (task t14), so the IR always carries
// explicit values for the worker to dispatch against rather than leaving it
// to infer defaults at dispatch time.
func expandHookOperations(n *node) {
	if n.PreRun != nil {
		expandOperationValue(&n.PreRun.Operation)
	}
	if n.PostRun != nil {
		expandOperationValue(&n.PostRun.Operation)
	}
}

// expandOperationValue is the shared default-expansion logic for any declared
// code operation, whether it is a code node's own operation or a
// pre_run/post_run hook's.
func expandOperationValue(op *codeOperation) {
	if op.Network == "" {
		op.Network = DefaultNetwork
	}
	if op.WorkingDirectory == "" {
		op.WorkingDirectory = DefaultWorkingDirectory
	}
	if len(op.AllowedOutputPaths) == 0 {
		op.AllowedOutputPaths = []string{DefaultAllowedOutputPath}
	}
	if op.RequiresShell == nil {
		op.RequiresShell = boolPtr(false)
	}
}

func stampNodeSchemaDigests(n *node) error {
	if n.Contract == nil {
		return nil
	}
	if err := stampSchemaDigest(n.Contract.Input); err != nil {
		return err
	}
	if err := stampSchemaDigest(n.Contract.Error); err != nil {
		return err
	}
	for _, outcome := range sortedKeys(n.Contract.Outcomes) {
		if err := stampSchemaDigest(n.Contract.Outcomes[outcome]); err != nil {
			return err
		}
	}
	return nil
}

// stampSchemaDigest content-addresses an inline schema (PRD §11.3, "bundled
// schema digests"). A schemaRef is left alone: resolving it needs a source
// root, which is the deployment level's job.
func stampSchemaDigest(source *schemaSource) error {
	if source == nil || source.Schema == nil {
		return nil
	}
	digest, err := contracts.DigestValue(source.Schema)
	if err != nil {
		return err
	}
	source.Digest = digest
	return nil
}

// normalizeEdges decomposes each edge and sorts the list, so two documents
// that differ only in the order they list edges compile to one digest.
func normalizeEdges(edges []edge) []irEdge {
	out := make([]irEdge, 0, len(edges))
	for _, e := range edges {
		fromNode, outcome, _ := splitEdgeFrom(e.From)
		out = append(out, irEdge{
			From:        e.From,
			FromNode:    fromNode,
			FromOutcome: outcome,
			To:          e.To,
			When:        e.When,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FromNode != out[j].FromNode {
			return out[i].FromNode < out[j].FromNode
		}
		if out[i].FromOutcome != out[j].FromOutcome {
			return out[i].FromOutcome < out[j].FromOutcome
		}
		if out[i].To != out[j].To {
			return out[i].To < out[j].To
		}
		return out[i].When < out[j].When
	})
	return out
}
