import type { Attempt } from "../api/types";

/**
 * Task t26 (issue #49, spec claim c32 / honesty h21): what the run detail
 * page shows for one attempt's bridge-reported preserve-on-failure branch
 * (task t25 mints and commits it, migrations/0025 persists it).
 *
 * `href` is set ONLY when the branch reached the configured remote AND the
 * operator configured a forge URL template to build a link from
 * (`VITE_PRESERVE_BRANCH_URL_TEMPLATE`, `{branch}` substituted) — a link is
 * never guessed from `remote` alone. A local-only branch is NEVER linked:
 * it exists on the bridge host and nowhere else, so a link would claim a
 * forge page exists when it does not. This is the honest-surfacing rule
 * task t26's brief states explicitly: a reader must be able to tell "go
 * look at this on the remote" from "this exists only on the machine that
 * ran it".
 */
export interface PreserveBranchInfo {
  branch: string;
  pushed: boolean;
  remote?: string;
  href?: string;
}

/**
 * Derives what to show for one attempt's preserve branch, or `null` when
 * the attempt reported none — the overwhelming common case, since a bridge
 * only preserves on a genuine technical failure that left workspace
 * changes behind (task t25's own gate). Pure and React-free so it is
 * testable on its own and reusable by any surface that lists attempts.
 *
 * `forgeUrlTemplate` is read by the caller from
 * `import.meta.env.VITE_PRESERVE_BRANCH_URL_TEMPLATE` (see web/README.md)
 * and passed in explicitly — this function never reads environment state
 * itself, so it stays a pure function of its arguments.
 */
export function preserveBranchInfo(
  attempt: Pick<Attempt, "preserve_branch" | "preserve_pushed" | "preserve_remote">,
  forgeUrlTemplate?: string,
): PreserveBranchInfo | null {
  const branch = attempt.preserve_branch;
  if (!branch) return null;

  const pushed = attempt.preserve_pushed === true;
  const info: PreserveBranchInfo = { branch, pushed };
  if (attempt.preserve_remote) info.remote = attempt.preserve_remote;

  // Never constructed for a local-only branch, and never constructed from
  // `remote` alone without an operator-set template — see this module's
  // own doc comment.
  if (pushed && forgeUrlTemplate) {
    info.href = forgeUrlTemplate.replace("{branch}", encodeURIComponent(branch));
  }
  return info;
}
