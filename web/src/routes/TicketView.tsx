import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { Link, useParams } from "react-router-dom";
import {
  ApiError,
  decideHumanTask,
  getTicket,
  postTicketReply,
  postTicketReviews,
} from "../api/client";
import type {
  PendingDecisionRun,
  TicketFrameData,
  TicketPendingTask,
  TicketProjection,
  TicketReviewRun,
  TicketReviewRunResult,
} from "../api/types";
import ErrorNotice from "../components/ErrorNotice";
import { SignedInAs } from "../components/IdentityGate";
import OutcomeButtons from "../components/OutcomeButtons";
import TicketFlowRail from "../components/TicketFlowRail";
import { ticketFlow } from "../domain/ticket-flow";
import RunDecisionCard, {
  confirmAllVerdicts,
  recordsWithVerdict,
  type RecordVerdict,
  type RunVerdicts,
} from "../components/RunDecisionCard";
import { useWhoami } from "../hooks/useWhoami";

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
  // Who replies and who decides is the signed-in principal's actor (task
  // t9, spec c8) — one fact from whoami, shown on the page, never typed.
  const whoami = useWhoami();
  const actorId = whoami.status === "bound" ? whoami.actorId : null;
  const [text, setText] = useState("");
  const [questionID, setQuestionID] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<ApiError | null>(null);
  const [sent, setSent] = useState(false);
  // The Decisions section's own state (task t18).
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
  // The ticket's undecided ledger claims, grouped by run and quoted at the
  // version THIS response read (task t14, spec c11). Absent from a control
  // plane older than t14, which is a "nothing to decide here" — not an error.
  const pendingRecords: PendingDecisionRun[] = projection?.pending_records ?? [];

  // Where the ticket is in the loop, derived from this projection alone
  // (domain/ticket-flow.ts). Recomputed only when the projection changes.
  const flow = useMemo(
    () => (projection ? ticketFlow(projection) : null),
    [projection],
  );

  const submissionID = useRef(newSubmissionID());

  /**
   * Re-read ONE run's group after its review conflicted (decision c40).
   *
   * A conflict means the ledger moved under the decider and nothing was
   * written, so the only useful answer is the records at the version they are
   * at NOW. The whole ticket is re-read because that is the only route that
   * serves `pending_records` — but only the named group is swapped in, so a
   * neighbouring run that just committed keeps its result on screen instead of
   * silently vanishing under a refresh.
   */
  async function reloadRecordGroup(runID: string) {
    const fresh = await getTicket(id);
    const group = (fresh.pending_records ?? []).find((item) => item.run_id === runID);
    setProjection((current) => {
      if (!current) return current;
      const groups = (current.pending_records ?? []).flatMap((item) =>
        item.run_id === runID ? (group ? [group] : []) : [item],
      );
      return { ...current, pending_records: groups };
    });
  }

  /**
   * Record one decision (task t18, spec c6). The ledger version submitted is
   * the one the API SERVED with this task — never a fresh read — so if the
   * run moved since this page loaded, the control plane refuses the decision
   * with a 409 and writes nothing. That is the correct outcome: the frame
   * this operator read is no longer the frame they would be deciding.
   */
  async function decide(task: TicketPendingTask, outcome: string) {
    if (!projection || actorId === null) return;
    setDeciding(task.id);
    setDecideError(null);
    try {
      await decideHumanTask(task.id, {
        outcome,
        decider_actor_id: actorId,
        // A task with a decision schema gets a schema-valid payload; one
        // without gets none, rather than an invented empty object.
        response: task.decision_schema_ref ? { outcome } : undefined,
        expected_ledger_version: task.ledger_version,
      });
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

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!projection || frozen || actorId === null || !text.trim()) return;
    setSubmitting(true);
    setSubmitError(null);
    setSent(false);
    try {
      const reply = await postTicketReply(projection.ticket_id, {
        // One id per submission; a retry of the same submission (network
        // error, 500) reuses it, so the server resumes the row instead of
        // minting a second reply and a second engine fact.
        id: submissionID.current,
        replier: actorId,
        text: text.trim(),
        ...(questionID ? { question_id: questionID } : {}),
      });
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
  if (!projection || !flow) return <section className="view-rail"><h1>Ticket {id}</h1><p>Loading ticket projection…</p></section>;

  return (
    <section className="view-rail ticket-view" data-ticket-id={projection.ticket_id}>
      <header className="ticket-view__head">
        <div>
          <p className="eyebrow">Ticket</p>
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

      {/* Where the ticket is, before anything a person has to read (task
          t17, issue #270). The first screen used to open on a page-title-
          sized key and then three paragraphs of prose; a person arriving
          from a Jira link could not tell whether the thing had even started
          without scrolling. */}
      <TicketFlowRail flow={flow} ticketId={projection.ticket_id} />

      {/* Decisions next, because they are the only thing on this page that
          is waiting on the reader. A Jira comment that names this ticket's
          options links here (task t11); before t18 it linked to a page that
          listed the task and offered nothing to click. A frozen ticket has
          none: t17 ended its runs. */}
      {pendingTasks.length ? (
        <section aria-labelledby="decisions-title" className="ticket-decisions">
          <h2 id="decisions-title">Decisions</h2>
          <SignedInAs verb="Deciding" whoami={whoami} />
          {decideError ? <ErrorNotice error={decideError} /> : null}
          <ul className="ticket-decisions__list">
            {pendingTasks.map((task) => (
              <li className="inbox-card decision-card" key={task.id} data-human-task-id={task.id}>
                <p className="decision-card__kind">{task.kind}</p>
                <p className="decision-card__meta muted">
                  run{" "}
                  <Link to={`/runs/${encodeURIComponent(task.run_id)}`}>{task.run_id}</Link>
                  {task.deadline ? <> · due <time dateTime={task.deadline}>{task.deadline}</time></> : null}
                  {" · "}
                  <code>{task.id}</code>
                </p>
                <OutcomeButtons
                  taskId={task.id}
                  outcomes={task.allowed_outcomes}
                  disabled={actorId === null}
                  busy={deciding === task.id}
                  onChoose={(outcome) => void decide(task, outcome)}
                />
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      {pendingRecords.length ? (
        <TicketClaimReviews
          ticketId={projection.ticket_id}
          groups={pendingRecords}
          actorId={actorId}
          whoami={whoami}
          onReloadGroup={reloadRecordGroup}
        />
      ) : null}

      {pendingTasks.length === 0 && pendingRecords.length === 0 ? (
        <p className="ticket-view__clear" data-testid="ticket-nothing-pending">
          Nothing on this ticket is waiting on a person.
        </p>
      ) : null}

      <TicketDetail
        projection={projection}
        claims={claims}
        questions={questions}
        decisions={decisions}
        decisionByQuestion={decisionByQuestion}
        frozen={frozen}
        whoami={whoami}
        actorId={actorId}
        text={text}
        setText={setText}
        questionID={questionID}
        setQuestionID={setQuestionID}
        submitting={submitting}
        submitError={submitError}
        sent={sent}
        onSubmit={submit}
      />
    </section>
  );
}

/** The three panels a person reads AFTER deciding — never before. */
const TABS = [
  { id: "claims", label: "Claims & questions" },
  { id: "runs", label: "Runs & reports" },
  { id: "thread", label: "Conversation" },
] as const;

type TabId = (typeof TABS)[number]["id"];

/**
 * Everything the ticket page used to stack under the decision, behind a tab
 * strip (task t17, issue #270).
 *
 * The content did not change and nothing was retired: frame claims are still
 * read-only with their arrival state, runs and reports still link out, the
 * reply thread and its form are still the whole conversation. What changed is
 * that a person meets one of the three at a time instead of all three at
 * once — the operator's "blocks of text and lists" was a description of the
 * stack, not of any one section.
 *
 * `claims` is the default panel because it is the ticket's subject matter;
 * a person who came to decide has already been offered the decision above,
 * and a person who came to read starts at what the ticket is about.
 */
function TicketDetail({
  projection,
  claims,
  questions,
  decisions,
  decisionByQuestion,
  frozen,
  whoami,
  actorId,
  text,
  setText,
  questionID,
  setQuestionID,
  submitting,
  submitError,
  sent,
  onSubmit,
}: {
  projection: TicketProjection;
  claims: NonNullable<TicketFrameData["claims"]>;
  questions: TicketQuestion[];
  decisions: TicketDecision[];
  decisionByQuestion: Map<string | undefined, TicketDecision>;
  frozen: boolean;
  whoami: ReturnType<typeof useWhoami>;
  actorId: string | null;
  text: string;
  setText: (value: string) => void;
  questionID: string;
  setQuestionID: (value: string) => void;
  submitting: boolean;
  submitError: ApiError | null;
  sent: boolean;
  onSubmit: (event: FormEvent) => void;
}) {
  const [tab, setTab] = useState<TabId>("claims");

  // Left/Right moves between tabs and focuses the one it lands on, which is
  // the tablist pattern's whole point: three panels behind one tab stop
  // rather than three more stops on the way to the reply box.
  function onKeyDown(event: React.KeyboardEvent<HTMLDivElement>) {
    if (event.key !== "ArrowRight" && event.key !== "ArrowLeft") return;
    event.preventDefault();
    const index = TABS.findIndex((entry) => entry.id === tab);
    const delta = event.key === "ArrowRight" ? 1 : -1;
    const next = TABS[(index + delta + TABS.length) % TABS.length];
    setTab(next.id);
    document.getElementById(`ticket-tab-${next.id}`)?.focus();
  }

  return (
    <section className="ticket-detail" aria-labelledby="ticket-detail-title">
      <h2 className="sr-only" id="ticket-detail-title">
        Ticket detail
      </h2>
      <div
        className="ticket-detail__tabs"
        role="tablist"
        aria-label="Ticket detail"
        onKeyDown={onKeyDown}
      >
        {TABS.map((entry) => (
          <button
            key={entry.id}
            type="button"
            role="tab"
            id={`ticket-tab-${entry.id}`}
            aria-selected={tab === entry.id}
            aria-controls={`ticket-panel-${entry.id}`}
            tabIndex={tab === entry.id ? 0 : -1}
            className={`ticket-detail__tab${tab === entry.id ? " is-active" : ""}`}
            onClick={() => setTab(entry.id)}
          >
            {entry.label}
          </button>
        ))}
      </div>

      <div
        className="ticket-detail__panel"
        role="tabpanel"
        id={`ticket-panel-${tab}`}
        aria-labelledby={`ticket-tab-${tab}`}
        tabIndex={0}
      >
        {tab === "claims" ? (
          <>
            {/* Frame claims are the custody checkout's, not this page's (spec
                c13, honesty condition h20): internal/devague.MapFrameClaims
                still has no production caller and the live path is an opaque
                frame_json blob, so the page states each claim and the
                confirmation state it arrived with, and offers nothing to
                change it. The claims this page DOES decide are the ledger
                records above. */}
            <section aria-labelledby="claims-title" id="ticket-frame-claims">
              <h2 id="claims-title">Frame claims</h2>
              <p className="muted">
                Read-only: a frame claim is confirmed in the custody checkout, not
                here. What this page decides is the ledger records above.
              </p>
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
          </>
        ) : null}

        {tab === "runs" ? (
          <section aria-labelledby="runs-title">
            <h2 id="runs-title">Runs and reports</h2>
            {projection.runs.length ? <ul className="ticket-rows">{projection.runs.map((run) => <li key={run.id}><Link to={`/runs/${encodeURIComponent(run.id)}`}>{run.name ?? run.id}</Link><StateLabel state={run.state} /></li>)}</ul> : <p className="muted">No runs for this ticket.</p>}
            {projection.ticket_reports.length ? <ul className="ticket-rows">{projection.ticket_reports.map((report) => <li key={report.id}><span>{report.phase} report · {report.run_id}</span><StateLabel state={report.status} /></li>)}</ul> : null}
          </section>
        ) : null}

        {tab === "thread" ? (
          <>
            <section aria-labelledby="replies-title">
              <h2 id="replies-title">Reply thread</h2>
              {projection.replies.length ? <ol className="ticket-replies">{projection.replies.map((reply) => <li key={reply.id}><p>{reply.text}</p><small>{reply.replier}{reply.question_id ? ` · ${reply.question_id}` : ""} · <time dateTime={reply.created_at}>{reply.created_at}</time></small></li>)}</ol> : <p className="muted">No replies yet.</p>}
            </section>

            <form className="ticket-reply-form" onSubmit={onSubmit} aria-labelledby="reply-title">
              <h2 id="reply-title">Reply</h2>
              <SignedInAs verb="Replying" whoami={whoami} />
              <label htmlFor="ticket-question">Question (optional)</label>
              <select id="ticket-question" value={questionID} onChange={(event) => setQuestionID(event.target.value)} disabled={frozen}>
                <option value="">General reply</option>
                {questions.filter((question) => question.id).map((question) => <option key={question.id} value={question.id}>{question.id}: {question.text ?? question.question}</option>)}
              </select>
              <label htmlFor="ticket-reply">Reply text</label>
              <textarea id="ticket-reply" rows={5} value={text} onChange={(event) => setText(event.target.value)} required disabled={frozen} />
              <button type="submit" disabled={frozen || submitting || actorId === null || !text.trim()}>{submitting ? "Sending…" : "Send reply"}</button>
              {sent ? <p role="status" className="ticket-reply-form__success">Reply sent.</p> : null}
              {submitError ? <p role="alert">{submitError.message}. {submitError.remediation}</p> : null}
            </form>
          </>
        ) : null}
      </div>
    </section>
  );
}

/**
 * The ticket's claim-deciding half (task t12, spec c11, decision c40).
 *
 * `pending_records` arrives grouped by run because that is how the ledger
 * decides: a review is opened against ONE run at ONE stated version
 * (PRD §10.8). A person deciding a ticket should not have to know that, so
 * this renders one rationale and one submit over every group, and
 * `POST /v1alpha1/tickets/{id}/reviews` commits one review per run in the
 * order sent.
 *
 * Three properties it refuses to fudge, all of them the /decisions view's:
 *
 *   - The rationale is required by the form, as it is by the API. A
 *     confirmation with no stated reason cannot be told apart from an unread
 *     one.
 *   - The reviewer is the signed-in principal's actor, never typed (task t9).
 *   - The version submitted for a group is the one the API SERVED that group
 *     at, never a fresh read. If the run moved since this page rendered, the
 *     control plane reports `conflict` for it, writes nothing, and every other
 *     run still commits — so partial success is reported per run rather than
 *     collapsed into one banner, and only the conflicted group offers a
 *     reload.
 *
 * And the property the whole surface exists to state: a review NAMES records.
 * A confirmed claim still reads `proposed` afterwards, with the review beside
 * it, which is what the committed groups below render.
 */
function TicketClaimReviews({
  ticketId,
  groups,
  actorId,
  whoami,
  onReloadGroup,
}: {
  ticketId: string;
  groups: PendingDecisionRun[];
  /** The signed-in principal's actor, or null when nothing can be recorded. */
  actorId: string | null;
  whoami: ReturnType<typeof useWhoami>;
  onReloadGroup: (runID: string) => Promise<void>;
}) {
  // Keyed by record id rather than by run, so a group reloaded at a new
  // version keeps the verdicts already chosen for the records that survived
  // it, and a record that is new to the page starts at the default.
  const [chosen, setChosen] = useState<RunVerdicts>({});
  const [rationale, setRationale] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<ApiError | null>(null);
  // What was sent and what came back, paired: the API answers one result per
  // submitted run "in the order submitted", so the pairing is by index. It is
  // kept because the response names a run, not the records — and marking a
  // record as decided when this page cannot say it was in a committed review
  // would be the page inventing a fact.
  const [sent, setSent] = useState<TicketReviewRun[]>([]);
  const [results, setResults] = useState<TicketReviewRunResult[]>([]);
  const [reloading, setReloading] = useState<string | null>(null);

  const verdicts: RunVerdicts = Object.assign(
    {},
    ...groups.map((group) => confirmAllVerdicts(group)),
    chosen,
  );

  /** The records this page can honestly say a committed review named. */
  function reviewedIn(runID: string): string[] {
    return results.flatMap((result, index) =>
      result.status === "committed" && sent[index]?.run_id === runID
        ? sent[index].records
        : [],
    );
  }

  // What a submit would send: every record still carrying a verdict and NOT
  // already named by a committed review. Leaving the decided ones in would
  // re-submit them at a version the commit itself moved past, so a second
  // click on a partly-committed ticket would manufacture a conflict on work
  // that already landed.
  const decided = new Set(groups.flatMap((group) => reviewedIn(group.run_id)));
  const batch: TicketReviewRun[] = groups.flatMap((group) =>
    (["confirm", "reject"] as const).flatMap((verdict) => {
      const records = recordsWithVerdict(group, verdicts, verdict).filter(
        (id) => !decided.has(id),
      );
      if (records.length === 0) return [];
      return [{
        run_id: group.run_id,
        expected_ledger_version: group.ledger_version,
        records,
        verdict: verdict === "confirm" ? ("confirmed" as const) : ("rejected" as const),
      }];
    }),
  );
  // A run whose records need both answers is two reviews at one version, and
  // the ledger will only take the first — so the page says that BEFORE the
  // click rather than letting it arrive as a surprise conflict.
  const splitRuns = groups
    .filter((group) =>
      batch.filter((entry) => entry.run_id === group.run_id).length > 1,
    )
    .map((group) => group.run_id);

  const canSubmit =
    actorId !== null && !submitting && batch.length > 0 && rationale.trim() !== "";

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!canSubmit || actorId === null) return;
    setSubmitError(null);
    setSubmitting(true);
    try {
      const result = await postTicketReviews(ticketId, {
        runs: batch,
        rationale: rationale.trim(),
        reviewer_actor_id: actorId,
      });
      // Appended, not replaced: a ticket decided in two passes (one run
      // conflicted, was reloaded, and was recorded again) must keep the first
      // pass's committed outcomes on screen.
      setSent((current) => [...current, ...batch]);
      setResults((current) => [...current, ...result.runs]);
    } catch (cause) {
      setSubmitError(
        cause instanceof ApiError
          ? cause
          : new ApiError(0, String(cause), "check the browser console"),
      );
    } finally {
      setSubmitting(false);
    }
  }

  async function reload(runID: string) {
    setReloading(runID);
    setSubmitError(null);
    try {
      await onReloadGroup(runID);
      // The results for THIS run go with the version they were measured
      // against; every other run's outcome stands.
      setResults((current) => current.filter((_, index) => sent[index]?.run_id !== runID));
      setSent((current) => current.filter((entry) => entry.run_id !== runID));
    } catch (cause) {
      setSubmitError(
        cause instanceof ApiError
          ? cause
          : new ApiError(0, String(cause), "check the browser console"),
      );
    } finally {
      setReloading(null);
    }
  }

  return (
    <section aria-labelledby="ticket-claims-title" className="ticket-claim-reviews">
      <h2 id="ticket-claims-title">Claims awaiting a decision</h2>
      <p className="muted">
        An agent saying it is done is a completion claim, not evidence. Say
        what you found for each record below; the decision is recorded as its
        own ledger record naming who decided and why, and the record it names
        does not change.
      </p>
      <SignedInAs verb="Deciding" whoami={whoami} />

      <ul className="decisions-list" id="ticket-pending-records">
        {groups.map((group) => {
          const runResults = results.filter(
            (_, index) => sent[index]?.run_id === group.run_id,
          );
          return (
            <RunDecisionCard
              key={group.run_id}
              group={group}
              verdicts={verdicts}
              onVerdictChange={(recordId: string, verdict: RecordVerdict) =>
                setChosen((current) => ({ ...current, [recordId]: verdict }))
              }
              disabled={actorId === null || submitting}
              reviewedRecordIds={reviewedIn(group.run_id)}
            >
              {splitRuns.includes(group.run_id) ? (
                <p className="muted">
                  This run has both confirmations and rejections. A run is
                  reviewed at one ledger version, so the confirmations commit
                  first and the rejections come back as a conflict — reload
                  this group and record them again.
                </p>
              ) : null}
              {runResults.length ? (
                <div
                  className="inbox-card__result"
                  data-testid={`review-result-${group.run_id}`}
                  role="status"
                >
                  {runResults.map((result, index) => (
                    <p key={`${result.run_id}-${index}`}>
                      {result.status === "committed" ? (
                        <>
                          decision recorded — review <code>{result.review_id}</code>;
                          this run&apos;s ledger is now at version{" "}
                          {result.ledger_version}. The records decided are
                          unchanged: a review names them, it never rewrites
                          them.
                        </>
                      ) : (
                        <>
                          {result.status} — {result.message ?? "nothing was written"}
                        </>
                      )}
                    </p>
                  ))}
                  {runResults.some((result) => result.status === "conflict") ? (
                    <button
                      type="button"
                      className="author-workflow__button"
                      disabled={reloading === group.run_id}
                      onClick={() => void reload(group.run_id)}
                    >
                      {reloading === group.run_id
                        ? "Reloading this group…"
                        : "Reload this group"}
                    </button>
                  ) : null}
                </div>
              ) : null}
            </RunDecisionCard>
          );
        })}
      </ul>

      <form className="inbox-card__form" onSubmit={submit}>
        <div className="inbox-card__field">
          <label htmlFor="ticket-review-rationale">
            Why (recorded on every decision)
          </label>
          <textarea
            id="ticket-review-rationale"
            rows={2}
            value={rationale}
            onChange={(event) => setRationale(event.target.value)}
          />
        </div>
        {submitError ? <ErrorNotice error={submitError} /> : null}
        <button
          type="submit"
          className="author-workflow__button author-workflow__button--primary"
          disabled={!canSubmit}
        >
          {submitting ? "Recording…" : "Record decisions"}
        </button>
      </form>
    </section>
  );
}

export default TicketView;
