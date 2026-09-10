package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The cancel endpoint's optional body (plan loop-closure t13, spec c6/c37).
//
// POST /v1alpha1/runs/{id}/cancel was bodiless: the operator pressed it, and
// the human who pressed it is the reason (cancelRunGuarded's own doc). The
// cleanup node is a different caller -- a deterministic code node acting on a
// FACT (the sweep's pr.merged or pr.closed) -- and a cancel it performs must
// say which fact, or its cancellations are indistinguishable from an
// operator's in the event stream and in runs.reason. So the endpoint now
// accepts an optional JSON body {"reason": "..."} and hands it to the
// cancelRunGuarded seam, which already writes a non-empty reason to
// runs.reason and into the run.cancelled event.
//
// The allowlist is deliberately short and closed. A free-form reason is a
// label nothing can group by (the same argument preserve.py makes for its
// missing_capability enum), and every value here names a fact the control
// plane can receive, not an opinion about the run.
//
// The body carries a second, independent field: an optional `parked_at`
// PRECONDITION. A caller that cancels a run because it observed the run
// parked somewhere read that fact in an earlier request, and between the two
// requests the run can move -- the cleanup node's whole trigger IS a human
// merging the PR, which is the same act that advances the approval node. An
// unconditional cancel would then kill the downstream nodes that approval
// just made live, work the caller never saw and never decided about. So the
// caller names the node it observed, and the cancel transaction re-checks it
// under the same per-run advisory lock the approval advance itself takes
// (engine/humandecision.go) -- the check and the cancel are one transaction,
// so the race has no window. A run that has moved on is a 412, never a
// cancellation: the guard fails CLOSED.

// cancelReasonPRMerged / cancelReasonPRClosed are the two facts a caller may
// cancel a run on. They deliberately share spelling with
// engine.HumanTaskExpiryReasonPRMerged and the pr.closed fact's own reason
// vocabulary, so a reader of runs.reason and human_tasks.expiry_reason sees
// one word for one fact.
const (
	cancelReasonPRMerged = "pr_merged"
	cancelReasonPRClosed = "pr_closed"
)

var allowedCancelReasons = map[string]struct{}{
	cancelReasonPRMerged: {},
	cancelReasonPRClosed: {},
}

// maxCancelBodyBytes bounds the body read: a reason is one short token, and
// an oversized body is a malformed request, not one worth buffering.
const maxCancelBodyBytes = 4 << 10

// cancelRunRequest is the optional body of POST /v1alpha1/runs/{id}/cancel.
type cancelRunRequest struct {
	Reason   string `json:"reason"`
	ParkedAt string `json:"parked_at"`
}

// readCancelRequest decodes the cancel endpoint's optional body. No body, an
// empty body, `{}` and {"reason": ""} all mean "no machine-readable reason"
// -- the operator path, exactly as before -- and return a zero request. A
// present reason must be on the allowlist; anything else, an unknown field
// included, is a 400 so a typo cannot silently become the bodiless cancel.
// An absent or empty `parked_at` means "no precondition", which is likewise
// the pre-t13 behaviour.
func readCancelRequest(r *http.Request) (cancelRunRequest, error) {
	var req cancelRunRequest
	if r.Body == nil {
		return req, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxCancelBodyBytes+1))
	if err != nil {
		return cancelRunRequest{}, badRequest(cancelBodyHint, "read request body: %v", err)
	}
	if len(raw) > maxCancelBodyBytes {
		return cancelRunRequest{}, badRequest("the cancel body is a single short reason and an optional node id",
			"request body exceeds %d bytes", maxCancelBodyBytes)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return cancelRunRequest{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return cancelRunRequest{}, badRequest(cancelBodyHint, "decode request body: %v", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return cancelRunRequest{}, badRequest("send exactly one JSON object", "request body carries trailing content")
	}
	if req.Reason != "" {
		if _, ok := allowedCancelReasons[req.Reason]; !ok {
			return cancelRunRequest{}, badRequest(
				fmt.Sprintf("reason must be one of %s, or omitted", strings.Join(allowedCancelReasonNames(), ", ")),
				"unknown cancel reason %q", req.Reason)
		}
	}
	return req, nil
}

// cancelBodyHint is the one remediation string every malformed-body refusal
// carries, so a caller sees the whole shape of the optional body once.
const cancelBodyHint = "send an optional JSON body {\"reason\": \"...\", \"parked_at\": \"...\"} " +
	"with no other fields, or no body"

// allowedCancelReasonNames lists the allowlist in a stable order for the
// error hint.
func allowedCancelReasonNames() []string {
	names := make([]string, 0, len(allowedCancelReasons))
	for name := range allowedCancelReasons {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// cancelDetail is the human-readable half of a cancel: the event's `detail`
// string names the endpoint and, for a reasoned cancel, the fact too, so a
// reader of the event stream alone sees why without unpacking runs.reason.
// An empty reason is the operator's own bodiless cancel and keeps the pre-t13
// string byte-for-byte -- the human who pressed it is the reason.
func cancelDetail(reason string) string {
	if reason == "" {
		return "cancelled via POST /v1alpha1/runs/{id}/cancel"
	}
	return "cancelled via POST /v1alpha1/runs/{id}/cancel (reason: " + reason + ")"
}

// checkParkedAt enforces the optional `parked_at` precondition inside the
// cancel transaction. An empty parkedAt is no precondition and passes. A
// non-empty one passes only while the named node still has a live node run:
// node_runs.node_key is the column behind NodeRunOut.node_id, and the three
// terminal statuses are the same set cancelRunGuarded's own node-run UPDATE
// excludes, so "still live here" means exactly what the caller read from
// GET /v1alpha1/runs/{id}.
func checkParkedAt(ctx context.Context, tx pgx.Tx, runID, parkedAt string) error {
	if parkedAt == "" {
		return nil
	}
	var live int
	err := tx.QueryRow(ctx, `
		SELECT count(*) FROM node_runs
		WHERE run_id = $1 AND node_key = $2 AND status NOT IN ('completed', 'failed', 'cancelled')`,
		runID, parkedAt).Scan(&live)
	if err != nil {
		return internalError(fmt.Errorf("cancel run: check parked_at: %w", err))
	}
	if live == 0 {
		return preconditionFailed(
			"re-read GET /v1alpha1/runs/{id} and cancel again only if the run is still at that node",
			"run %s is live but has no active node run at node %q", runID, parkedAt)
	}
	return nil
}
