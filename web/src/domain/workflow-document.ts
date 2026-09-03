import {
  isAlias,
  isCollection,
  isMap,
  isPair,
  isScalar,
  isSeq,
  parseDocument,
  type Document,
  type Node,
  type Pair,
  type Scalar,
  type YAMLMap,
  type YAMLSeq,
} from "yaml";

/**
 * Surgical mutations on the *author's own* workflow document (task t14).
 *
 * The authoring surface must never hand an operator back a re-formatted file.
 * A workflow YAML carries comments explaining why a node exists, blank lines
 * grouping blocks, anchors, and a key order the author chose — none of which
 * survive a `parse → mutate the plain object → stringify` round-trip. This
 * module keeps the parsed *document* (yaml's CST-backed `Document`, which
 * stores comments and spacing on the nodes) and mutates that, so an insert
 * touches the inserted lines and nothing else.
 *
 * The stringify options are the ones the t12 spike measured
 * (`docs/decisions/2026-09-03-design-canvas-spike.md`, verdict go): they were
 * byte-identical on `deploy/compose/testdata/smoke.workflow.yaml` and both
 * authoring fixtures, and `flowCollectionPadding: false` is what matches this
 * repo's committed style (`outcomes: [completed]`, `propose: [claim, result]`).
 *
 * Two limits are real and are not papered over:
 *
 * - **Padded flow collections normalize.** Flow padding is a global stringify
 *   option, not a per-node fact the parser records, so a document written
 *   `[ completed ]` comes back `[completed]`. Every committed workflow in this
 *   repo is unpadded, which is why the spike's option set is byte-identical
 *   there; a padded document is the one round-trip that is not.
 * - **A merge key is refused, not guessed.** If any map on the path from the
 *   document root to the mutation site holds `<<`, the map's effective keys
 *   are not the text the author is looking at — an insert could collide with
 *   a merged-in key, and an in-place edit could shadow one. The mutation is
 *   refused with a reason and the document is left exactly as parsed, so
 *   `toString()` still returns the original bytes.
 *
 * JSON sources take the same API. JSON is parsed by `parseDocument` too (it is
 * a YAML subset) but re-emitted by this module's own JSON writer rather than
 * the YAML stringifier, so a JSON file stays JSON. The writer preserves key
 * order and each scalar's original token, and lays the document out the way
 * `JSON.stringify(value, null, indent)` does at the indent detected from the
 * source — the canonical form the control plane and every editor already emit.
 */

/** The t12 spike's measured stringify options. Exported so a test can pin them. */
export const WORKFLOW_STRINGIFY_OPTIONS = {
  flowCollectionPadding: false,
  lineWidth: 0,
  indentSeq: true,
} as const;

export type WorkflowSourceFormat = "yaml" | "json";

/**
 * A mutation either happened or was refused with a reason the UI can show.
 * There is no third state: a refusal leaves the document untouched.
 */
export type MutationResult = { ok: true } | { ok: false; reason: string };

export type OpenResult =
  | { ok: true; doc: WorkflowDocument }
  | { ok: false; reason: string };

/** The `spec.edges` item shape this repo's workflows use. */
export type EdgeInput = { from: string; to: string } & Record<string, unknown>;

const NODES_PATH = ["spec", "nodes"] as const;
const EDGES_PATH = ["spec", "edges"] as const;

const refuse = (reason: string): MutationResult => ({ ok: false, reason });
const done: MutationResult = { ok: true };

/** `spec.nodes.greet`, or "the document root" for the empty path. */
function describePath(path: readonly string[]): string {
  return path.length === 0 ? "the document root" : path.join(".");
}

function keyText(key: unknown): string | undefined {
  if (isScalar(key)) return typeof key.value === "string" ? key.value : undefined;
  return typeof key === "string" ? key : undefined;
}

