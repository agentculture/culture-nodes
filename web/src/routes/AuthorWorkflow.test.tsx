import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import AuthorWorkflow from "./AuthorWorkflow";
import { ApiError, publishWorkflow, validateWorkflow } from "../api/client";
import {
  AUTHORING_DIGEST,
  INVALID_VALIDATION,
  INVALID_YAML_SOURCE,
  PUBLISHED_VERSION,
  VALID_VALIDATION,
  VALID_YAML_SOURCE,
} from "../fixtures/authoring-fixture";
import { getAgentState, resetAgentState } from "../agent-state/store";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return { ...actual, validateWorkflow: vi.fn(), publishWorkflow: vi.fn() };
});

const mockValidateWorkflow = vi.mocked(validateWorkflow);
const mockPublishWorkflow = vi.mocked(publishWorkflow);

function renderView() {
  return render(
    <MemoryRouter initialEntries={["/workflows/new"]}>
      <AuthorWorkflow />
    </MemoryRouter>,
  );
}

function paste(source: string) {
  const textarea = screen.getByLabelText("Workflow source");
  fireEvent.change(textarea, { target: { value: source } });
}

beforeEach(() => {
  mockValidateWorkflow.mockReset();
  mockPublishWorkflow.mockReset();
  resetAgentState();
});

describe("AuthorWorkflow: paste and validate", () => {
  it("disables Validate until there is source to validate", () => {
    renderView();
    expect(screen.getByRole("button", { name: "Validate" })).toBeDisabled();
    paste(VALID_YAML_SOURCE);
    expect(screen.getByRole("button", { name: "Validate" })).toBeEnabled();
  });

  it("renders invalid diagnostics verbatim, and never enables Publish for them", async () => {
    mockValidateWorkflow.mockResolvedValue(INVALID_VALIDATION);
    renderView();
    paste(INVALID_YAML_SOURCE);
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));

    const diagnostic = await screen.findByText(
      /edge target "does-not-exist" is not a declared node/,
    );
    expect(diagnostic).toBeInTheDocument();
    expect(
      document.querySelector('[data-diagnostic-level="error"]'),
    ).toBeInTheDocument();
    expect(screen.getByText(/declare a node named/)).toBeInTheDocument();

    expect(screen.getByRole("button", { name: "Publish" })).toBeDisabled();
    expect(mockPublishWorkflow).not.toHaveBeenCalled();
    // No preview for an invalid document.
    expect(
      document.querySelector("#workflow-preview-canvas"),
    ).not.toBeInTheDocument();
  });

  it("shows a clean-diagnostics note and enables Publish for a valid document", async () => {
    mockValidateWorkflow.mockResolvedValue(VALID_VALIDATION);
    renderView();
    paste(VALID_YAML_SOURCE);
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));

    await screen.findByText(/No diagnostics/);
    expect(screen.getByRole("button", { name: "Publish" })).toBeEnabled();
  });

  it("calls validateWorkflow with the pasted source and the selected format", async () => {
    mockValidateWorkflow.mockResolvedValue(VALID_VALIDATION);
    renderView();
    paste(VALID_YAML_SOURCE);
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    await waitFor(() => expect(mockValidateWorkflow).toHaveBeenCalledTimes(1));
    expect(mockValidateWorkflow).toHaveBeenCalledWith({
      format: "yaml",
      source: VALID_YAML_SOURCE,
    });
  });

  it("re-disables Publish and drops the stale preview once the source is edited after validating", async () => {
    mockValidateWorkflow.mockResolvedValue(VALID_VALIDATION);
    renderView();
    paste(VALID_YAML_SOURCE);
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Publish" })).toBeEnabled(),
    );

    paste(`${VALID_YAML_SOURCE}\n# edited\n`);
    expect(screen.getByRole("button", { name: "Publish" })).toBeDisabled();
  });
});

describe("AuthorWorkflow: preview", () => {
  it("renders a read-only graph preview once the document validates clean", async () => {
    mockValidateWorkflow.mockResolvedValue(VALID_VALIDATION);
    renderView();
    paste(VALID_YAML_SOURCE);
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));

    await waitFor(() =>
      expect(
        document.querySelector("#workflow-preview-canvas"),
      ).toBeInTheDocument(),
    );
  });
});

