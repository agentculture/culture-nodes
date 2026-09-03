import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import {
  WORKFLOW_STRINGIFY_OPTIONS,
  openWorkflowDocument,
  type MutationResult,
  type WorkflowDocument,
} from "./workflow-document";
import {
  INVALID_YAML_SOURCE,
  VALID_YAML_SOURCE,
} from "../fixtures/authoring-fixture";

const GOLDEN = "src/domain/__golden__/";
const golden = (name: string) => readFileSync(GOLDEN + name, "utf8");

/** Open a golden, failing the test with the refusal reason if it will not open. */
function open(
  name: string,
  format: "yaml" | "json" = "yaml",
): WorkflowDocument {
  const opened = openWorkflowDocument(golden(name), format);
  if (!opened.ok) throw new Error(`could not open ${name}: ${opened.reason}`);
  return opened.doc;
}

function reasonOf(result: MutationResult): string {
  return result.ok ? "<no refusal: the mutation was applied>" : result.reason;
}

/**
 * A minimal unified diff (three lines of context) so a golden can state
 * *which lines changed* rather than restating the whole file. Test-local on
 * purpose: nothing in the product needs to render a diff.
 */
const CONTEXT = 3;

function unifiedDiff(before: string, after: string): string {
  const a = before.split("\n");
  const b = after.split("\n");
  const lcs: number[][] = Array.from({ length: a.length + 1 }, () =>
    new Array<number>(b.length + 1).fill(0),
  );
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      lcs[i][j] =
        a[i] === b[j]
          ? lcs[i + 1][j + 1] + 1
          : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
    }
  }

  type Op = { tag: " " | "-" | "+"; text: string; ai: number; bi: number };
  const ops: Op[] = [];
  let i = 0;
  let j = 0;
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) ops.push({ tag: " ", text: a[i], ai: i++, bi: j++ });
    else if (lcs[i + 1][j] >= lcs[i][j + 1])
      ops.push({ tag: "-", text: a[i], ai: i++, bi: j });
    else ops.push({ tag: "+", text: b[j], ai: i, bi: j++ });
  }
  while (i < a.length) ops.push({ tag: "-", text: a[i], ai: i++, bi: j });
  while (j < b.length) ops.push({ tag: "+", text: b[j], ai: i, bi: j++ });

  const changed = ops
    .map((op, index) => (op.tag === " " ? -1 : index))
    .filter((index) => index >= 0);
  if (changed.length === 0) return "";

  const hunks: string[] = [];
  // Two runs of changes closer than this share a hunk, as `diff -u` does.
  const merge = CONTEXT * 2 + 1;
  let cursor = 0;
  while (cursor < changed.length) {
    const start = Math.max(0, changed[cursor] - CONTEXT);
    let end = changed[cursor];
    while (cursor + 1 < changed.length && changed[cursor + 1] - end <= merge) {
      cursor += 1;
      end = changed[cursor];
    }
    cursor += 1;
    const slice = ops.slice(start, Math.min(ops.length, end + CONTEXT + 1));
    const aLen = slice.filter((op) => op.tag !== "+").length;
    const bLen = slice.filter((op) => op.tag !== "-").length;
    hunks.push(
      `@@ -${slice[0].ai + 1},${aLen} +${slice[0].bi + 1},${bLen} @@\n` +
        slice.map((op) => op.tag + op.text).join("\n"),
    );
  }
  return hunks.join("\n") + "\n";
}

describe("the spike's stringify options", () => {
  it("are exactly the ones docs/decisions/2026-09-03-design-canvas-spike.md measured", () => {
    expect(WORKFLOW_STRINGIFY_OPTIONS).toEqual({
      flowCollectionPadding: false,
      lineWidth: 0,
      indentSeq: true,
    });
  });
});

