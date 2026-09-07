package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// The cancel endpoint's optional body (plan loop-closure t13, spec c6/c37).
//
// POST /v1alpha1/runs/{id}/cancel was bodiless: the operator pressed it, and
// the human who pressed it is the reason (cancelRunWithReason's own doc). The
// cleanup node is a different caller -- a deterministic code node acting on a
// FACT (the sweep's pr.merged or pr.closed) -- and a cancel it performs must
// say which fact, or its cancellations are indistinguishable from an
// operator's in the event stream and in runs.reason. So the endpoint now
// accepts an optional JSON body {"reason": "..."} and hands it to the
// existing cancelRunWithReason seam, which already writes a non-empty reason
// to runs.reason and into the run.cancelled event.
//
// The allowlist is deliberately short and closed. A free-form reason is a
// label nothing can group by (the same argument preserve.py makes for its
// missing_capability enum), and every value here names a fact the control
// plane can receive, not an opinion about the run.

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
	Reason string `json:"reason"`
}

// readCancelReason decodes the cancel endpoint's optional body. No body, an
// empty body, `{}` and {"reason": ""} all mean "no machine-readable reason"
// -- the operator path, exactly as before -- and return "". A present reason
// must be on the allowlist; anything else, an unknown field included, is a
// 400 so a typo cannot silently become the bodiless cancel.
func readCancelReason(r *http.Request) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxCancelBodyBytes+1))
	if err != nil {
		return "", badRequest("send an optional JSON body {\"reason\": \"...\"} or no body", "read request body: %v", err)
	}
	if len(raw) > maxCancelBodyBytes {
		return "", badRequest("the cancel body is a single short reason", "request body exceeds %d bytes", maxCancelBodyBytes)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var req cancelRunRequest
	if err := dec.Decode(&req); err != nil {
		return "", badRequest("send an optional JSON body {\"reason\": \"...\"} with no other fields, or no body", "decode request body: %v", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", badRequest("send exactly one JSON object", "request body carries trailing content")
	}
	if req.Reason == "" {
		return "", nil
	}
	if _, ok := allowedCancelReasons[req.Reason]; !ok {
		return "", badRequest(
			fmt.Sprintf("reason must be one of %s, or omitted", strings.Join(allowedCancelReasonNames(), ", ")),
			"unknown cancel reason %q", req.Reason)
	}
	return req.Reason, nil
}

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

// cancelDetail is the human-readable half of a reasoned cancel: the event's
// `detail` string names the endpoint AND the fact, so a reader of the
// event stream alone sees why without unpacking runs.reason.
func cancelDetail(reason string) string {
	return "cancelled via POST /v1alpha1/runs/{id}/cancel (reason: " + reason + ")"
}
