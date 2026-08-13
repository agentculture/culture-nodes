import { parse as parseYaml } from "yaml";
import type { WorkflowIR } from "../api/types";

/**
 * Best-effort, client-side parse of authored workflow source into the same
 * shape `domain/graph.ts`'s `parseWorkflowGraph` reads (`WorkflowIR`).
 *
 * This is deliberately **not** the server's normalized IR: schema refs are
 * not resolved to digests, no default is filled in, and nothing here is
 * validated against the compiler's rules. It exists only to drive the t9
 * read-only graph preview (ADR 0007) once the server has already confirmed
 * the document compiles (`WorkflowValidation.valid === true`) — the authored
 * `spec.entry`/`spec.nodes`/`spec.edges` topology already matches
 * `WorkflowIR`'s shape one-for-one (compare `examples/self-hosting-loop`'s
 * authored YAML with `schemas/examples/deliver-change.workflow.json`'s
 * normalized form: same node kinds, `uses`, `outcomes`/`contract.outcomes`,
 * and `from`/`to` edges), so parsing it directly for *preview topology* is
 * honest. The result is never sent back to the server — publish always ships
 * the operator's original source bytes verbatim, never this parsed value.
 *
 * Returns `null` when the source cannot be parsed into that shape at all.
 * That should not happen once the server has said `valid: true`, but a UI
 * must not throw on a document it did not itself validate.
 */
export function parseWorkflowSourceForPreview(
  source: string,
  format: "yaml" | "json",
): WorkflowIR | null {
  try {
    const doc: unknown =
      format === "json" ? JSON.parse(source) : parseYaml(source);
    if (!doc || typeof doc !== "object") return null;

    const spec = (doc as { spec?: unknown }).spec as
      | { entry?: unknown; nodes?: unknown; edges?: unknown }
      | undefined;
    if (
      !spec ||
      typeof spec.entry !== "string" ||
      typeof spec.nodes !== "object" ||
      spec.nodes === null ||
      !Array.isArray(spec.edges)
    ) {
      return null;
    }

    return doc as WorkflowIR;
  } catch {
    return null;
  }
}

/** Infer the source format from an uploaded file's name; yaml is the default. */
export function formatFromFilename(name: string): "yaml" | "json" {
  return /\.json$/i.test(name) ? "json" : "yaml";
}
