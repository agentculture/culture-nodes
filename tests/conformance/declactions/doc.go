// Package declactions is the declaration-engine action conformance suite
// (plan trigger-condition-action t29, issue #328): one contract test per
// action kind, dispatched through the real actor registry on a real,
// migrated PostgreSQL. It lives beside, not inside, the PRD §13 protocol
// kit in tests/conformance, so the adapter workflows that run that kit
// against a live bridge do not start a database for it.
package declactions
