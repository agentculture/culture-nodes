import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import Workflows from "./Workflows";
import { ApiError, listRuns, listWorkflows } from "../api/client";
import {
  DELIVER_CHANGE_V2_DIGEST,
  HELLO_WORLD_DIGEST,
  WORKFLOW_VERSIONS,
  WORKFLOWS_RUNS,
} from "../fixtures/workflows-fixture";
import { WORKFLOW_DIGEST } from "../fixtures/run-fixture";
import { getAgentState, resetAgentState } from "../agent-state/store";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, listWorkflows: vi.fn(), listRuns: vi.fn() };
});

const mockListWorkflows = vi.mocked(listWorkflows);
const mockListRuns = vi.mocked(listRuns);

function renderWorkflows() {
  return render(
    <MemoryRouter initialEntries={["/workflows"]}>
      <Workflows />
    </MemoryRouter>,
  );
}

function resolveFixture() {
  mockListWorkflows.mockResolvedValue({ items: WORKFLOW_VERSIONS });
  mockListRuns.mockResolvedValue({ items: WORKFLOWS_RUNS });
}

beforeEach(() => {
  mockListWorkflows.mockReset();
  mockListRuns.mockReset();
  resetAgentState();
});

describe("Workflows loading/empty/error", () => {
  it("shows a loading state before both requests resolve", () => {
    mockListWorkflows.mockReturnValue(new Promise(() => {}));
    mockListRuns.mockReturnValue(new Promise(() => {}));
    renderWorkflows();
    expect(screen.getByText("Loading workflows…")).toBeInTheDocument();
  });

  it("shows the empty state when no workflow has been published", async () => {
    mockListWorkflows.mockResolvedValue({ items: [] });
    mockListRuns.mockResolvedValue({ items: [] });
    renderWorkflows();
    expect(
      await screen.findByText(/No workflows published yet\./),
    ).toBeInTheDocument();
  });

  it("renders an error notice and stops loading when the workflows request fails", async () => {
    mockListWorkflows.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    mockListRuns.mockResolvedValue({ items: [] });
    renderWorkflows();
    await screen.findByText("error:", { exact: false });
    expect(
      screen.getByText("cannot reach the control plane", { exact: false }),
    ).toBeInTheDocument();
  });

  it("renders an error notice when the runs request fails, even if workflows succeeded", async () => {
    mockListWorkflows.mockResolvedValue({ items: WORKFLOW_VERSIONS });
    mockListRuns.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    renderWorkflows();
    await screen.findByText("error:", { exact: false });
  });

  it("marks agent-state ready once both requests settle, loading otherwise", async () => {
    resolveFixture();
    renderWorkflows();
    expect(getAgentState().status).toBe("loading");
    await screen.findByText("deliver-change");
    expect(getAgentState().status).toBe("ready");
  });
});

describe("Workflows data fetch", () => {
  it("requests every published workflow version and every run sorted by updated_at", async () => {
    resolveFixture();
    renderWorkflows();
    await screen.findByText("deliver-change");
    expect(mockListWorkflows).toHaveBeenCalledTimes(1);
    expect(mockListRuns).toHaveBeenCalledTimes(1);
    const [, runParams] = mockListRuns.mock.calls[0];
    expect(runParams).toEqual({ sort: "updated_at" });
  });
});

describe("Workflows grouping and rendering", () => {
  it("renders one card per workflow_key, not one per version", async () => {
    resolveFixture();
    renderWorkflows();
    await screen.findByText("deliver-change");
    expect(
      document.querySelectorAll('[data-workflow-key="deliver-change"]'),
    ).toHaveLength(1);
    expect(
      document.querySelectorAll('[data-workflow-key="hello-world"]'),
    ).toHaveLength(1);
  });

  it("lists every version of a workflow with its own digest, newest version first", async () => {
    resolveFixture();
    renderWorkflows();
    await screen.findByText("deliver-change");
    const card = document.querySelector(
      '[data-workflow-key="deliver-change"]',
    ) as HTMLElement;
    const rows = within(card).getAllByRole("row").slice(1); // drop header row
    expect(rows).toHaveLength(2);
    expect(
      within(card).getByText(String(2)),
    ).toBeInTheDocument();
    expect(
      card.querySelector(`[data-workflow-digest="${DELIVER_CHANGE_V2_DIGEST}"]`),
    ).toBeInTheDocument();
    expect(
      card.querySelector(`[data-workflow-digest="${WORKFLOW_DIGEST}"]`),
    ).toBeInTheDocument();
  });

  it("shows the owner from the latest version's metadata", async () => {
    resolveFixture();
    renderWorkflows();
    await screen.findByText("deliver-change");
    const card = document.querySelector(
      '[data-workflow-key="deliver-change"]',
    ) as HTMLElement;
    expect(within(card).getByText("team/platform-ai")).toBeInTheDocument();
  });

  it("lists each workflow's recent runs across all of its versions, newest first, never a run belonging to another workflow", async () => {
    resolveFixture();
    renderWorkflows();
    await screen.findByText("deliver-change");
    const card = document.querySelector(
      '[data-workflow-key="deliver-change"]',
    ) as HTMLElement;
    const runLinks = within(card)
      .getAllByRole("link")
      .filter((link) => link.getAttribute("href")?.startsWith("/runs/"));
    expect(runLinks.map((link) => link.textContent)).toEqual([
      "run-deliver-v2-01J8XKWORKFLOWS02",
      "run-deliver-v1-01J8XKWORKFLOWS04",
    ]);
  });

  it("never renders a run whose digest matches no published version", async () => {
    resolveFixture();
    renderWorkflows();
    await screen.findByText("deliver-change");
    expect(
      screen.queryByText("run-orphan-01J8XKWORKFLOWS0003"),
    ).not.toBeInTheDocument();
  });

  it("shows a workflow's own empty state when it has no recent runs", async () => {
    mockListWorkflows.mockResolvedValue({ items: WORKFLOW_VERSIONS });
    mockListRuns.mockResolvedValue({ items: [] });
    renderWorkflows();
    await screen.findByText("hello-world");
    const card = document.querySelector(
      '[data-workflow-key="hello-world"]',
    ) as HTMLElement;
    expect(within(card).getByText(/No runs yet/)).toBeInTheDocument();
  });

  it("every recent-run row is a real link into the existing Run view", async () => {
    resolveFixture();
    renderWorkflows();
    await screen.findByText("hello-world");
    const helloRun = WORKFLOWS_RUNS.find(
      (run) => run.workflow_digest === HELLO_WORLD_DIGEST,
    )!;
    const link = screen.getByRole("link", { name: new RegExp(helloRun.id) });
    expect(link).toHaveAttribute("href", `/runs/${helloRun.id}`);
  });
});