// Acceptance criterion 1: open → no edit → toString is byte-identical.
describe("open → no edit → toString", () => {
  it("round-trips a commented, anchored, deliberately ordered YAML document byte-identically", () => {
    const source = golden("round-trip.workflow.yaml");
    // The fixture is only worth anything if it really carries the constructs
    // a naive re-serializer drops.
    expect(source).toMatch(/^# Round-trip golden/);
    expect(source).toContain("\n\nmetadata:"); // a blank line
    expect(source).toContain("&owner"); // an anchor
    expect(source).toContain("*owner"); // and its alias
    expect(source).toContain("kind: agent # the author's trailing comment");
    expect(source).toContain("    # the only agent node in this fixture");
    expect(source.indexOf("ownerRef: &owner")).toBeLessThan(
      source.indexOf("name: golden-round-trip"), // a non-alphabetical key order
    );

    expect(open("round-trip.workflow.yaml").toString()).toBe(source);
  });

  it("round-trips a JSON document byte-identically", () => {
    const source = golden("round-trip.workflow.json");
    expect(open("round-trip.workflow.json", "json").toString()).toBe(source);
  });

  it("round-trips the flow-style document byte-identically", () => {
    const source = golden("flow-edges.workflow.yaml");
    expect(open("flow-edges.workflow.yaml").toString()).toBe(source);
  });

  it("round-trips the committed compose smoke workflow byte-identically", () => {
    // The file the t12 spike measured, read from where it actually lives so
    // this test breaks if that document grows a construct the options lose.
    const source = readFileSync(
      "../deploy/compose/testdata/smoke.workflow.yaml",
      "utf8",
    );
    const opened = openWorkflowDocument(source, "yaml");
    expect(opened.ok).toBe(true);
    expect(opened.ok && opened.doc.toString()).toBe(source);
  });

  it("round-trips both committed authoring fixtures byte-identically", () => {
    for (const source of [VALID_YAML_SOURCE, INVALID_YAML_SOURCE]) {
      const opened = openWorkflowDocument(source, "yaml");
      expect(opened.ok).toBe(true);
      expect(opened.ok && opened.doc.toString()).toBe(source);
    }
  });

  it("refuses a source it cannot parse instead of throwing", () => {
    const bad = openWorkflowDocument("spec: [unterminated\n", "yaml");
    expect(bad.ok).toBe(false);
    expect(bad.ok || bad.reason).toBeTruthy();

    const badJson = openWorkflowDocument("{not json", "json");
    expect(badJson.ok).toBe(false);
    expect(badJson.ok || badJson.reason).toMatch(/not valid JSON/);
  });

  it("refuses a document whose root is not a mapping", () => {
    const scalar = openWorkflowDocument("just a string\n", "yaml");
    expect(scalar.ok).toBe(false);
    expect(scalar.ok || scalar.reason).toBe(
      "the document root is not a mapping",
    );
  });
});

// Acceptance criterion 2, first half: a mutation changes only inserted lines.
describe("addNode and addEdge", () => {
  it("change only the inserted lines", () => {
    const source = golden("round-trip.workflow.yaml");
    const doc = open("round-trip.workflow.yaml");

    expect(
      doc.addNode("review", {
        kind: "agent",
        ownerRef: "team/platform-ai",
        outcomes: ["completed"],
      }),
    ).toEqual({ ok: true });
    expect(
      doc.addEdge({ from: "greet.changes_required", to: "review" }),
    ).toEqual({ ok: true });

    const after = doc.toString();
    expect(unifiedDiff(source, after)).toBe(
      golden("round-trip.add-node-add-edge.diff"),
    );
    // Restating the property the golden encodes, independently of it: every
    // line of the original still appears, in order, in the result.
    const remaining = after.split("\n");
    for (const line of source.split("\n")) {
      const at = remaining.indexOf(line);
      expect(at, `original line dropped or reordered: ${line}`).toBeGreaterThanOrEqual(0);
      remaining.splice(0, at + 1);
    }
  });

  it("write the insert in the flow style the author already uses", () => {
    const source = golden("flow-edges.workflow.yaml");
    const doc = open("flow-edges.workflow.yaml");
    doc.addNode("review", {
      kind: "agent",
      ownerRef: "team/platform-ai",
      outcomes: ["completed"],
    });
    doc.addEdge({ from: "greet.completed", to: "review" });

    expect(unifiedDiff(source, doc.toString())).toBe(
      golden("flow-edges.add-node-add-edge.diff"),
    );
  });

  it("refuses a node id that is already declared", () => {
    const doc = open("round-trip.workflow.yaml");
    const source = golden("round-trip.workflow.yaml");
    const result = doc.addNode("greet", { kind: "agent" });
    expect(result.ok).toBe(false);
    expect(reasonOf(result)).toBe('node "greet" is already declared in spec.nodes');
    expect(doc.toString()).toBe(source);
  });

  it("refuses an edge that is already there, on the same identity removeEdge uses", () => {
    const source = golden("round-trip.workflow.yaml");
    const doc = open("round-trip.workflow.yaml");
    const result = doc.addEdge({ from: "greet.completed", to: "finish" });
    expect(result.ok).toBe(false);
    expect(reasonOf(result)).toBe(
      'spec.edges already has an edge from "greet.completed" to "finish"',
    );
    expect(doc.toString()).toBe(source);
    // The edge it refused to duplicate is the one removeEdge finds.
    expect(doc.removeEdge("greet.completed", "finish")).toEqual({ ok: true });
  });

  it("refuses when the collection the mutation targets is absent", () => {
    const opened = openWorkflowDocument("spec:\n  entry: greet\n", "yaml");
    expect(opened.ok).toBe(true);
    if (!opened.ok) return;
    expect(reasonOf(opened.doc.addNode("a", { kind: "agent" }))).toBe(
      "spec.nodes is not present in the document",
    );
    expect(reasonOf(opened.doc.addEdge({ from: "a.completed", to: "b" }))).toBe(
      "spec.edges is not present in the document",
    );
    expect(opened.doc.toString()).toBe("spec:\n  entry: greet\n");
  });
});

// Acceptance criterion 2, second half: a merge key at the site is refused.
describe("merge keys", () => {
  it("refuses addNode when spec.nodes itself holds a merge key, and re-serializes nothing", () => {
    const source = golden("merge-key-nodes.workflow.yaml");
    const doc = open("merge-key-nodes.workflow.yaml");
    const result = doc.addNode("review", { kind: "agent" });

    expect(result.ok).toBe(false);
    expect(reasonOf(result)).toContain("spec.nodes");
    expect(reasonOf(result)).toContain("merge key (<<)");
    expect(doc.toString()).toBe(source);
  });

  it("refuses setNodeProp and removeNode when the node's own map holds a merge key", () => {
    const source = golden("merge-key-nodes.workflow.yaml");
    const doc = open("merge-key-nodes.workflow.yaml");

    const set = doc.setNodeProp("greet", "ownerRef", "team/other");
    expect(set.ok).toBe(false);
    expect(reasonOf(set)).toContain("merge key (<<)");

    const removed = doc.removeNode("greet");
    expect(removed.ok).toBe(false);
    expect(reasonOf(removed)).toContain("merge key (<<)");

    expect(doc.toString()).toBe(source);
  });

  it("refuses addEdge when a map on the path to spec.edges holds a merge key", () => {
    const source = golden("merge-key-spec.workflow.yaml");
    const doc = open("merge-key-spec.workflow.yaml");
    const result = doc.addEdge({ from: "greet.completed", to: "finish" });

    expect(result.ok).toBe(false);
    // The site itself is not even in the file — `spec` is what carries the
    // merge, so that is what the reason has to name.
    expect(reasonOf(result)).toBe(
      "spec holds a YAML merge key (<<), so its effective keys are not the text in the file; edit the anchored map instead",
    );
    expect(doc.toString()).toBe(source);
  });

  it("refuses only the sites the merge key actually covers", () => {
    // spec.edges in merge-key-nodes.workflow.yaml is ordinary text, so the
    // refusal above is about the site, not about the document.
    const doc = open("merge-key-nodes.workflow.yaml");
    expect(doc.addEdge({ from: "finish.completed", to: "finish" })).toEqual({
      ok: true,
    });
    expect(doc.toString()).toContain("    - from: finish.completed\n");
  });

  it("refuses a mutation site reached through an alias", () => {
    const source = [
      "templates:",
      "  nodes: &shared",
      "    greet:",
      "      kind: agent",
      "spec:",
      "  nodes: *shared",
      "  edges: []",
      "",
    ].join("\n");
    const opened = openWorkflowDocument(source, "yaml");
    expect(opened.ok).toBe(true);
    if (!opened.ok) return;
    const result = opened.doc.addNode("review", { kind: "agent" });
    expect(result.ok).toBe(false);
    expect(reasonOf(result)).toBe(
      "spec.nodes is a YAML alias; edit the anchored value it points at instead",
    );
    expect(opened.doc.toString()).toBe(source);
  });
});

describe("setNodeProp", () => {
  it("edits the value in place and keeps the author's trailing comment", () => {
    const source = golden("round-trip.workflow.yaml");
    const doc = open("round-trip.workflow.yaml");
    expect(doc.setNodeProp("greet", "kind", "code")).toEqual({ ok: true });

    const after = doc.toString();
    expect(after).toContain("kind: code # the author's trailing comment");
    expect(after.split("\n").length).toBe(source.split("\n").length);
    expect(unifiedDiff(source, after)).toBe(
      [
        "@@ -19,7 +19,7 @@",
        "   nodes:",
        "     # the only agent node in this fixture",
        "     greet:",
        "-      kind: agent # the author's trailing comment",
        "+      kind: code # the author's trailing comment",
        "       ownerRef: *owner",
        "       outcomes: [completed, changes_required]",
        " ",
        "",
      ].join("\n"),
    );
  });

  it("appends a key the node does not have yet", () => {
    const source = golden("round-trip.workflow.yaml");
    const doc = open("round-trip.workflow.yaml");
    expect(doc.setNodeProp("finish", "output", { from: "/run/input" })).toEqual(
      { ok: true },
    );
    expect(doc.setNodeProp("finish", "uses", "actor://company/x")).toEqual({
      ok: true,
    });
    const after = doc.toString();
    expect(after).toContain("      uses: actor://company/x\n");
    // `output` already existed on `finish`, so it is replaced, not appended.
    expect(after).toContain("      output:\n        from: /run/input\n");
    expect(after).not.toContain("/nodes/greet/output");
    expect(unifiedDiff(source, after)).toBe(
      [
        "@@ -27,7 +27,8 @@",
        "       kind: end",
        "       ownerRef: *owner",
        "       output:",
        "-        from: /nodes/greet/output",
        "+        from: /run/input",
        "+      uses: actor://company/x",
        " ",
        "   edges:",
        "     - to: finish",
        "",
      ].join("\n"),
    );
  });

  it("refuses a node that is not declared", () => {
    const doc = open("round-trip.workflow.yaml");
    expect(reasonOf(doc.setNodeProp("nope", "kind", "agent"))).toBe(
      'node "nope" is not declared in spec.nodes',
    );
  });
});

describe("removeNode and removeEdge", () => {
  it("removes only the node's own block", () => {
    const source = golden("round-trip.workflow.yaml");
    const doc = open("round-trip.workflow.yaml");
    expect(doc.removeNode("finish")).toEqual({ ok: true });
    const after = doc.toString();
    expect(after).not.toContain("    finish:");
    // The edge that pointed at it is deliberately left alone.
    expect(after).toContain("    - to: finish\n      from: greet.completed\n");
    // Everything before the removed block is untouched.
    expect(after.slice(0, after.indexOf("  edges:"))).toBe(
      source.slice(0, source.indexOf("    finish:")),
    );
  });

  it("removes the matching edge", () => {
    const doc = open("round-trip.workflow.yaml");
    expect(doc.removeEdge("greet.completed", "finish")).toEqual({ ok: true });
    const after = doc.toString();
    expect(after).not.toContain("greet.completed");
    expect(after).toContain("  edges: []\n");
  });

  it("refuses an edge that is not there and a node that is not declared", () => {
    const source = golden("round-trip.workflow.yaml");
    const doc = open("round-trip.workflow.yaml");
    expect(reasonOf(doc.removeEdge("greet.completed", "nope"))).toBe(
      'spec.edges has no edge from "greet.completed" to "nope"',
    );
    expect(reasonOf(doc.removeNode("nope"))).toBe(
      'node "nope" is not declared in spec.nodes',
    );
    expect(doc.toString()).toBe(source);
  });
});

// Acceptance criterion 2, third part: JSON sources take the same API.
describe("JSON documents", () => {
  it("take the same mutations and stay JSON", () => {
    const source = golden("round-trip.workflow.json");
    const doc = open("round-trip.workflow.json", "json");
    expect(doc.format).toBe("json");

    expect(
      doc.addNode("review", {
        kind: "agent",
        ownerRef: "team/platform-ai",
        outcomes: ["completed"],
      }),
    ).toEqual({ ok: true });
    expect(
      doc.addEdge({ from: "greet.changes_required", to: "review" }),
    ).toEqual({ ok: true });
    expect(doc.setNodeProp("greet", "kind", "code")).toEqual({ ok: true });
    expect(doc.removeNode("finish")).toEqual({ ok: true });

    const after = doc.toString();
    expect(after).toBe(golden("round-trip.mutated.workflow.json"));
    // Still JSON, and still the author's key order.
    const parsed = JSON.parse(after);
    expect(Object.keys(parsed.spec.nodes)).toEqual(["greet", "review"]);
    expect(Object.keys(parsed.spec.nodes.greet)).toEqual([
      "kind",
      "ownerRef",
      "outcomes",
    ]);
    expect(parsed.spec.edges).toEqual([
      { to: "finish", from: "greet.completed" },
      { from: "greet.changes_required", to: "review" },
    ]);
    expect(source).not.toBe(after);
  });

  it("refuses the same merge-key site when the source is JSON-formatted YAML", () => {
    // A `.json`-named file that is not JSON is refused at open, so the merge
    // guard is never reached by a document pretending to be JSON.
    const opened = openWorkflowDocument(
      golden("merge-key-nodes.workflow.yaml"),
      "json",
    );
    expect(opened.ok).toBe(false);
    expect(opened.ok || opened.reason).toMatch(/not valid JSON/);
  });

  it("keeps a four-space author's indent", () => {
    const source = JSON.stringify(
      { spec: { entry: "a", nodes: { a: { kind: "agent" } }, edges: [] } },
      null,
      4,
    );
    const opened = openWorkflowDocument(source, "json");
    expect(opened.ok).toBe(true);
    if (!opened.ok) return;
    expect(opened.doc.toString()).toBe(source);
    expect(opened.doc.addNode("b", { kind: "end" })).toEqual({ ok: true });
    expect(opened.doc.toString()).toContain('\n            "b": {\n');
  });
});
