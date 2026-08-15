// Package handover records what a handed-over git ref actually contains, as
// evidence the control plane measured itself (task t10, issue #13).
//
// # The gap this closes
//
// internal/runners/dispatch.go's buildEvidence has always been able to turn a
// runner's Result into an `observed` record. Both shipped runners say, in
// their own package docs, that they cannot answer the questions it would
// record — headspace-cli 0.11.0 has no verb that reports a workspace diff
// (internal/runners/headspace/doc.go), and the Lambda adapter has no
// workspace at all — so `res.Observations.ChangedPaths.Measured` is false on
// every dispatch either can produce. The seam is real and dormant: no
// production run has ever carried an evidence record.
//
// An agent node makes the gap sharper. Its dispatch produces no
// runners.Result at all, only the actor's own §13.2 body: an outcome, an
// output, and a `ledger_delta` of records that PRD §10.4 caps at `proposed`.
// "I changed these files" from an agent is a completion claim. Nothing in the
// control plane had ever checked one.
//
// # What this package does instead
//
// It takes exactly one thing from the agent's report — the NAME of a ref the
// session says it created — and measures everything else for itself: it
// fetches that ref from a remote the CONTROL PLANE is configured with, and
// records the ref, the commit sha the fetch resolved, and the paths that
// commit changed. The agent's own account of any of those three is never
// read, never compared, and never written. A record from here says "I fetched
// this and this is what was in it", which is what makes `observed` honest
// (PRD §10.4: a trusted runner may create observed evidence "only for fields
// they directly measured").
//
// The remote is the control plane's configuration and never the agent's
// report. A handle carries the agent's own idea of where its ref lives, and
// fetching from it would let a session point the measurement at a repository
// it prepared — the measurement would be real and the subject would be
// forged. ValidateRef applies the second half of the same fence: only a ref
// under refs/culture-nodes/ is ever fetched, matching the ref namespace t9
// fences server-side, so a claimed "ref" naming a release branch measures
// nothing.
//
// # No fetchable ref means no record
//
// This is the load-bearing rule, and it is a refusal rather than a
// degradation. If the ref is absent, unfetchable, outside the namespace, or
// never claimed at all, this package appends NOTHING — not a record marked
// unmeasured, not a record with null fields, not a record citing the agent's
// summary. A ledger row that exists says a measurement happened. There is no
// shape of `observed` record that honestly means "I could not look", because
// the thing the record would attest to is the looking. The absence of a row
// is the honest answer, and it is legible: a run with a completion claim and
// no observed evidence beside it is exactly a run whose work nobody checked.
//
// Diagnostics for why a fetch failed go to Observer.OnError, which is
// operator-facing logging — deliberately not the ledger.
//
// # Subprocess boundary
//
// Fetching runs `git` as a subprocess of the control plane, which is worth
// stating against PRD §21's "no arbitrary code executes in the control-plane
// process". Nothing arbitrary executes here: the binary is fixed, the
// argument vector is built from constants plus one validated ref and the
// operator's own configured remote, no shell is involved, every call is
// bounded by a context deadline, and git is run with prompting disabled so a
// credential challenge fails rather than hangs. What §21 refuses is executing
// WORK — a workflow's code, an image, a script — inside the control plane;
// that still goes through the runner boundary and nothing here changes it.
package handover
