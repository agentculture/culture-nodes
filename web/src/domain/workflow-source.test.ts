import { describe, expect, it } from "vitest";
import {
  formatFromFilename,
  parseWorkflowSourceForPreview,
} from "./workflow-source";
import { VALID_YAML_SOURCE } from "../fixtures/authoring-fixture";

describe("parseWorkflowSourceForPreview", () => {
  it("parses a valid authored YAML document into WorkflowIR-shaped data", () => {
    const ir = parseWorkflowSourceForPreview(VALID_YAML_SOURCE, "yaml");
    expect(ir).not.toBeNull();
    expect(ir?.spec.entry).toBe("greet");
    expect(Object.keys(ir?.spec.nodes ?? {})).toEqual(["greet", "finish"]);
    expect(ir?.spec.edges).toEqual([{ from: "greet.completed", to: "finish" }]);
  });

  it("parses the same document authored as JSON", () => {
    const json = JSON.stringify({
      spec: {
        entry: "a",
        nodes: { a: { kind: "agent" }, b: { kind: "end" } },
        edges: [{ from: "a.completed", to: "b" }],
      },
    });
    const ir = parseWorkflowSourceForPreview(json, "json");
    expect(ir?.spec.entry).toBe("a");
  });

  it("returns null for unparseable YAML", () => {
    const ir = parseWorkflowSourceForPreview(
      "not: valid: yaml: [unterminated",
      "yaml",
    );
    expect(ir).toBeNull();
  });

  it("returns null for unparseable JSON", () => {
    expect(parseWorkflowSourceForPreview("{not json", "json")).toBeNull();
  });

  it("returns null when the document has no spec.entry/nodes/edges shape", () => {
    expect(parseWorkflowSourceForPreview("foo: bar", "yaml")).toBeNull();
    expect(parseWorkflowSourceForPreview("[]", "json")).toBeNull();
    expect(parseWorkflowSourceForPreview("null", "yaml")).toBeNull();
  });
});

describe("formatFromFilename", () => {
  it("infers json from a .json extension", () => {
    expect(formatFromFilename("workflow.json")).toBe("json");
    expect(formatFromFilename("WORKFLOW.JSON")).toBe("json");
  });

  it("defaults to yaml for .yaml, .yml, or anything else", () => {
    expect(formatFromFilename("workflow.yaml")).toBe("yaml");
    expect(formatFromFilename("workflow.yml")).toBe("yaml");
    expect(formatFromFilename("workflow.txt")).toBe("yaml");
    expect(formatFromFilename("workflow")).toBe("yaml");
  });
});
