import type { WhoamiBound, WhoamiUnbound } from "../api/types";

/**
 * `GET /v1alpha1/whoami` answers (task t9, spec c8/c9): the browser's only
 * source of identity. The shape is internal/api/whoami.go's — a bound
 * principal carries the actor the Access identity maps to, an unbound one
 * carries only the principal, and a request with no principal at all is a
 * 401 (not a body, so there is no fixture for it).
 */
export const WHOAMI_ACTOR_ID = "actor-human-ori";
export const WHOAMI_EMAIL = "ori@example.test";
export const WHOAMI_SUBJECT = "cf-sub-0001";

export const WHOAMI_BOUND: WhoamiBound = {
  principal: {
    provider: "cloudflare-access",
    subject: WHOAMI_SUBJECT,
    email: WHOAMI_EMAIL,
  },
  actor_id: WHOAMI_ACTOR_ID,
  roles: ["approver"],
};

/** A service token has no email; its display name is the common_name. */
export const WHOAMI_SERVICE_TOKEN: WhoamiBound = {
  principal: {
    provider: "cloudflare-service-token",
    subject: "ops-cli",
    common_name: "ops-cli",
  },
  actor_id: "actor-human-ops",
  roles: ["approver"],
};

export const WHOAMI_UNBOUND_EMAIL = "newcomer@example.test";
export const WHOAMI_UNBOUND_SUBJECT = "cf-sub-0002";

export const WHOAMI_UNBOUND: WhoamiUnbound = {
  unbound: true,
  principal: {
    provider: "cloudflare-access",
    subject: WHOAMI_UNBOUND_SUBJECT,
    email: WHOAMI_UNBOUND_EMAIL,
  },
};
