import { describe, expect, it } from "vitest";
import type { HumanTask } from "../api/types";
import {
  BAD_DEADLINE_TASK,
  DECIDED_TASK,
  EXPIRED_TASK,
  INBOX_NOW,
  NOTICE_TASK,
  PENDING_TASK,
  PENDING_TASK_MINIMAL,
  UNDECIDABLE_TASK,
  WAITING_TASK,
} from "../fixtures/human-tasks-fixture";
import {
  isOverdue,
  isUndecidable,
  parseInboxTab,
  partitionInbox,
  taskDeadlineMs,
} from "./inbox-tabs";

describe("inbox tabs (task t44)", () => {
  it("parses ?tab= and falls back to act; t44's open link still lands on act", () => {
    expect(parseInboxTab("waiting")).toBe("waiting");
    expect(parseInboxTab("decided")).toBe("decided");
    expect(parseInboxTab("review")).toBe("review");
    expect(parseInboxTab("act")).toBe("act");
    expect(parseInboxTab("open")).toBe("act");
    expect(parseInboxTab(null)).toBe("act");
    expect(parseInboxTab("Decided")).toBe("act");
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
    expect(tabs.act.map((t) => t.id)).toEqual([
      PENDING_TASK.id,
      PENDING_TASK_MINIMAL.id,
      BAD_DEADLINE_TASK.id,
    ]);
    expect(tabs.waiting.map((t) => t.id)).toEqual([WAITING_TASK.id]);
    expect(tabs.decided.map((t) => t.id)).toEqual([EXPIRED_TASK.id, DECIDED_TASK.id]);
  });

  // Task t45: nothing to select is not open work — but it is never hidden.
  it("puts a pending task with no selectable outcome in Waiting, not Open", () => {
    const expiredOnly: HumanTask = {
      ...PENDING_TASK_MINIMAL,
      id: "ht-expired-only",
      request: { allowed_outcomes: ["expired"] },
    };
    expect(isUndecidable(UNDECIDABLE_TASK)).toBe(true);
    expect(isUndecidable(expiredOnly)).toBe(true);
    expect(isUndecidable(NOTICE_TASK)).toBe(false);
    const tabs = partitionInbox([UNDECIDABLE_TASK, expiredOnly, NOTICE_TASK], INBOX_NOW);
    expect(tabs.act.map((t) => t.id)).toEqual([NOTICE_TASK.id]);
    expect(tabs.waiting.map((t) => t.id)).toEqual([UNDECIDABLE_TASK.id, expiredOnly.id]);
    expect(tabs.decided).toEqual([]);
  });
});
