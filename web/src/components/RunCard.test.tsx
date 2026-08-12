import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import RunCard from "./RunCard";
import type { Run } from "../api/types";

const NOW = new Date("2026-08-09T09:10:00Z");

const RUN: Run = {
  id: "run-01J8XKQ3P0FIRSTSLICEXYZ",
  workflow_digest:
    "sha256:2c1e0a9b6d4f8e37a5b0c9d2e1f4a7b6c3d8e5f2a9b4c7d0e3f6a1b8c5d2e9f4",
  state: "running",
  created_at: "2026-08-09T09:00:00Z",
  updated_at: "2026-08-09T09:05:00Z",
};

function renderCard(
  run: Run,
  props: { reducedMotion?: boolean; now?: Date } = {},
) {
  return render(
    <MemoryRouter initialEntries={["/board"]}>
      <Routes>
        <Route
          path="/board"
          element={
            <RunCard
              run={run}
              reducedMotion={props.reducedMotion}
              now={props.now ?? NOW}
            />
          }
        />
        <Route path="/runs/:id" element={<p>run view for {run.id}</p>} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("RunCard", () => {
  it("shows the workflow reference, a shortened run id, and the relative update time", () => {
    const { container } = renderCard(RUN);
    expect(screen.getByTitle(RUN.workflow_digest)).toBeInTheDocument();
    // Shortened per the repo's digest-slicing convention (20 chars + ellipsis).
    expect(
      screen.getByText(`${RUN.workflow_digest.slice(0, 20)}…`),
    ).toBeInTheDocument();
    expect(screen.getByText(`${RUN.id.slice(0, 20)}…`)).toBeInTheDocument();
    expect(screen.getByText("5 minutes ago")).toBeInTheDocument();
    expect(container.querySelector(".run-card")).toHaveAttribute(
      "data-run-id",
      RUN.id,
    );
  });

  it("carries the run's state as a data attribute and a status chip", () => {
    const { container } = renderCard({ ...RUN, state: "waiting" });
    const card = container.querySelector<HTMLElement>(".run-card");
    expect(card?.dataset.runState).toBe("waiting");
    expect(screen.getByText("waiting")).toBeInTheDocument();
  });

  it("is a named link to the run's own /runs/:id view", () => {
    renderCard(RUN);
    const link = screen.getByRole("link", {
      name: `run ${RUN.id}, running, updated 5 minutes ago`,
    });
    expect(link).toHaveAttribute("href", `/runs/${RUN.id}`);
  });

  it("navigates to the run view on click", async () => {
    const user = userEvent.setup();
    renderCard(RUN);
    await user.click(
      screen.getByRole("link", { name: new RegExp(`run ${RUN.id}`) }),
    );
    expect(await screen.findByText(`run view for ${RUN.id}`)).toBeInTheDocument();
  });

  it("is in the tab order and navigates on Enter, the native link activation key", async () => {
    const user = userEvent.setup();
    renderCard(RUN);
    await user.tab();
    const link = screen.getByRole("link", {
      name: new RegExp(`run ${RUN.id}`),
    });
    expect(link).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(await screen.findByText(`run view for ${RUN.id}`)).toBeInTheDocument();
  });

  describe("running-state pulse and its reduced-motion fallback", () => {
    it("pulses a running card when motion is allowed", () => {
      const { container } = renderCard(
        { ...RUN, state: "running" },
        { reducedMotion: false },
      );
      expect(container.querySelector(".run-card.is-pulse")).toBeInTheDocument();
      expect(screen.queryByText("updating live")).not.toBeInTheDocument();
    });

    it("replaces the pulse with a text badge when motion is reduced", () => {
      const { container } = renderCard(
        { ...RUN, state: "running" },
        { reducedMotion: true },
      );
      expect(
        container.querySelector(".run-card.is-pulse"),
      ).not.toBeInTheDocument();
      expect(screen.getByText("updating live")).toBeInTheDocument();
    });

    it("never pulses a card that is not running", () => {
      const { container } = renderCard(
        { ...RUN, state: "completed" },
        { reducedMotion: false },
      );
      expect(container.querySelector(".is-pulse")).not.toBeInTheDocument();
    });
  });

  it("renders only what the run record carries — no client-invented fields", () => {
    // honesty condition h5: the card must not require data a run record
    // doesn't have (no output/completed_at needed here).
    const minimal: Run = {
      id: "run-minimal",
      workflow_digest: "sha256:abc",
      state: "created",
      created_at: "2026-08-09T09:00:00Z",
      updated_at: "2026-08-09T09:00:00Z",
    };
    expect(() => renderCard(minimal)).not.toThrow();
  });

  describe("name, derived hint, and category (task t5)", () => {
    it("shows the operator-given name, not marked as derived", () => {
      const { container } = renderCard({ ...RUN, name: "nightly regression sweep" });
      const name = screen.getByText("nightly regression sweep");
      expect(name).toBeInTheDocument();
      expect(name).toHaveAttribute("data-derived", "false");
      expect(container.querySelector(".run-name--derived")).toBeNull();
    });

    it("falls back to display_hint, visibly marked as a derived guess", () => {
      const { container } = renderCard({
        ...RUN,
        display_hint: "add the ledger projection endpoint",
      });
      const hint = screen.getByText("add the ledger projection endpoint");
      expect(hint).toHaveAttribute("data-derived", "true");
      expect(hint.className).toContain("run-name--derived");
      // The title makes explicit this is a guess, never presented as a given name.
      expect(hint).toHaveAttribute(
        "title",
        expect.stringContaining("derived guess"),
      );
      expect(container.querySelector(".run-name:not(.run-name--derived)")).toBeNull();
    });

    it("renders neither a name row nor a duplicate id when the run has no name or hint", () => {
      const { container } = renderCard(RUN);
      expect(container.querySelector(".run-name")).toBeNull();
      // The id renders exactly once (in .run-card__id), never duplicated as a "name".
      expect(screen.getAllByText(`${RUN.id.slice(0, 20)}…`)).toHaveLength(1);
    });

    it("renders the category as a small chip alongside the name", () => {
      const { container } = renderCard({ ...RUN, name: "nightly sweep", category: "ci" });
      const chip = container.querySelector(".category-chip");
      expect(chip).toHaveTextContent("ci");
      expect(chip).toHaveAttribute("data-category", "ci");
    });

    it("renders no category chip when the run has none", () => {
      const { container } = renderCard(RUN);
      expect(container.querySelector(".category-chip")).toBeNull();
    });
  });
});
