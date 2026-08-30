import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { Link, useParams } from "react-router-dom";
import { ApiError, decideHumanTask, getTicket, postTicketReply } from "../api/client";
import {
  clearDecisionToken,
  getDecisionToken,
  setDecisionActorID,
  setDecisionToken,
} from "../api/decision-token";
import type { TicketFrameData, TicketPendingTask, TicketProjection } from "../api/types";
import DeciderActorField, { useDeciderActorID } from "../components/DeciderActorField";
import ErrorNotice from "../components/ErrorNotice";
import OutcomeButtons from "../components/OutcomeButtons";

function newSubmissionID(): string {
  const raw = typeof crypto !== "undefined" && "randomUUID" in crypto ? crypto.randomUUID() : `${Date.now()}-${Math.random()}`;
  return raw.replace(/[^A-Za-z0-9_-]/g, "").slice(0, 64);
}

type TicketQuestion = NonNullable<TicketFrameData["questions"]>[number];
type TicketDecision = NonNullable<TicketFrameData["decisions"]>[number];

const STATE_ICONS: Record<string, string> = {
  confirmed: "✓",
  proposed: "?",
  rejected: "×",
  observed: "●",
  derived: "◇",
  superseded: "↷",
  open: "?",
  answered: "✓",
};

function display(value: unknown): string {
  if (typeof value === "string") return value;
  if (value === undefined || value === null) return "";
  return JSON.stringify(value);
}

function StateLabel({ state }: { state?: string }) {
  const word = state?.trim() || "unknown";
  return (
    <span className={`ticket-state ticket-state--${word}`} data-state={word}>
      <span aria-hidden="true">{STATE_ICONS[word] ?? "○"}</span>
      <span>{word}</span>
    </span>
  );
}

function mergedPR(frame: TicketFrameData): { href?: string; label: string } | null {
  if (!frame.merged_pr) return null;
  if (typeof frame.merged_pr === "string") {
    return { href: frame.merged_pr, label: frame.merged_pr };
  }
  const label = frame.merged_pr.title
    ?? (frame.merged_pr.number ? `PR #${frame.merged_pr.number}` : "merged PR");
  return { href: frame.merged_pr.url, label };
}

