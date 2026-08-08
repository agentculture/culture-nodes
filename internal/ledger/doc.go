// Package ledger implements the agent-native work ledger described in
// docs/initial-design/culture-nodes-prd-spec.md §10: an append-only stream of
// typed records whose authority, provenance, and review state are explicit.
//
// # What the ledger is for
//
// The runtime event log answers what the orchestrator did. The work ledger
// answers what the work currently means and what supports it (§10.1). It is
// structured project state, not a transcript: announcements, claims,
// assumptions, questions, tasks, decisions, success signals, evidence,
// results, and reviews.
//
// # Immutability and supersession
//
// Records are never updated or deleted. A correction appends a new record
// whose Supersedes names the record it replaces (§10.3). The PostgreSQL
// store enforces this in the database with a trigger, not merely by
// convention; this package never issues an UPDATE against a record.
//
// A record is superseded when some other record names it in Supersedes.
// Superseded records are excluded from every projection — see Live.
//
// # Authority is enforced at append, against the producer
//
// Authority is constrained by who produced the record (§10.4), and that
// constraint is the point of this package:
//
//   - an agent may only ever create proposed records;
//   - a human may create proposed records directly, and confirmed or
//     rejected records only inside a review transaction (CommitReview);
//   - a trusted runner may create only observed evidence, and only for the
//     fields its manifest declares it directly measured;
//   - a deterministic engine or validator may create only derived records;
//   - superseded is never an appendable authority: replacement is expressed
//     by the replacing record's Supersedes pointer, not by writing a record
//     that declares itself replaced;
//   - no actor promotes its own proposal by changing a field.
//
// A violation returns *AuthorityError, which names the rule that refused.
//
// CommitReview never rewrites the record under review. It appends new review
// records that carry the confirmed or rejected authority and reference the
// target. An agent's proposal therefore stays proposed forever; what changes
// is that a human-origin review record now points at it. That is why
// "an agent-origin record never becomes confirmed without an authorized
// review" (§22) is structural here rather than merely tested: the flag that
// permits confirmed/rejected authority is unexported and set only by
// CommitReview.
//
// # What this package cannot prove
//
// Authority enforcement is only as good as the identity handed to it. This
// package takes Origin — and the reviewer bound to a review request — on
// trust from an authenticated caller. Verifying that an actor id really
// belongs to a human reviewer, or that a runner manifest was issued to the
// runner presenting it, is the API and policy layer's job. The trust
// boundary is stated here rather than implied: a document cannot prove
// anything about its own producer.
//
// # Ledger version
//
// The ledger version of a run is the number of records appended to that run.
// Because the record table is append-only, that count only ever increases,
// and it increases by exactly one per append — which makes it a usable
// optimistic-concurrency token for review transactions (§10.8) without a
// separate sequence column. A review request records the version it read; a
// commit that finds a different current version is stale and applies
// nothing.
//
// # Projections
//
// Projections (§10.9) are pure functions over a record slice. Each sorts its
// inputs by record id before emitting, so the same set of records produces
// the same projection — and therefore the same digest — regardless of the
// order the store returned them in.
package ledger
