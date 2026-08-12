/**
 * Parses a ledger evidence record's `data` payload (opaque `unknown` on
 * `LedgerRecord`, per api/types.ts) for the measured workspace-snapshot
 * fields task t12's worker hook actually appends (internal/runners/dispatch.go
 * `buildEvidence`, proven by internal/worker/hooks_test.go's
 * `TestPostRunWorkspaceSnapshotEvidenceIsAppendedNotAgentDelta`):
 *
 *   - `changed_paths` — string[], only when the runner's own
 *     `changed_paths` observation says it directly compared the workspace.
 *   - `snapshot_digest` — string, alongside `changed_paths`.
 *   - `artifact_refs` — string[], the evidence schema's own named field
 *     (schemas/ledger/evidence.schema.json) for stored content this record
 *     points at (e.g. the diff artifact ref).
 *
 * `diffstat` is NOT one of those fields today — task t10's bridges (the
 * claude-code/codex/colleague adapters' own workspace.py modules) compute a
 * `git diff --stat` string into their own `workspace_measured` block, but
 * that block reaches the actor result, not the ledger: `buildEvidence`
 * never copies it in. Until a
 * producer folds it into evidence data (top-level `diffstat`, or under the
 * evidence schema's own free-form `measurements` object), this parser
 * degrades honestly — it recognizes a `diffstat` string defensively, on
 * either shape, so nothing here has to change the day one does, but no
 * current fixture backed by the real Go/Python shapes will set it.
 *
 * Evidence's `data` schema is deliberately loose (Phase 0 — see the schema's
 * own `$comment`), so this returns `null`, not a thrown error, for anything
 * that isn't shaped like a workspace-snapshot observation: this is
 * additive, shape-driven rendering, never a replacement for the generic
 * evidence line every record still gets.
 */
export interface WorkspaceEvidence {
  changedPaths?: string[];
  snapshotDigest?: string;
  artifactRefs?: string[];
  diffstat?: string;
}

function isStringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((item) => typeof item === "string");
}

function asRecord(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null
    ? (value as Record<string, unknown>)
    : null;
}

export function parseWorkspaceEvidence(data: unknown): WorkspaceEvidence | null {
  const record = asRecord(data);
  if (!record) return null;

  const changedPaths = isStringArray(record.changed_paths)
    ? record.changed_paths
    : undefined;
  const snapshotDigest =
    typeof record.snapshot_digest === "string" ? record.snapshot_digest : undefined;
  const artifactRefs = isStringArray(record.artifact_refs)
    ? record.artifact_refs
    : undefined;

  const measurements = asRecord(record.measurements);
  const diffstatRaw = record.diffstat ?? measurements?.diffstat;
  const diffstat = typeof diffstatRaw === "string" ? diffstatRaw : undefined;

  if (!changedPaths && !snapshotDigest && !artifactRefs && !diffstat) return null;
  return { changedPaths, snapshotDigest, artifactRefs, diffstat };
}
