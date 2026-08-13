/**
 * Reads a ledger success_signal record's `data` payload (opaque `unknown` on
 * `LedgerRecord`, per api/types.ts) for the one question a render surface
 * has to answer about it: is this signal machine-checkable at all?
 *
 * The schema (schemas/ledger/success_signal.schema.json) is explicit about
 * what the `mechanical` flag means: "False means the signal is not yet
 * machine-checkable, and says so instead of implying it is." The evaluator
 * (internal/worker/successsignal.go, task t18) honors that by appending a
 * derived evaluation record only for mechanical:true signals — a
 * mechanical:false signal gets none, ever. So wherever success signals
 * render, a non-mechanical one must say "not machine-checkable" rather than
 * sit indistinguishable from a signal whose verdict merely hasn't landed
 * yet.
 *
 * Only `mechanical === true` counts as machine-checkable here: the payload
 * shape is deliberately loose (Phase 0), and a signal that omits the flag —
 * or carries something that isn't a boolean — has not stated that a machine
 * can check it, which is the same honest default the Go evaluator applies.
 */

/** The label render surfaces show for a signal no machine will evaluate. */
export const NOT_MACHINE_CHECKABLE = "not machine-checkable";

export function isMachineCheckableSignal(data: unknown): boolean {
  if (typeof data !== "object" || data === null) return false;
  return (data as Record<string, unknown>).mechanical === true;
}
