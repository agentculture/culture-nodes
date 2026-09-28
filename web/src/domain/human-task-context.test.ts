import { describe, expect, it } from "vitest";
import {
  JIRA_SITE_DEFAULT,
  githubBlobHref,
  jiraBrowseHref,
  parseJsonObject,
  readAgentReport,
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

describe("agent reports that arrive as JSON strings (task t46)", () => {
  const base: HumanTask = {
    ...PENDING_TASK_MINIMAL,
    id: "ht-report",
  };

  it("parses only a string that is a JSON object", () => {
    expect(parseJsonObject(' {"reason":"x"} ')).toEqual({ reason: "x" });
    expect(parseJsonObject("[1,2]")).toBeUndefined();
    expect(parseJsonObject("{not json")).toBeUndefined();
    expect(parseJsonObject("plain prose")).toBeUndefined();
  });

  it("names the known keys and sends the rest to the audit", () => {
    const report = readAgentReport(
      {
        reason: "no credential",
        remediation: "re-grant",
        summary: "could not push",
        evidence: "push: 403",
        changes_made: "none",
        intended_line: "Jira: SCRUM-16",
        pr: "gh:o/r#7",
        workspace_measured: { dirty: false },
        extra: 1,
      },
      undefined,
    );
    expect(report).toMatchObject({
      reason: "no credential",
      remediation: "re-grant",
      summary: "could not push",
      evidence: ["push: 403"],
      changesMade: "none",
      intendedLine: "Jira: SCRUM-16",
      pr: { label: "o/r#7", href: "https://github.com/o/r/pull/7" },
    });
    expect(report.audit).toEqual({ workspace_measured: { dirty: false }, extra: 1 });
  });

  it("links a bare PR number only when the repository is known, and never a malformed ref", () => {
    expect(readAgentReport({ pr: 7 }, "o/r").pr?.href).toBe("https://github.com/o/r/pull/7");
    expect(readAgentReport({ pr: 7 }).pr).toEqual({ label: "#7" });
    expect(readAgentReport({ pr: "javascript:alert(1)" }).pr?.href).toBeUndefined();
  });

  it("reads a top-level agent_report binding whose value is a JSON string, and keeps prose as prose", () => {
    const facts = taskContextFacts({
      ...base,
      resolved_context: [
        { name: "agent_report", ref: "/nodes/x/output/agent_report", value: '{"reason":"stuck"}' },
        { name: "notes", ref: "/nodes/x/output/notes", value: "just words" },
      ],
    });
    expect(facts.reports).toHaveLength(1);
    expect(facts.reports[0].reason).toBe("stuck");
    expect(facts.narratives).toEqual([{ label: "notes", text: "just words" }]);
  });
});
