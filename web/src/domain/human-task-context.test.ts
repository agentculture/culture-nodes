import { describe, expect, it } from "vitest";
import {
  JIRA_SITE_DEFAULT,
  githubBlobHref,
  jiraBrowseHref,
  taskContextFacts,
} from "./human-task-context";
import { MERGE_TASK, PENDING_TASK_MINIMAL } from "../fixtures/human-tasks-fixture";
import type { HumanTask } from "../api/types";

describe("human task context links (issue 332)", () => {
  it("links a Jira key to the site the data names, else the default", () => {
    expect(jiraBrowseHref("SCRUM-15")).toBe(`${JIRA_SITE_DEFAULT}/browse/SCRUM-15`);
    expect(jiraBrowseHref("SCRUM-15", "https://acme.atlassian.net/")).toBe(
      "https://acme.atlassian.net/browse/SCRUM-15",
    );
    expect(jiraBrowseHref("SCRUM-15", "javascript:alert(1)")).toBe(
      `${JIRA_SITE_DEFAULT}/browse/SCRUM-15`,
    );
    expect(jiraBrowseHref("not a key")).toBeUndefined();
  });

  it("builds a file link only from well-formed parts", () => {
    expect(githubBlobHref("o/r", "abc1234", "a b/c.py", 3)).toBe(
      "https://github.com/o/r/blob/abc1234/a%20b/c.py#L3",
    );
    expect(githubBlobHref("o/r", "abc1234", "../etc/passwd")).toBeUndefined();
    expect(githubBlobHref("o/r", "main;rm", "a.py")).toBeUndefined();
    expect(githubBlobHref("https://evil/x", "abc1234", "a.py")).toBeUndefined();
  });

  it("links no finding file without a head sha", () => {
    const value = (MERGE_TASK.resolved_context![0].value as Record<string, unknown>);
    const task: HumanTask = {
      ...MERGE_TASK,
      resolved_context: [{ name: "finding", ref: "/run/input", value: { ...value, head_sha: undefined } }],
    };
    const facts = taskContextFacts(task);
    expect(facts.findings[0].file).toBe("tests/test_hand_turn_cli.py");
    expect(facts.findings[0].fileHref).toBeUndefined();
    expect(facts.prHref).toBe("https://github.com/agentculture/culture-nodes/pull/326");
  });

  it("derives a title from the node when nothing asks a question", () => {
    expect(taskContextFacts(PENDING_TASK_MINIMAL).question).toBe("Gate");
  });
});