/**
 * True for a YAML merge pair (`<<: *anchor`). Parsed with default options a
 * merge key is just a plain `<<` scalar key; with `merge: true` yaml resolves
 * it into its own pair whose key only carries the source token. Both are
 * checked so the guard does not depend on how the document was parsed.
 */
function isMergeKeyPair(item: unknown): boolean {
  if (!isPair(item)) return false;
  const key: unknown = item.key;
  if (isScalar(key)) return key.value === "<<" || key.source === "<<";
  return key === "<<";
}

/**
 * Style a freshly created node the way its new neighbours are already written,
 * so an insert does not switch the file between flow and block style halfway
 * down. A sequence of plain scalars is written flow (`outcomes: [completed]`)
 * because that is how every committed workflow in this repo writes one.
 */
function styleCreatedNode(node: unknown, flow: boolean): void {
  if (isSeq(node)) {
    node.flow = flow || node.items.every((item) => isScalar(item));
    for (const item of node.items) styleCreatedNode(item, node.flow);
    return;
  }
  if (isMap(node)) {
    node.flow = flow;
    for (const item of node.items) {
      if (isPair(item)) styleCreatedNode(item.value, flow);
    }
  }
}

/** Whether a new sibling should be flow-styled, judged from the last existing one. */
function siblingFlow(container: YAMLMap | YAMLSeq): boolean {
  const items = container.items;
  for (let i = items.length - 1; i >= 0; i--) {
    const item = items[i];
    const candidate: unknown = isPair(item) ? item.value : item;
    if (isCollection(candidate)) return candidate.flow === true;
  }
  return container.flow === true;
}

/** Move an author's comments and blank line onto the node replacing theirs. */
function carryComments(from: unknown, to: Node): void {
  if (!isScalar(from) && !isCollection(from)) return;
  if (from.comment != null) to.comment = from.comment;
  if (from.commentBefore != null) to.commentBefore = from.commentBefore;
  if (from.spaceBefore) to.spaceBefore = true;
}

/** Indent of the first indented line, so a re-emitted JSON keeps the author's. */
function detectJsonIndent(source: string): string {
  return /\n([ \t]+)\S/.exec(source)?.[1] ?? "  ";
}

function jsonKey(key: unknown): string {
  const text = keyText(key);
  if (text !== undefined) return JSON.stringify(text);
  return JSON.stringify(String(isScalar(key) ? key.value : key));
}

function jsonToken(node: Scalar): string {
  const { source, value } = node;
  if (typeof source === "string" && source.length > 0) {
    try {
      if (JSON.parse(source) === value) return source;
    } catch {
      // fall through to a freshly written token
    }
  }
  return JSON.stringify(value ?? null);
}

/** Write the document back as JSON, preserving key order and scalar tokens. */
function emitJson(node: unknown, indent: string, depth = 0): string {
  const pad = indent.repeat(depth);
  const inner = indent.repeat(depth + 1);
  if (isMap(node)) {
    const pairs = node.items.filter(isPair);
    if (pairs.length === 0) return "{}";
    const body = pairs
      .map(
        (pair) =>
          `${inner}${jsonKey(pair.key)}: ` +
          emitJson(pair.value, indent, depth + 1),
      )
      .join(",\n");
    return `{\n${body}\n${pad}}`;
  }
  if (isSeq(node)) {
    if (node.items.length === 0) return "[]";
    const body = node.items
      .map((item) => inner + emitJson(item, indent, depth + 1))
      .join(",\n");
    return `[\n${body}\n${pad}]`;
  }
  if (isScalar(node)) return jsonToken(node);
  return JSON.stringify(node ?? null);
}

/**
 * An author's workflow document, open for surgical edits.
 *
 * Every mutation reports whether it happened. A refusal is a fact about the
 * document (a missing collection, a duplicate id, a merge key at the site),
 * never a silent no-op, and never a partially applied edit.
 */
export class WorkflowDocument {
  private constructor(
    private readonly doc: Document,
    readonly format: WorkflowSourceFormat,
    private readonly jsonIndent: string,
    private readonly jsonTrailingNewline: boolean,
  ) {}

