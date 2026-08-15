import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import GenerateWorkflow from "./GenerateWorkflow";
import { createWorkflowGeneration } from "../api/client";
import type { WorkflowGeneration } from "../api/types";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ...actual,
    createWorkflowGeneration: vi.fn(),
    getWorkflowGeneration: vi.fn(),
  };
});

const mockCreate = vi.mocked(createWorkflowGeneration);

function renderView() {
  return render(
    <MemoryRouter initialEntries={["/workflows/generate"]}>
      <GenerateWorkflow />
    </MemoryRouter>,
  );
}

function proposal(over: Partial<WorkflowGeneration> = {}): WorkflowGeneration {
  return {
    id: "gen_01",
    run_id: "01M02TESTRUN0000000000000",
    status: "proposed",
    valid: true,
    source: "apiVersion: nodes.culture.dev/v1alpha1\nkind: Workflow\n",
    ...over,
  } as WorkflowGeneration;
}

async function generate() {
  fireEvent.change(screen.getByLabelText(/description/i), {
    target: { value: "sweep open PRs and triage them" },
  });
  fireEvent.change(screen.getByLabelText(/actor ref/i), {
    target: { value: "actor://company/planner@sha256:aaaa" },
  });
  fireEvent.click(screen.getByRole("button", { name: /generate proposal/i }));
  await waitFor(() => expect(mockCreate).toHaveBeenCalled());
}

describe("GenerateWorkflow", () => {
  beforeEach(() => vi.clearAllMocks());

  // The acceptance criterion this page exists to satisfy: a generated document
  // is a PROPOSAL until a human says otherwise. If the page ever renders a
  // generated workflow as though it were adopted, the human confirmation stops
  // being a gate and becomes a formality — which is the whole risk of letting a
  // model author graphs (#81, PRD §10.4).
  it("shows a generated workflow as proposed, and offers no route onward", async () => {
    mockCreate.mockResolvedValue(proposal());
    renderView();
    await generate();

    // Assert on the machine-readable status the section carries, not on prose:
    // the page's own explainer contains the word "proposed", so a text matcher
    // would pass even if the result rendered as adopted.
    await waitFor(() =>
      expect(
        document.querySelector("#workflow-generation-result"),
      ).toHaveAttribute("data-status", "proposed"),
    );

    // The publish door is deliberately unreachable from an unconfirmed
    // proposal. This assertion is the gate; without it the page could start
    // linking straight through and every other test would still pass.
    expect(
      screen.queryByRole("link", { name: /validate and publish/i }),
    ).not.toBeInTheDocument();
  });

  it("offers the publish door only once a human has confirmed it", async () => {
    mockCreate.mockResolvedValue(proposal({ status: "confirmed", valid: true }));
    renderView();
    await generate();

    expect(
      await screen.findByRole("link", { name: /validate and publish/i }),
    ).toBeInTheDocument();
  });

  // Confirmed-but-not-compiling must not reach the door either. Two independent
  // conditions guard it, and a test that only ever varies one would not notice
  // if the other were dropped.
  it("withholds the publish door from a confirmed proposal that does not compile", async () => {
    mockCreate.mockResolvedValue(
      proposal({ status: "confirmed", valid: false }),
    );
    renderView();
    await generate();

    expect(await screen.findByText(/does not compile/i)).toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: /validate and publish/i }),
    ).not.toBeInTheDocument();
  });

  // Naming a registered actor is not a convenience field: it is what puts
  // generation on the fleet instead of in the control plane, which
  // tests/lint/modelisolation_test.go enforces on the other side. A page that
  // let you generate without one would be describing a capability the control
  // plane is forbidden to have.
  it("cannot dispatch a generation without a registered actor ref", () => {
    renderView();
    fireEvent.change(screen.getByLabelText(/description/i), {
      target: { value: "sweep open PRs and triage them" },
    });
    expect(
      screen.getByRole("button", { name: /generate proposal/i }),
    ).toBeDisabled();
    expect(mockCreate).not.toHaveBeenCalled();
  });

  it("says the agent is still working when no source has arrived yet", async () => {
    mockCreate.mockResolvedValue(proposal({ source: "", valid: false }));
    renderView();
    await generate();

    expect(await screen.findByText(/still working/i)).toBeInTheDocument();
  });
});