export function TicketView() {
  const { id = "" } = useParams();
  const [projection, setProjection] = useState<TicketProjection | null>(null);
  const [loadError, setLoadError] = useState<ApiError | null>(null);
  const [token, setToken] = useState(() => getDecisionToken() ?? "");
  const [replier, setReplier] = useState("");
  const [text, setText] = useState("");
  const [questionID, setQuestionID] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<ApiError | null>(null);
  const [sent, setSent] = useState(false);
  // The Decisions section's own state (task t18). The decider id is
  // remembered across sittings, the token is not — see api/decision-token.ts
  // for why those two are stored differently.
  const [decider, setDecider] = useDeciderActorID();
  const [deciding, setDeciding] = useState<string | null>(null);
  const [decideError, setDecideError] = useState<ApiError | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    setProjection(null);
    setLoadError(null);
    getTicket(id, controller.signal)
      .then(setProjection)
      .catch((error: ApiError) => {
        if (!controller.signal.aborted) setLoadError(error);
      });
    return () => controller.abort();
  }, [id]);

  const frame = projection?.latest_frame?.frame ?? {};
  const ticketURL = projection?.ticket_url
    ?? projection?.jira_url
    ?? (typeof frame.ticket_url === "string" ? frame.ticket_url : undefined)
    ?? (typeof frame.jira_url === "string" ? frame.jira_url : undefined);
  const frozen = projection?.frozen === true || frame.frozen === true;
  // The freeze banner sentence is composed by the API (TicketFreeze.banner)
  // rather than assembled here, so the run counts a human reads on this page
  // are the counts the API test asserts against the runs themselves (spec
  // h19). Absent on a ticket frozen before this shipped.
  const freeze = projection?.freeze;
  const pr = mergedPR({ ...frame, merged_pr: projection?.merged_pr ?? frame.merged_pr });
  const claims = frame.claims ?? [];
  const questions: TicketQuestion[] = frame.questions ?? claims
    .filter((claim) => claim.kind === "open_question")
    .map((claim): TicketQuestion => ({ id: claim.id, text: claim.text, state: claim.state, status: claim.status }));
  const decisions: TicketDecision[] = frame.decisions ?? claims
    .filter((claim) => claim.kind === "decision")
    .map((claim): TicketDecision => ({ id: claim.id, text: claim.text, state: claim.state, status: claim.status }));
  const decisionByQuestion = useMemo(() => new Map(
    decisions.filter((item) => item.question_id).map((item) => [item.question_id, item]),
  ), [decisions]);

  const pendingTasks: TicketPendingTask[] = projection?.pending_tasks ?? [];

  const submissionID = useRef(newSubmissionID());

  /**
   * Record one decision (task t18, spec c6). The ledger version submitted is
   * the one the API SERVED with this task — never a fresh read — so if the
   * run moved since this page loaded, the control plane refuses the decision
   * with a 409 and writes nothing. That is the correct outcome: the frame
   * this operator read is no longer the frame they would be deciding.
   */
  async function decide(task: TicketPendingTask, outcome: string) {
    if (!projection || !token || !decider.trim()) return;
    // Remember the TRIMMED id: what is stored is what was submitted.
    setDecisionActorID(decider.trim());
    setDeciding(task.id);
    setDecideError(null);
    try {
      await decideHumanTask(task.id, {
        outcome,
        decider_actor_id: decider.trim(),
        // A task with a decision schema gets a schema-valid payload; one
        // without gets none, rather than an invented empty object.
        response: task.decision_schema_ref ? { outcome } : undefined,
        expected_ledger_version: task.ledger_version,
      }, token);
      setProjection((current) => current && ({
        ...current,
        pending_tasks: (current.pending_tasks ?? []).filter((item) => item.id !== task.id),
      }));
    } catch (error) {
      setDecideError(error as ApiError);
    } finally {
      setDeciding(null);
    }
  }

  function holdToken(next: string) {
    setToken(next);
    if (next) setDecisionToken(next);
    else clearDecisionToken();
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!projection || frozen || !token || !replier.trim() || !text.trim()) return;
    setSubmitting(true);
    setSubmitError(null);
    setSent(false);
    try {
      const reply = await postTicketReply(projection.ticket_id, {
        // One id per submission; a retry of the same submission (network
        // error, 500) reuses it, so the server resumes the row instead of
        // minting a second reply and a second engine fact.
        id: submissionID.current,
        replier: replier.trim(),
        text: text.trim(),
        ...(questionID ? { question_id: questionID } : {}),
      }, token);
      setProjection({ ...projection, replies: [...projection.replies, reply] });
      setText("");
      submissionID.current = newSubmissionID();
      setQuestionID("");
      setSent(true);
    } catch (error) {
      setSubmitError(error as ApiError);
    } finally {
      setSubmitting(false);
    }
  }

  if (loadError) {
    return <section className="view-rail"><h1>Ticket {id}</h1><p role="alert">{loadError.message}. {loadError.remediation}</p></section>;
  }
  if (!projection) return <section className="view-rail"><h1>Ticket {id}</h1><p>Loading ticket projection…</p></section>;

  return (
    <section className="view-rail ticket-view" data-ticket-id={projection.ticket_id}>
      <header className="ticket-view__head">
        <div>
          <p className="eyebrow">Ticket conversation</p>
          <h1>{projection.ticket_id}</h1>
        </div>
        {ticketURL ? <a id="ticket-jira-link" href={ticketURL}>Open in Jira</a> : null}
      </header>

      {frozen ? (
        <div className="ticket-freeze" role="status">
          <strong>Frozen — this ticket was merged.</strong>{" "}
          {freeze ? <span className="ticket-freeze__runs">{freeze.banner}</span> : null}{" "}
          {pr?.href ? <a href={pr.href}>{pr.label}</a> : pr ? pr.label : "Replies are closed."}
        </div>
      ) : null}

      {/* Decisions first, because they are the only thing on this page that
          is waiting on the reader. A Jira comment that names this ticket's
          options links here (task t11); before t18 it linked to a page that
          listed the task and offered nothing to click. A frozen ticket has
          none: t17 ended its runs. */}
      {pendingTasks.length ? (
        <section aria-labelledby="decisions-title" className="ticket-decisions">
          <h2 id="decisions-title">Decisions</h2>
          <p className="muted">
            Each option below is one the engine will accept for that task. Hold
            the decision token in the reply form and name yourself here to
            enable them — the token authenticates the deployment, so who
            decided is a separate, explicit claim.
          </p>
          <DeciderActorField id="ticket-decider-actor" value={decider} onChange={setDecider} />
          {decideError ? <ErrorNotice error={decideError} /> : null}
          <ul className="ticket-decisions__list">
            {pendingTasks.map((task) => (
              <li className="inbox-card" key={task.id} data-human-task-id={task.id}>
                <code>{task.id}</code> · {task.kind} · run{" "}
                <Link to={`/runs/${encodeURIComponent(task.run_id)}`}>{task.run_id}</Link>
                {task.deadline ? <> · due <time dateTime={task.deadline}>{task.deadline}</time></> : null}
                <OutcomeButtons
                  taskId={task.id}
                  outcomes={task.allowed_outcomes}
                  disabled={!token || !decider.trim()}
                  busy={deciding === task.id}
                  onChoose={(outcome) => void decide(task, outcome)}
                />
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      <section aria-labelledby="claims-title">
        <h2 id="claims-title">Claims</h2>
        {claims.length ? <ol className="ticket-cards">{claims.map((claim, index) => {
          const key = claim.id ?? claim.ref ?? String(index);
          return <li key={key} data-claim-id={key}><div><strong>{claim.id ?? claim.ref ?? `Claim ${index + 1}`}</strong><StateLabel state={claim.state ?? claim.status} /></div><p>{claim.text ?? claim.title ?? claim.claim ?? "No claim text supplied"}</p></li>;
        })}</ol> : <p className="muted">No frame claims have been posted.</p>}
      </section>

      <section aria-labelledby="questions-title">
        <h2 id="questions-title">Questions and decisions</h2>
        {questions.length ? <ol className="ticket-cards">{questions.map((question, index) => {
          const key = question.id ?? String(index);
          const decision = question.id ? decisionByQuestion.get(question.id) : undefined;
          return <li key={key}><div><strong>{question.id ?? `Question ${index + 1}`}</strong><StateLabel state={question.state ?? question.status} /></div><p>{question.text ?? question.question ?? "No question text supplied"}</p>{question.answer !== undefined ? <p><b>Answer:</b> {display(question.answer)}</p> : null}{decision ? <p><b>Decision:</b> {decision.text ?? decision.decision ?? decision.outcome}</p> : null}</li>;
        })}</ol> : <p className="muted">No questions have been posted.</p>}
        {decisions.filter((item) => !item.question_id).map((decision, index) => <p key={decision.id ?? index}><b>Decision:</b> {decision.text ?? decision.decision ?? decision.outcome}</p>)}
      </section>

      <section aria-labelledby="runs-title">
        <h2 id="runs-title">Runs and reports</h2>
        {projection.runs.length ? <ul className="ticket-rows">{projection.runs.map((run) => <li key={run.id}><Link to={`/runs/${encodeURIComponent(run.id)}`}>{run.name ?? run.id}</Link><StateLabel state={run.state} /></li>)}</ul> : <p className="muted">No runs for this ticket.</p>}
        {projection.ticket_reports.length ? <ul className="ticket-rows">{projection.ticket_reports.map((report) => <li key={report.id}><span>{report.phase} report · {report.run_id}</span><StateLabel state={report.status} /></li>)}</ul> : null}
      </section>

      <section aria-labelledby="replies-title">
        <h2 id="replies-title">Reply thread</h2>
        {projection.replies.length ? <ol className="ticket-replies">{projection.replies.map((reply) => <li key={reply.id}><p>{reply.text}</p><small>{reply.replier}{reply.question_id ? ` · ${reply.question_id}` : ""} · <time dateTime={reply.created_at}>{reply.created_at}</time></small></li>)}</ol> : <p className="muted">No replies yet.</p>}
      </section>

      <form className="ticket-reply-form" onSubmit={submit} aria-labelledby="reply-title">
        <h2 id="reply-title">Reply</h2>
        <p className="muted">The decision token stays in this tab’s session storage.</p>
        <label htmlFor="ticket-token">Decision token</label>
        <input id="ticket-token" type="password" autoComplete="off" value={token} onChange={(event) => holdToken(event.target.value)} disabled={frozen} />
        <label htmlFor="ticket-replier">Your name</label>
        <input id="ticket-replier" value={replier} onChange={(event) => setReplier(event.target.value)} required disabled={frozen} />
        <label htmlFor="ticket-question">Question (optional)</label>
        <select id="ticket-question" value={questionID} onChange={(event) => setQuestionID(event.target.value)} disabled={frozen}>
          <option value="">General reply</option>
          {questions.filter((question) => question.id).map((question) => <option key={question.id} value={question.id}>{question.id}: {question.text ?? question.question}</option>)}
        </select>
        <label htmlFor="ticket-reply">Reply text</label>
        <textarea id="ticket-reply" rows={5} value={text} onChange={(event) => setText(event.target.value)} required disabled={frozen} />
        <button type="submit" disabled={frozen || submitting || !token || !replier.trim() || !text.trim()}>{submitting ? "Sending…" : "Send reply"}</button>
        {sent ? <p role="status" className="ticket-reply-form__success">Reply sent.</p> : null}
        {submitError ? <p role="alert">{submitError.message}. {submitError.remediation}</p> : null}
      </form>
    </section>
  );
}

export default TicketView;
