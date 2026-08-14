import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import PlanView from "./PlanView";
import { ApiError, getPlanImport, listPlanImports } from "../api/client";
import {
  PLAN_IMPORT,
  PLAN_IMPORT_SUMMARIES,
  PLAN_SLUG,
} from "../fixtures/plan-fixture";
import { getAgentState, resetAgentState } from "../agent-state/store";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, listPlanImports: vi.fn(), getPlanImport: vi.fn() };
});

const mockListPlanImports = vi.mocked(listPlanImports);
const mockGetPlanImport = vi.mocked(getPlanImport);

function renderPlan(slug: string | null = PLAN_SLUG) {
  const path = slug ? `/plan/${slug}` : "/plan";
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/plan" element={<PlanView />} />
        <Route path="/plan/:slug" element={<PlanView />} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockListPlanImports.mockReset();
  mockGetPlanImport.mockReset();
  resetAgentState();
});

describe("PlanView — waves, task status, and the origin distinction (task t23)", () => {
  it("renders the most recent snapshot's waves and per-task status", async () => {
    mockListPlanImports.mockResolvedValue({ items: PLAN_IMPORT_SUMMARIES });
    mockGetPlanImport.mockResolvedValue(PLAN_IMPORT);

    renderPlan();
    await waitFor(() => expect(getAgentState().status).toBe("ready"));

    expect(mockListPlanImports).toHaveBeenCalledWith(PLAN_SLUG, expect.anything());
    // items[0] (the most recent summary) is the one fetched in full.
    expect(mockGetPlanImport).toHaveBeenCalledWith(
      PLAN_IMPORT_SUMMARIES[0].id,
      expect.anything(),
    );

    // Two real waves (t1/t2 in wave 1, t3/t4 in wave 2) plus the
    // unscheduled (rejected) bucket for t5.
    expect(document.querySelectorAll("#plan-waves .plan-wave")).toHaveLength(3);
    const wave1 = document.querySelector('.plan-wave[data-wave="1"]');
    expect(wave1?.querySelectorAll(".plan-task")).toHaveLength(2);
    const wave2 = document.querySelector('.plan-wave[data-wave="2"]');
    expect(wave2?.querySelectorAll(".plan-task")).toHaveLength(2);
    expect(document.getElementById("plan-unscheduled")).toBeInTheDocument();

    // t3's real dependency is only t1, even though t2 is also in t1's wave
    // (spec c15 — never the "everything in the previous wave" reading).
    const t3Deps = document.querySelector(
      '.plan-task[data-task-ref="t3"] .plan-task__deps',
    );
    expect(t3Deps).toHaveTextContent("t1");
    expect(t3Deps?.textContent).not.toContain("t2");

    // Per-task status renders via the AuthorityChip vocabulary, not
    // flattened to one value: t1 confirmed (solid), t2 proposed (dashed).
    const t1Chip = document.querySelector(
      '.plan-task[data-task-ref="t1"] .authority-chip',
    );
    expect(t1Chip).toHaveAttribute("data-authority", "confirmed");
    const t2Chip = document.querySelector(
      '.plan-task[data-task-ref="t2"] .authority-chip',
    );
    expect(t2Chip).toHaveAttribute("data-authority", "proposed");
  });

  it("distinguishes a human/user-origin deviation from an agent/llm-origin one at a glance", async () => {
    mockListPlanImports.mockResolvedValue({ items: PLAN_IMPORT_SUMMARIES });
    mockGetPlanImport.mockResolvedValue(PLAN_IMPORT);

    renderPlan();
    await waitFor(() => expect(getAgentState().status).toBe("ready"));

    const table = document.getElementById("plan-deviations-table");
    expect(table).toBeInTheDocument();
    expect(document.querySelectorAll("#plan-deviations-table tbody tr")).toHaveLength(
      PLAN_IMPORT.deviations.length,
    );

    // d1 is origin "human" ("user reports"): rendered SOLID/confirmed.
    const d1Row = document.querySelector('tr[data-deviation-ref="d1"]');
    expect(d1Row).toHaveAttribute("data-origin", "human");
    const d1OriginChip = d1Row?.querySelector(".plan-origin .authority-chip");
    expect(d1OriginChip).toHaveAttribute("data-authority", "confirmed");
    expect(d1OriginChip).toHaveAttribute("data-edge-style", "SOLID");
    expect(d1Row).toHaveTextContent("user reports");

    // d2 is origin "agent" ("system knows"): rendered DASHED/proposed —
    // visibly distinct from d1 even though both are otherwise plain rows.
    const d2Row = document.querySelector('tr[data-deviation-ref="d2"]');
    expect(d2Row).toHaveAttribute("data-origin", "agent");
    const d2OriginChip = d2Row?.querySelector(".plan-origin .authority-chip");
    expect(d2OriginChip).toHaveAttribute("data-authority", "proposed");
    expect(d2OriginChip).toHaveAttribute("data-edge-style", "DASHED");
    expect(d2Row).toHaveTextContent("system knows");

    // The two origins render with genuinely different chip styling, not
    // just different text — the "at a glance" acceptance bar.
    expect(d1OriginChip?.getAttribute("data-authority")).not.toBe(
      d2OriginChip?.getAttribute("data-authority"),
    );
  });

  it("shows an honest empty state for a slug with no imports", async () => {
    mockListPlanImports.mockResolvedValue({ items: [] });

    renderPlan("never-imported");
    await waitFor(() => expect(getAgentState().status).toBe("ready"));

    expect(document.getElementById("plan-view-not-found")).toBeInTheDocument();
    expect(mockGetPlanImport).not.toHaveBeenCalled();
  });

  it("prompts for a slug when none is in the URL, and navigating the form goes to /plan/:slug", async () => {
    const user = userEvent.setup();
    renderPlan(null);

    expect(document.getElementById("plan-view-empty")).toBeInTheDocument();
    expect(mockListPlanImports).not.toHaveBeenCalled();

    mockListPlanImports.mockResolvedValue({ items: PLAN_IMPORT_SUMMARIES });
    mockGetPlanImport.mockResolvedValue(PLAN_IMPORT);

    await user.type(screen.getByLabelText("Plan slug"), PLAN_SLUG);
    await user.click(screen.getByRole("button", { name: "Go" }));

    await waitFor(() =>
      expect(mockListPlanImports).toHaveBeenCalledWith(PLAN_SLUG, expect.anything()),
    );
  });

  it("renders an error notice and still marks agent-state ready when the fetch fails", async () => {
    mockListPlanImports.mockRejectedValue(
      new ApiError(0, "cannot reach the control plane", "start `nodes serve`"),
    );
    renderPlan();
    await screen.findByText("error:", { exact: false });
    expect(getAgentState().status).toBe("ready");
  });
});
