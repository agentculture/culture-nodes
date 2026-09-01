/**
 * The ticket key a run was started for, dug out of the run's own input.
 *
 * There is no ticket listing endpoint and no ticket column on `runs` — the
 * only place the key exists outside the ticket projection is the input the
 * run was created with. The Decisions view has read it that way since task
 * t12; task t17's first-visit page needs the same answer, so the reader
 * moved here rather than being written a second time. Two independent
 * spellings of "which keys count as a ticket key" would drift, and a page
 * that silently stopped linking to tickets is invisible until someone
 * notices they cannot get anywhere.
 *
 * It reads only these three key names, and returns null rather than
 * guessing: a run whose input names its ticket some other way is honestly
 * "no ticket recorded", not a fabricated link to a page that 404s.
 */
const TICKET_KEYS = ["ticket_key", "issue_key", "jira_key"];

export function findTicketKey(input: unknown): string | null {
  if (!input || typeof input !== "object") return null;
  for (const [key, value] of Object.entries(input)) {
    if (TICKET_KEYS.includes(key) && typeof value === "string") return value;
    const nested = findTicketKey(value);
    if (nested) return nested;
  }
  return null;
}
