import type { ReactNode } from "react";
import { useWhoami, type WhoamiState } from "../hooks/useWhoami";

/** Where a person signs in: Cloudflare Access in front of this origin. */
export const SIGN_IN_ORIGIN = "https://nodes.culture.dev";

/** The three-place onboarding recipe (Access policy, actor, binding). */
export const PEOPLE_DOC_URL =
  "https://github.com/agentculture/culture-nodes/blob/main/docs/operations/people.md";

/**
 * The identity states a routed view must not paper over (task t9, spec c8
 * and the onboarding requirement). Neither builds a login form: Cloudflare
 * Access IS the login, and the only thing this page can do about a missing
 * or unbound identity is say so precisely.
 *
 *   - `unbound` replaces the page. An allowlisted person nobody has onboarded
 *     is named — subject and email — and pointed at the recipe. Never a
 *     silent viewer: every write would be refused 403 anyway, and a page that
 *     rendered the inbox with dead buttons would read as a broken inbox.
 *   - `unauthenticated` (a 401 from whoami: no Access identity reached the
 *     control plane) states that sign-in is required and where, ABOVE the
 *     routed view rather than instead of it. Reads stay open on the LAN
 *     listener (spec c9); what a person cannot do there is write, and every
 *     decision surface is disabled for a non-bound state.
 *   - `loading` and `unavailable` render the view as-is. A slow or failed
 *     whoami is not a fact about who is here.
 */
export function IdentityGate({ children }: { children: ReactNode }) {
  const whoami = useWhoami();

  if (whoami.status === "unbound") {
    return (
      <section className="view-rail identity-state" id="identity-unbound" role="alert">
        <h1>No actor is bound to this login</h1>
        <p>
          You are signed in as <strong>{whoami.displayName}</strong> (
          {whoami.principal.provider} subject <code>{whoami.principal.subject}</code>
          ), but no registered actor is bound to that identity, so nothing
          here can be decided, reviewed or replied to as you.
        </p>
        <p>
          Onboarding is three places — the Access allow policy, a registered
          human actor, and the identity binding — and the recipe is{" "}
          <a href={PEOPLE_DOC_URL}>docs/operations/people.md</a>. Ask the
          namespace administrator to bind this subject to your actor.
        </p>
      </section>
    );
  }

  return (
    <>
      {whoami.status === "unauthenticated" ? <SignInRequired /> : null}
      {children}
    </>
  );
}

function SignInRequired() {
  return (
    <section className="view-rail identity-state" id="identity-sign-in" role="alert">
      <h1>Sign in required</h1>
      <p>
        No signed-in identity reached the control plane, so nothing on this
        page can be decided, reviewed or replied to. Sign in through{" "}
        <a href={SIGN_IN_ORIGIN}>{SIGN_IN_ORIGIN}</a> — Cloudflare Access is
        the login; there is nothing to type here.
      </p>
    </section>
  );
}

/**
 * One line naming who a write will be recorded as — "Deciding as …",
 * "Reviewing as …", "Replying as …" — or, for a state that cannot write,
 * why not. Every decision surface renders this where its token panel and
 * free-text identity field used to be, so the accountable actor is always
 * on screen and never editable.
 */
export function SignedInAs({ verb, whoami }: { verb: string; whoami: WhoamiState }) {
  return (
    <p className="identity-line muted" data-identity-status={whoami.status}>
      {identityLine(verb, whoami)}
    </p>
  );
}

function identityLine(verb: string, whoami: WhoamiState): ReactNode {
  switch (whoami.status) {
    case "bound":
      return (
        <>
          {verb} as <strong>{whoami.displayName}</strong> (
          <code>{whoami.actorId}</code>) — the signed-in identity, not a typed
          name.
        </>
      );
    case "unbound":
      return `${whoami.displayName} has no actor bound — nothing can be recorded under this login`;
    case "unauthenticated":
      return "not signed in — nothing can be recorded";
    case "unavailable":
      return "identity unavailable — nothing can be recorded until whoami answers";
    case "loading":
      return "identity…";
  }
}

export default IdentityGate;
