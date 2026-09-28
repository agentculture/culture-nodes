import { describe, expect, it } from "vitest";
import type { HumanTask } from "../api/types";
import {
  BAD_DEADLINE_TASK,
  DECIDED_TASK,
  EXPIRED_TASK,
  INBOX_NOW,
  PENDING_TASK,
  PENDING_TASK_MINIMAL,
  WAITING_TASK,
} from "../fixtures/human-tasks-fixture";
import { isOverdue, parseInboxTab, partitionInbox, taskDeadlineMs } from "./inbox-tabs";

describe("inbox tabs (task t44)", () => {
  it("parses ?tab= and falls back to open", () => {
    expect(parseInboxTab("waiting")).toBe("waiting");
    expect(parseInboxTab("decided")).toBe("decided");
    expect(parseInboxTab("open")).toBe("open");
    expect(parseInboxTab(null)).toBe("open");
    expect(parseInboxTab("Decided")).toBe("open");
  });

  it("treats a missing, empty or unparseable deadline as no deadline", () => {
    expect(taskDeadlineMs(PENDING_TASK_MINIMAL)).toBeNull();
    expect(taskDeadlineMs(BAD_DEADLINE_TASK)).toBeNull();
    const empty: HumanTask = {
      ...PENDING_TASK,
      request: { ...PENDING_TASK.request, deadline: "" },
    };
    expect(taskDeadlineMs(empty)).toBeNull();
    expect(isOverdue(BAD_DEADLINE_TASK, INBOX_NOW)).toBe(false);
  });

  it("is overdue at exactly the deadline, and never once not pending", () => {
    const at = new Date(WAITING_TASK.request.deadline!);
    expect(isOverdue(WAITING_TASK, at)).toBe(true);
    expect(isOverdue(WAITING_TASK, new Date(at.getTime() - 1))).toBe(false);
    expect(isOverdue({ ...WAITING_TASK, status: "decided" }, INBOX_NOW)).toBe(false);
  });

  it("partitions by status and deadline, dedups, and sorts decided newest first", () => {
    const tabs = partitionInbox(
      [
        PENDING_TASK,
        WAITING_TASK,
        PENDING_TASK_MINIMAL,
        BAD_DEADLINE_TASK,
        DECIDED_TASK,
        EXPIRED_TASK,
        PENDING_TASK,
      ],
      INBOX_NOW,
    );
    expect(tabs.open.map((t) => t.id)).toEqual([
      PENDING_TASK.id,
      PENDING_TASK_MINIMAL.id,
      BAD_DEADLINE_TASK.id,
    ]);
    expect(tabs.waiting.map((t) => t.id)).toEqual([WAITING_TASK.id]);
    expect(tabs.decided.map((t) => t.id)).toEqual([EXPIRED_TASK.id, DECIDED_TASK.id]);
  });
});