  /**
   * Parse `source`. Refuses rather than throws, because a UI opening a file it
   * did not itself write must not blow up on it.
   */
  static open(source: string, format: WorkflowSourceFormat): OpenResult {
    if (format === "json") {
      try {
        JSON.parse(source);
      } catch (err) {
        return {
          ok: false,
          reason: `not valid JSON: ${err instanceof Error ? err.message : String(err)}`,
        };
      }
    }
    const doc = parseDocument(source);
    if (doc.errors.length > 0) {
      return { ok: false, reason: doc.errors[0].message };
    }
    if (!isMap(doc.contents)) {
      return { ok: false, reason: "the document root is not a mapping" };
    }
    return {
      ok: true,
      doc: new WorkflowDocument(
        doc,
        format,
        detectJsonIndent(source),
        source.endsWith("\n"),
      ),
    };
  }

  /**
   * Refuse if anything on the path from the root to `path` would make a text
   * edit dishonest: a merge key (the map's effective keys are not its text) or
   * an alias (the text to edit lives at the anchor, not here).
   */
  private guardPath(path: readonly string[]): string | null {
    let node: unknown = this.doc.contents;
    const walked: string[] = [];
    for (let depth = 0; ; depth++) {
      if (isAlias(node)) {
        return `${describePath(walked)} is a YAML alias; edit the anchored value it points at instead`;
      }
      if (isMap(node) && node.items.some(isMergeKeyPair)) {
        return `${describePath(walked)} holds a YAML merge key (<<), so its effective keys are not the text in the file; edit the anchored map instead`;
      }
      if (depth === path.length) return null;
      if (!isMap(node)) {
        return `${describePath(walked)} is not a mapping`;
      }
      node = node.get(path[depth], true);
      walked.push(path[depth]);
      if (node === undefined) {
        return `${describePath(walked)} is not present in the document`;
      }
    }
  }

  /** The `spec.nodes` map, or the reason a mutation there must be refused. */
  private nodesMap(): YAMLMap | string {
    const reason = this.guardPath(NODES_PATH);
    if (reason) return reason;
    const map = this.doc.getIn(NODES_PATH, true);
    if (!isMap(map)) return "spec.nodes is not a mapping";
    return map;
  }

  /** The `spec.edges` sequence, or the reason a mutation there must be refused. */
  private edgesSeq(): YAMLSeq | string {
    const reason = this.guardPath(EDGES_PATH);
    if (reason) return reason;
    const seq = this.doc.getIn(EDGES_PATH, true);
    if (!isSeq(seq)) return "spec.edges is not a sequence";
    return seq;
  }

  /** Declare a new node under `spec.nodes`. */
  addNode(id: string, node: Record<string, unknown>): MutationResult {
    const nodes = this.nodesMap();
    if (typeof nodes === "string") return refuse(nodes);
    if (nodes.has(id)) {
      return refuse(`node "${id}" is already declared in spec.nodes`);
    }
    const value = this.doc.createNode(node);
    styleCreatedNode(value, siblingFlow(nodes));
    nodes.set(this.doc.createNode(id), value);
    return done;
  }

  /**
   * Append an edge to `spec.edges`. An edge with the same `from` and `to` is
   * refused rather than appended twice: `removeEdge` identifies an edge by
   * that pair, so a duplicate would be a line no caller could point back at.
   */
  addEdge(edge: EdgeInput): MutationResult {
    const edges = this.edgesSeq();
    if (typeof edges === "string") return refuse(edges);
    if (this.findEdge(edges, edge.from, edge.to) >= 0) {
      return refuse(
        `spec.edges already has an edge from "${edge.from}" to "${edge.to}"`,
      );
    }
    const value = this.doc.createNode(edge);
    styleCreatedNode(value, siblingFlow(edges));
    edges.add(value);
    return done;
  }

