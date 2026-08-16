# Owner decision: add OIDC workload authentication (#6)

Status: proposed decision brief, 2026-08-15.

## Decision requested

Should Culture Nodes add OIDC validation for API callers and actor callbacks
now, including Microsoft Entra ID, or close that scope as not required for the
current private thor/orin deployment?

The present callback credential is not absent: `internal/actors/token.go`
implements an attempt-scoped, expiring HMAC token, binds the attempt ID into
the signed payload, and verifies it with `hmac.Equal`. The separate
`internal/auth` package contains only its package comment
(`internal/auth/doc.go`). OIDC is therefore a new trust and authorization path,
not a small replacement of a stubbed token function. The issue body
(`.owner-issues/issue-6.md`) additionally requires Entra-issued tokens.

## Options and cost

### A — Build OIDC now

Define trusted issuers, audiences, subject-to-actor/operator mapping, JWKS
retrieval and caching, key rotation, clock-skew handling, and denial behaviour;
apply that policy separately to API callers and attempt callbacks; support and
test Entra discovery and claims; document coexistence or cutover from HMAC.

Engineering cost and ongoing identity-provider cost are **unknown**. The
cheapest experiment that would turn the engineering unknown into an estimate
is a throwaway vertical slice that validates one Entra client-credentials token
against a single protected test endpoint, including cached JWKS and a rotated
signing key, then records changed packages and elapsed engineering time. It
must not be treated as production authentication until issuer/audience policy,
authorization mapping, failure tests, and operational key-rotation behaviour
are designed.

### B — Do not build OIDC for this deployment (recommended)

Keep the existing short-lived HMAC attempt-token path and the deployment's
private-network posture. Cost: no implementation spend; continued secret
distribution and rotation remain operational work. No dollar figure for that
work is recorded in the repository, so this brief does not invent one.

## Dependencies

Option A needs an owner-selected issuer set and tenant policy, registered
workload identities, exact audiences, a subject-to-Culture-Nodes authority
model, network access to discovery/JWKS endpoints, and a decision on whether a
new JWT/JWK dependency is compatible with the zero-third-party-dependency
runtime rule. The existing API description says most of the phase-1 API is
authless behind a private network (`api/openapi/openapi.yaml`), so API-wide
OIDC also depends on deciding which operations and human roles it protects.

Option B depends on keeping the API private and retaining the callback secret
configuration used by `deploy/prod/compose.thor.yml`.

## Consequence of “no”

**No means close #6 as won't-do for the current private thor/orin deployment;
OIDC is not backlog. A future cloud lane or requirement to expose the API
outside the private network must open a new, narrower issue naming its issuer,
audience, and protected operations.**
