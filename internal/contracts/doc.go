// Package contracts validates workflow, ledger, and runner documents against
// the JSON Schema Draft 2020-12 definitions embedded from schemas/, and turns
// a document into the canonical bytes everything else content-addresses.
//
// # Why canonical form exists
//
// A published workflow version is immutable and content-addressed, a run pins
// one workflow digest, and every ledger record carries the digest of its own
// content. None of that survives if two spellings of the same document hash
// differently. CanonicalJSON therefore defines one byte sequence per logical
// document, and Digest turns it into a "sha256:<hex>" identifier.
//
// # Canonicalization rules
//
// CanonicalJSON marshals the value with encoding/json (HTML escaping off),
// re-reads it with json.Number in force, and re-emits it under these rules:
//
//  1. Object keys are sorted lexicographically by Go string comparison — that
//     is, by UTF-8 byte order, which for keys equals Unicode code-point order.
//     This differs from RFC 8785 (JCS), which sorts by UTF-16 code units; the
//     two orders agree on every key below U+10000 and so on every identifier
//     these schemas allow. The deviation is deliberate: sorting the bytes we
//     actually store beats re-deriving UTF-16 indices we never use.
//  2. No insignificant whitespace. No spaces after ':' or ',', no indentation,
//     no trailing newline.
//  3. Strings are emitted as UTF-8 with encoding/json's escaping and HTML
//     escaping disabled, so '<', '>' and '&' stay literal while '"', '\\' and
//     control characters are escaped. \uXXXX escapes in the input are folded
//     to the characters they denote. Input must be valid UTF-8; invalid bytes
//     become U+FFFD exactly as encoding/json substitutes them.
//  4. Numbers keep the literal spelling encoding/json produced. A number that
//     arrived as JSON text survives verbatim — 1.0 stays 1.0, 1e3 stays 1e3,
//     and a 30-digit integer is not pushed through float64 — while a Go value
//     is spelled by encoding/json's defaults. Canonicalization never re-spells
//     a number: this normalizes *documents*, not arithmetic, and a producer
//     that changes how it prints a number has changed the document it signed.
//  5. Duplicate object keys resolve to the last occurrence, encoding/json's
//     behaviour. Documents reaching this function have already been accepted
//     as JSON; deduplication is not a validation step.
//
// The rules are stable API. Changing any of them changes every digest this
// system has ever issued, so a change is a versioned migration, not a fix.
//
// # Validation
//
// Validator compiles every embedded schema at construction, so a malformed
// schema or a dangling $ref fails at startup rather than at first use. A
// rejection returns *ValidationError, whose violations each name the offending
// value with a JSON Pointer — including the synthesized pointer to a missing
// required property or an unexpected extra one, which the underlying library
// reports against the enclosing object. Telling a caller only that a document
// is invalid is not a diagnostic; telling it which field is.
//
// This package deliberately stops at syntax and structure (PRD §11.4). Graph
// reachability, contract compatibility, binding resolution, digest pinning,
// and policy are compiler and runtime concerns. Schema validity is also never
// authority: whether an actor is allowed to write a record with a given
// authority value is enforced by the ledger runtime against an authenticated
// identity, which no document can prove about itself.
package contracts