describe("AuthorWorkflow: publish", () => {
  it("publishes the exact pasted source bytes, never a re-serialized value", async () => {
    // A source with a deliberately idiosyncratic comment/whitespace, so a
    // round-trip through any YAML re-encoder would change it byte-for-byte.
    const peculiar = `${VALID_YAML_SOURCE}# a trailing comment, and trailing space \n`;
    mockValidateWorkflow.mockResolvedValue(VALID_VALIDATION);
    mockPublishWorkflow.mockResolvedValue(PUBLISHED_VERSION);
    renderView();
    paste(peculiar);
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Publish" })).toBeEnabled(),
    );

    fireEvent.click(screen.getByRole("button", { name: "Publish" }));
    await waitFor(() => expect(mockPublishWorkflow).toHaveBeenCalledTimes(1));
    expect(mockPublishWorkflow).toHaveBeenCalledWith({
      format: "yaml",
      source: peculiar,
    });
  });

  it("renders the returned digest after a successful publish", async () => {
    mockValidateWorkflow.mockResolvedValue(VALID_VALIDATION);
    mockPublishWorkflow.mockResolvedValue(PUBLISHED_VERSION);
    renderView();
    paste(VALID_YAML_SOURCE);
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Publish" })).toBeEnabled(),
    );
    fireEvent.click(screen.getByRole("button", { name: "Publish" }));

    await screen.findByText(AUTHORING_DIGEST);
    expect(
      document.querySelector(`[data-published-digest="${AUTHORING_DIGEST}"]`),
    ).toBeInTheDocument();
  });

  it("surfaces a publish failure via ErrorNotice without pretending success", async () => {
    mockValidateWorkflow.mockResolvedValue(VALID_VALIDATION);
    mockPublishWorkflow.mockRejectedValue(
      new ApiError(422, "the document does not compile", "run validate again"),
    );
    renderView();
    paste(VALID_YAML_SOURCE);
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Publish" })).toBeEnabled(),
    );
    fireEvent.click(screen.getByRole("button", { name: "Publish" }));

    await screen.findByText("error:", { exact: false });
    expect(
      screen.getByText("the document does not compile", { exact: false }),
    ).toBeInTheDocument();
    expect(
      document.querySelector("#workflow-publish-result"),
    ).not.toBeInTheDocument();
  });
});

describe("AuthorWorkflow: upload", () => {
  it("populates the source and infers the format from an uploaded file", async () => {
    renderView();
    const file = new File([VALID_YAML_SOURCE], "workflow.yaml", {
      type: "application/x-yaml",
    });
    const input = document.querySelector<HTMLInputElement>(
      "#workflow-file-input",
    )!;
    fireEvent.change(input, { target: { files: [file] } });

    await waitFor(() =>
      expect(
        (screen.getByLabelText("Workflow source") as HTMLTextAreaElement)
          .value,
      ).toBe(VALID_YAML_SOURCE),
    );
    expect(
      (document.querySelector("#workflow-format-select") as HTMLSelectElement)
        .value,
    ).toBe("yaml");
  });

  it("infers json format from a .json upload", async () => {
    renderView();
    const jsonSource = '{"spec":{"entry":"a","nodes":{},"edges":[]}}';
    const file = new File([jsonSource], "workflow.json", {
      type: "application/json",
    });
    const input = document.querySelector<HTMLInputElement>(
      "#workflow-file-input",
    )!;
    fireEvent.change(input, { target: { files: [file] } });

    await waitFor(() =>
      expect(
        (document.querySelector("#workflow-format-select") as HTMLSelectElement)
          .value,
      ).toBe("json"),
    );
  });
});

describe("AuthorWorkflow: agent-state", () => {
  it("registers ready status and the authoring step as the flow progresses", async () => {
    mockValidateWorkflow.mockResolvedValue(VALID_VALIDATION);
    mockPublishWorkflow.mockResolvedValue(PUBLISHED_VERSION);
    renderView();
    await waitFor(() => expect(getAgentState().status).toBe("ready"));
    expect(getAgentState().authoring?.step).toBe("editing");

    paste(VALID_YAML_SOURCE);
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    await waitFor(() => expect(getAgentState().authoring?.step).toBe("valid"));
    expect(getAgentState().authoring?.diagnostics_count).toBe(0);
    expect(getAgentState().authoring?.valid).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: "Publish" }));
    await waitFor(() =>
      expect(getAgentState().authoring?.step).toBe("published"),
    );
    expect(getAgentState().authoring?.digest).toBe(AUTHORING_DIGEST);
  });

  it("registers the invalid step and diagnostics count for an invalid document", async () => {
    mockValidateWorkflow.mockResolvedValue(INVALID_VALIDATION);
    renderView();
    paste(INVALID_YAML_SOURCE);
    fireEvent.click(screen.getByRole("button", { name: "Validate" }));
    await waitFor(() =>
      expect(getAgentState().authoring?.step).toBe("invalid"),
    );
    expect(getAgentState().authoring?.valid).toBe(false);
    expect(getAgentState().authoring?.diagnostics_count).toBe(1);
  });
});
