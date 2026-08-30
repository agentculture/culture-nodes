/**
 * The browser's half of the human-decision bearer token (plan risk r1,
 * settled here in task t14).
 *
 * The control plane authenticates `POST /v1alpha1/human-tasks/{id}/decision`
 * against ONE deployment-shared secret (`NODES_HUMAN_DECISION_TOKEN_SECRET`,
 * internal/api/humantasks.go's requireDecisionAuth) — there is no per-user
 * identity behind the token in this phase; accountability is carried
 * separately by the required `decider_actor_id` on every decision. The UI
 * therefore treats the token as a credential the operator *presents*, not
 * one it provisions:
 *
 *   - entered by the user in the inbox, never baked into the bundle;
 *   - held in `sessionStorage` ONLY — scoped to this tab and gone when the
 *     tab closes. Never `localStorage` (outlives the sitting and leaks
 *     across every tab of the origin) and never a cookie (would ride along
 *     on every request automatically and reintroduce a CSRF surface the
 *     explicit Authorization header deliberately avoids);
 *   - always visible: the inbox shows a held/absent indicator and a
 *     clear-token affordance, so walking away from a shared machine has a
 *     one-click remedy on top of the tab-close default.
 *
 * No token, no mutation: the decision form cannot submit without one, and
 * nothing else in this client ever attaches it.
 */

const KEY = "nodes.human-decision-token";

/** The held token, or null. sessionStorage may be unavailable (jsdom edge
 * cases, privacy modes that throw) — treat that as "no token held". */
export function getDecisionToken(): string | null {
  try {
    return window.sessionStorage.getItem(KEY);
  } catch {
    return null;
  }
}

export function setDecisionToken(token: string): void {
  try {
    window.sessionStorage.setItem(KEY, token);
  } catch {
    /* nothing to do: the indicator will keep reporting "no token" */
  }
}

export function clearDecisionToken(): void {
  try {
    window.sessionStorage.removeItem(KEY);
  } catch {
    /* already effectively cleared */
  }
}

const ACTOR_KEY = "nodes.human-decision-actor-id";

export function getDecisionActorID(): string {
  try {
    return window.localStorage.getItem(ACTOR_KEY) ?? "";
  } catch {
    return "";
  }
}

export function setDecisionActorID(actorID: string): void {
  try {
    window.localStorage.setItem(ACTOR_KEY, actorID);
  } catch {
    /* persistence is best-effort; the controlled input still works */
  }
}
