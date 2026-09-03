import { useState } from "react";
import { Link } from "react-router-dom";
import {
  ApiError,
  createWorkflowGeneration,
  getWorkflowGeneration,
} from "../api/client";
import type { WorkflowGeneration } from "../api/types";
import ErrorNotice from "../components/ErrorNotice";

export default function GenerateWorkflow() {
  const [description, setDescription] = useState("");
  const [actorRef, setActorRef] = useState("");
  const [baseDigest, setBaseDigest] = useState("");
  const [proposal, setProposal] = useState<WorkflowGeneration | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);

  const fail = (cause: unknown) =>
    setError(
      cause instanceof ApiError
        ? cause
        : new ApiError(0, String(cause), "try again"),
    );

  const generate = async () => {
    setBusy(true);
    setError(null);
    try {
      setProposal(
        await createWorkflowGeneration({
          description,
          actor_ref: actorRef,
          base_digest: baseDigest || undefined,
        }),
      );
    } catch (cause) {
      fail(cause);
    } finally {
      setBusy(false);
    }
  };
  const refresh = async () => {
    if (!proposal) return;
    setBusy(true);
    setError(null);
    try {
      setProposal(await getWorkflowGeneration(proposal.run_id));
    } catch (cause) {
      fail(cause);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="view-rail author-workflow" id="generate-workflow-view">
      <h1>Generate workflow</h1>
      <p className="muted">
        A registered fleet agent proposes source. The proposal stays visibly
        proposed until a human confirms it; this page never publishes.
      </p>
      {error ? <ErrorNotice error={error} /> : null}
      {/* One `field` per control — label above, shared `control` classes on
          the input itself (task t27). These were native unstyled inputs with
          their labels floating loose beside them, which read as an unfinished
          form next to the authoring page's. */}
      <div className="author-workflow__source">
        <div className="field">
          <label className="field__label" htmlFor="generation-description">
            Plain-text description
          </label>
          <textarea
            id="generation-description"
            className="control control--textarea author-workflow__textarea"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </div>
        <div className="field">
          <label className="field__label" htmlFor="generation-actor">
            Registered actor ref
          </label>
          <input
            id="generation-actor"
            className="control control--input"
            value={actorRef}
            onChange={(e) => setActorRef(e.target.value)}
            placeholder="actor://company/planner@sha256:…"
          />
        </div>
        <div className="field">
          <label className="field__label" htmlFor="generation-base">
            Pinned base digest (required for edits)
          </label>
          <input
            id="generation-base"
            className="control control--input"
            value={baseDigest}
            onChange={(e) => setBaseDigest(e.target.value)}
            placeholder="sha256:…"
          />
        </div>
        <div className="author-workflow__actions">
          <button
            type="button"
            className="control control--button control--button-primary"
            disabled={busy || !description.trim() || !actorRef.trim()}
            onClick={generate}
          >
            {busy ? "Dispatching…" : "Generate proposal"}
          </button>
          {proposal ? (
            <button
              type="button"
              className="control control--button"
              disabled={busy}
              onClick={refresh}
            >
              Refresh
            </button>
          ) : null}
        </div>
      </div>
      {proposal ? (
        <section id="workflow-generation-result" data-status={proposal.status}>
          <h2>Generation</h2>
          <p><strong>Status:</strong> {proposal.status}</p>
          <p><strong>Run:</strong> <Link to={`/runs/${proposal.run_id}`}>{proposal.run_id}</Link></p>
          {proposal.source ? (
            <>
              <p>{proposal.valid ? "Compiles with 0 errors." : "The proposal does not compile."}</p>
              <label className="field__label" htmlFor="generation-proposed-source">
                Proposed source
              </label>
              <textarea
                id="generation-proposed-source"
                readOnly
                className="control control--textarea author-workflow__textarea"
                value={proposal.source}
              />
            </>
          ) : <p className="muted">The agent is still working.</p>}
          {proposal.diff ? <><h3>Diff against {proposal.base_digest}</h3><pre>{proposal.diff}</pre></> : null}
          {proposal.status === "confirmed" && proposal.valid ? (
            <p><Link to="/design/new">Open the validate and publish door</Link> after copying the exact source above.</p>
          ) : null}
        </section>
      ) : null}
    </section>
  );
}
