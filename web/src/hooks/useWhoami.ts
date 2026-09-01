import { useEffect, useSyncExternalStore } from "react";
import { ApiError, getWhoami } from "../api/client";
import type { WhoamiPrincipal } from "../api/types";

/**
 * The browser's identity model (task t9, spec c8/c9).
 *
 * Identity is DERIVED, never typed: the Cloudflare edge cookie carries the
 * verified login on every same-origin request, and `GET /v1alpha1/whoami`
 * says which registered actor that login is bound to. This hook reads it
 * ONCE per page session and shares the answer with every component that
 * asks — the header's readout, the gate that replaces the page for an
 * unbound login, and each decision surface that needs the actor id to name
 * as decider, reviewer or replier.
 *
 * What replaced what: the shared bearer the operator pasted into three token
 * panels (held per tab) and the free-text actor id they typed and the page
 * remembered (held per origin) are both gone. Nothing here touches web
 * storage and nothing here can be typed into; a state that is not `bound`
 * disables every write, and the control plane refuses them regardless.
 *
 * The five states are deliberately distinct so no view can mistake one for
 * another: `unbound` is an allowlisted person nobody has onboarded yet (a
 * named, full-page fact — never a silent viewer); `unauthenticated` is a
 * 401, i.e. no Access identity reached the control plane at all (the LAN
 * listener, or the tunnel misconfigured); `unavailable` is any other failure
 * — the control plane is down or the route is older than t8 — which is not
 * a statement about who is here and must not render as "signed out".
 */
export type WhoamiState =
  | { status: "loading" }
  | {
      status: "bound";
      principal: WhoamiPrincipal;
      actorId: string;
      roles: string[];
      displayName: string;
    }
  | { status: "unbound"; principal: WhoamiPrincipal; displayName: string }
  | { status: "unauthenticated" }
  | { status: "unavailable"; error: ApiError };

/**
 * What to call the person: the email for an Access login, the common_name for
 * a service token, and the opaque subject when neither is present. Display
 * only — the binding key is provider + subject and lives server-side.
 */
export function principalDisplayName(principal: WhoamiPrincipal): string {
  return principal.email || principal.common_name || principal.subject;
}

const LOADING: WhoamiState = { status: "loading" };

let state: WhoamiState = LOADING;
let started = false;
const listeners = new Set<() => void>();

function publish(next: WhoamiState) {
  state = next;
  for (const listener of listeners) listener();
}

function start() {
  if (started) return;
  started = true;
  getWhoami()
    .then((whoami) => {
      if (whoami.unbound === true) {
        publish({
          status: "unbound",
          principal: whoami.principal,
          displayName: principalDisplayName(whoami.principal),
        });
        return;
      }
      publish({
        status: "bound",
        principal: whoami.principal,
        actorId: whoami.actor_id,
        roles: whoami.roles ?? [],
        displayName: principalDisplayName(whoami.principal),
      });
    })
    .catch((cause: unknown) => {
      const error =
        cause instanceof ApiError
          ? cause
          : new ApiError(0, String(cause), "check the browser console");
      publish(
        error.status === 401
          ? { status: "unauthenticated" }
          : { status: "unavailable", error },
      );
    });
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function snapshot(): WhoamiState {
  return state;
}

/** The signed-in principal, read once per session and shared. */
export function useWhoami(): WhoamiState {
  const current = useSyncExternalStore(subscribe, snapshot, snapshot);
  useEffect(() => {
    start();
  }, []);
  return current;
}

/** Forget the session's answer so the next mount fetches again (tests only). */
export function resetWhoamiForTests(): void {
  state = LOADING;
  started = false;
  listeners.clear();
}