  /**
   * Set one property on one node, in place when the key is already there (so a
   * trailing comment on that line survives) and appended to the node's block
   * when it is not.
   */
  setNodeProp(id: string, key: string, value: unknown): MutationResult {
    const nodeMap = this.nodeMapFor(id);
    if (typeof nodeMap === "string") return refuse(nodeMap);
    const next = this.doc.createNode(value);
    const existing = nodeMap.items.find(
      (item): item is Pair => isPair(item) && keyText(item.key) === key,
    );
    if (!existing) {
      styleCreatedNode(next, nodeMap.flow === true);
      nodeMap.set(this.doc.createNode(key), next);
      return done;
    }
    const previous: unknown = existing.value;
    if (isScalar(previous) && isScalar(next)) {
      // Mutate the parsed scalar rather than replace it: its comment, its
      // anchor and its quoting style all live on that node. `source` is the
      // original token and would be re-emitted verbatim, so it has to go.
      previous.value = next.value;
      previous.source = undefined;
      return done;
    }
    styleCreatedNode(
      next,
      isCollection(previous) ? previous.flow === true : nodeMap.flow === true,
    );
    carryComments(previous, next);
    existing.value = next;
    return done;
  }

  /**
   * Delete a node's whole block. Edges that referenced it are deliberately left
   * alone — removing them would edit lines the operator did not point at, and
   * the compiler reports the dangling reference precisely.
   */
  removeNode(id: string): MutationResult {
    const nodeMap = this.nodeMapFor(id);
    if (typeof nodeMap === "string") return refuse(nodeMap);
    const nodes = this.nodesMap();
    if (typeof nodes === "string") return refuse(nodes);
    nodes.delete(id);
    return done;
  }

  /** Delete the first `spec.edges` item matching `from` and `to`. */
  removeEdge(from: string, to: string): MutationResult {
    const edges = this.edgesSeq();
    if (typeof edges === "string") return refuse(edges);
    const index = this.findEdge(edges, from, to);
    if (index < 0) {
      return refuse(`spec.edges has no edge from "${from}" to "${to}"`);
    }
    const item = edges.items[index];
    if (isMap(item) && item.items.some(isMergeKeyPair)) {
      return refuse(
        `spec.edges[${index}] holds a YAML merge key (<<), so its effective keys are not the text in the file; edit the anchored map instead`,
      );
    }
    edges.delete(index);
    return done;
  }

  /** The document's current bytes, in the format it was opened as. */
  toString(): string {
    if (this.format === "json") {
      return (
        emitJson(this.doc.contents, this.jsonIndent) +
        (this.jsonTrailingNewline ? "\n" : "")
      );
    }
    return this.doc.toString(WORKFLOW_STRINGIFY_OPTIONS);
  }

  /** Index of the first `spec.edges` item with this `from`/`to`, or -1. */
  private findEdge(edges: YAMLSeq, from: string, to: string): number {
    return edges.items.findIndex(
      (item) =>
        isMap(item) &&
        keyText(item.get("from", true)) === from &&
        keyText(item.get("to", true)) === to,
    );
  }

  /** One node's map, or the reason a mutation on it must be refused. */
  private nodeMapFor(id: string): YAMLMap | string {
    const nodes = this.nodesMap();
    if (typeof nodes === "string") return nodes;
    if (!nodes.has(id)) return `node "${id}" is not declared in spec.nodes`;
    const reason = this.guardPath([...NODES_PATH, id]);
    if (reason) return reason;
    const nodeMap = this.doc.getIn([...NODES_PATH, id], true);
    if (!isMap(nodeMap)) return `node "${id}" is not a mapping`;
    return nodeMap;
  }
}

/** Open an author's workflow source for surgical editing. */
export function openWorkflowDocument(
  source: string,
  format: WorkflowSourceFormat,
): OpenResult {
  return WorkflowDocument.open(source, format);
}
