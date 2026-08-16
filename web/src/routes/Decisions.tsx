import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { setAgentState } from "../agent-state/store";
import {
  ApiError,
  commitReview,
  createReview,
  listPendingDecisions,
} from "../api/client";
import {
  clearDecisionToken,
  getDecisionToken,
  setDecisionToken,
} from "../api/decision-token";
import type { PendingDecisionRun, ReviewCommitResult } from "../api/types";
import AuthorityChip from "../components/AuthorityChip";
import ErrorNotice from "../components/ErrorNotice";
import { useSharedEvents, type SharedEventType } from "../hooks/useSharedEvents";

/**
 * Events that can change what is awaiting a decision: a new record appended,
 * or a review just committed (possibly by another operator). A stable
 * module-level reference, as useSharedEvents requires (issue #46).
 */
const DECISION_EVENT_TYPES = [
  "dev.culture.nodes.ledger.record-appended",
  "dev.culture.nodes.ledger.review-committed",
] as const satisfies readonly SharedEventType[];

/** Mirrors the Inbox/Mesh refresh discipline. */
const REFRESH_DEBOUNCE_MS = 4000;

/**
 * The Decisions view (`/decisions`, task t30 / issue #99): the affirmative
 * half of PRD §10.4, reachable by a human through the product.
 *
 * The refusal half — an agent may only propose, and no actor promotes its own
 * proposal — has always worked. What did not exist was a way for a person to
 * *make* the decision without hand-writing two authenticated HTTP calls. This
 * view lists every proposed record no review has decided, and lets a human
 * confirm or reject them: create a review over the run's records at the
 * ledger version this page read, then commit it with a verdict and a stated
 * rationale.
 *
 * Three properties this view refuses to fudge:
 *
 *   - The rationale is required by the form, as it is by the API. A
 *     confirmation with no stated reason cannot be told apart from an unread
 *     one.
 *   - The reviewer is typed in, never inferred. The token authenticates the
 *     deployment, not the person; who decided is a separate, explicit claim,
 *     and the API refuses any reviewer the registry does not record as a
 *     human.
 *   - The record payload is rendered in full. A decider must read the claim,
 *     including the qualifying half of it — the same reason the operator's
 *     ledger output stopped truncating.
 *
 * Deciding does not change the records decided: the ledger appends a review
 * record naming each one, and the claim keeps reading `proposed` forever.
 * The list shrinks because the join changed, not because a record did.
 */
export function Decisions() {
  const [groups, setGroups] = useState<PendingDecisionRun[] | null>(null);
  const [recordCount, setRecordCount] = useState(0);
  // Decisions recorded in this sitting, kept at page level rather than on the
  // card that made them. Found by driving this view against a live control
  // plane: a decided run leaves the pending list, so a confirmation rendered
  // inside its card disappears the moment the list refreshes — the operator
  // clicks "Record decision" and the card silently vanishes, which is
  // indistinguishable from the click having done nothing.
  const [recorded, setRecorded] = useState<RecordedDecision[]>([]);
  const [error, setError] = useState<ApiError | null>(null);
  const [tokenHeld, setTokenHeld] = useState(getDecisionToken() !== null);
  const [reloadKey, setReloadKey] = useState(0);
  const reloadTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const lastReload = useRef(0);

  const scheduleReload = useCallback(() => {
    if (reloadTimer.current) return;
    const elapsed = Date.now() - lastReload.current;
    const wait = Math.max(0, REFRESH_DEBOUNCE_MS - elapsed);
    reloadTimer.current = setTimeout(() => {
      reloadTimer.current = undefined;
      lastReload.current = Date.now();
      setReloadKey((key) => key + 1);
    }, wait);
  }, []);

  useEffect(
    () => () => {
      if (reloadTimer.current) clearTimeout(reloadTimer.current);
    },
    [],
  );

  useSharedEvents(DECISION_EVENT_TYPES, scheduleReload);

  useEffect(() => {
    const controller = new AbortController();
    // "ready" must never regress to "loading" on a refresh: only the first
    // render reports loading, every later reload is stale-while-revalidate.
    if (reloadKey === 0) setAgentState({ status: "loading", run: null });
    setError(null);

    listPendingDecisions(controller.signal)
      .then((payload) => {
        if (controller.signal.aborted) return;
        setGroups(payload.items);
        setRecordCount(payload.record_count);
        setAgentState({ status: "ready", run: null });
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        setGroups((prev) => prev ?? []);
        setError(
          cause instanceof ApiError
            ? cause
            : new ApiError(0, String(cause), "check the browser console"),
        );
        setAgentState({ status: "ready", run: null });
      });
    return () => controller.abort();
  }, [reloadKey]);

  const token = tokenHeld ? getDecisionToken() : null;

  return (
    <section className="view-rail decisions-view">
      <h1>Decisions</h1>
      <p className="muted">
        Every proposed record no review has decided. An agent saying it is done
        is a completion claim, not evidence — a decision here is a human&apos;s,
        recorded as its own ledger record naming who decided and why.
      </p>

      <TokenPanel
        held={tokenHeld}
        onHold={(value) => {
          setDecisionToken(value);
          setTokenHeld(true);
        }}
        onClear={() => {
          clearDecisionToken();
          setTokenHeld(false);
        }}
      />

      {error ? <ErrorNotice error={error} /> : null}

      {recorded.length > 0 ? (
        <ul className="decisions-recorded" id="decisions-recorded" role="status">
          {recorded.map((entry) => (
            <li key={entry.reviewId}>
              <strong>{entry.verdict}</strong>ed {entry.recordCount} record(s)
              on run <code>{entry.runId}</code> — review{" "}
              <code>{entry.reviewId}</code>, ledger now at version{" "}
              {entry.ledgerVersion}. The records decided are unchanged: a
              review names them, it never rewrites them.
            </li>
          ))}
        </ul>
      ) : null}

      {groups === null ? (
        <p className="muted" id="decisions-loading">
          Loading pending decisions…
        </p>
      ) : groups.length === 0 ? (
        <p className="muted" id="decisions-empty">
          Nothing is awaiting a decision. Every proposed record has been
          confirmed or rejected.
        </p>
      ) : (
        <>
          <p className="muted" id="decisions-count">
            {recordCount} record(s) awaiting a decision across {groups.length}{" "}
            run(s).
          </p>
          <ul className="decisions-list" id="decisions-runs">
            {groups.map((group) => (
              <RunDecisionCard
                key={group.run_id}
                group={group}
                token={token}
                onDecided={(entry) => {
                  setRecorded((current) => [entry, ...current]);
                  setReloadKey((key) => key + 1);
                }}
              />
            ))}
          </ul>
        </>
      )}
    </section>
  );
}

/**
 * Token entry, indicator, and clear affordance — the same contract the Inbox
 * states (risk r1, api/decision-token.ts): the input is `type="password"`,
 * its draft is dropped the moment the token is held, and the held value is
 * never rendered back into the page.
 */
function TokenPanel({
  held,
  onHold,
  onClear,
}: {
  held: boolean;
  onHold: (token: string) => void;
  onClear: () => void;
}) {
  const [draft, setDraft] = useState("");
  return (
    <div className="inbox-token" id="decisions-token">
      <div className="inbox-token__entry">
        <label htmlFor="decision-token-input">Decision token</label>
        <input
          id="decision-token-input"
          type="password"
          autoComplete="off"
          value={draft}
          onChange={(event) => setDraft(event.target.value)}
        />
        <button
          type="button"
          className="author-workflow__button"
          disabled={draft === ""}
          onClick={() => {
            onHold(draft);
            setDraft("");
          }}
        >
          Hold token
        </button>
        {held ? (
          <button
            type="button"
            className="author-workflow__button"
            onClick={onClear}
          >
            Clear token
          </button>
        ) : null}
      </div>
      <p
        className="inbox-token__state muted"
        id="decision-token-state"
        data-token-held={held}
      >
        {held
          ? "token held — this tab only; cleared when the tab closes"
          : "no token held — decisions are disabled until one is entered"}
      </p>
    </div>
  );
}

/** One recorded decision, kept above the list after its run leaves it. */
interface RecordedDecision {
  reviewId: string;
  runId: string;
  verdict: "confirm" | "reject";
  recordCount: number;
  ledgerVersion: number;
}

/**
 * One run's undecided records and the form that decides them.
 *
 * The whole group is decided with one verdict and one rationale, because that
 * is the shape a review transaction has (PRD §10.8: all of it applies or none
 * of it does). Deciding a subset is deciding a different frame, so it is a
 * second review — the checkbox per record selects which records this review
 * covers.
 */
function RunDecisionCard({
  group,
  token,
  onDecided,
}: {
  group: PendingDecisionRun;
  token: string | null;
  onDecided: (entry: RecordedDecision) => void;
}) {
  const [selected, setSelected] = useState<string[]>(() =>
    group.records.map((record) => record.id),
  );
  const [verdict, setVerdict] = useState<"confirm" | "reject">("confirm");
  const [reviewer, setReviewer] = useState("");
  const [rationale, setRationale] = useState("");
  const [submitError, setSubmitError] = useState<ApiError | null>(null);
  const [result, setResult] = useState<ReviewCommitResult | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const canSubmit =
    token !== null &&
    !submitting &&
    result === null &&
    selected.length > 0 &&
    reviewer.trim() !== "" &&
    rationale.trim() !== "";

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!canSubmit || token === null) return;

    setSubmitError(null);
    setSubmitting(true);
    try {
      // The ledger version submitted is the one this page READ, never a
      // fresh fetch: that is what makes the stale guard meaningful. If the
      // run moved since the list loaded, the API refuses the commit with a
      // 409 and writes nothing, which is the correct outcome — the frame
      // this operator read is no longer the frame they would be deciding.
      const review = await createReview(
        group.run_id,
        {
          record_ids: selected,
          ledger_version: group.ledger_version,
          reviewer_actor_id: reviewer.trim(),
        },
        token,
      );
      const decisions: Record<string, "confirm" | "reject"> = {};
      for (const id of selected) decisions[id] = verdict;

      const committed = await commitReview(
        review.id,
        {
          decisions,
          expected_ledger_version: group.ledger_version,
          rationale: rationale.trim(),
        },
        token,
      );
      setResult(committed);
      onDecided({
        reviewId: committed.review_id,
        runId: group.run_id,
        verdict,
        recordCount: committed.records.length,
        ledgerVersion: committed.ledger_version,
      });
    } catch (cause) {
      setSubmitError(
        cause instanceof ApiError
          ? cause
          : new ApiError(0, String(cause), "check the browser console"),
      );
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <li className="inbox-card decisions-card" data-run-id={group.run_id}>
      <div className="inbox-card__head">
        <AuthorityChip authority="proposed" />
        <code className="inbox-card__id">
          <Link to={`/runs/${group.run_id}`}>{group.run_id}</Link>
        </code>
        <span className="inbox-card__kind">
          ledger version {group.ledger_version}
        </span>
      </div>

      <ul className="decisions-records">
        {group.records.map((record) => (
          <li key={record.id} data-record-id={record.id}>
            <label className="decisions-record__select">
              <input
                type="checkbox"
                checked={selected.includes(record.id)}
                onChange={(event) =>
                  setSelected((current) =>
                    event.target.checked
                      ? [...current, record.id]
                      : current.filter((id) => id !== record.id),
                  )
                }
                aria-label={`Include ${record.id}`}
              />
              <code>{record.id}</code> · {record.record_type} · from{" "}
              {record.origin_actor_id ?? "an unnamed actor"} (
              {record.origin_kind})
            </label>
            {/* The payload in full: a decision on a claim nobody read is the
                failure this whole surface exists to prevent. */}
            <pre className="decisions-record__data">
              {JSON.stringify(record.data, null, 2)}
            </pre>
          </li>
        ))}
      </ul>

      {result === null ? (
        <form className="inbox-card__form" onSubmit={submit}>
          <fieldset className="inbox-card__outcomes">
            <legend>Verdict</legend>
            <label className="inbox-card__outcome">
              <input
                type="radio"
                name={`verdict-${group.run_id}`}
                value="confirm"
                checked={verdict === "confirm"}
                onChange={() => setVerdict("confirm")}
              />
              confirm
            </label>
            <label className="inbox-card__outcome">
              <input
                type="radio"
                name={`verdict-${group.run_id}`}
                value="reject"
                checked={verdict === "reject"}
                onChange={() => setVerdict("reject")}
              />
              reject
            </label>
          </fieldset>
          <div className="inbox-card__field">
            <label htmlFor={`reviewer-${group.run_id}`}>
              Reviewer actor id
            </label>
            <input
              id={`reviewer-${group.run_id}`}
              type="text"
              value={reviewer}
              onChange={(event) => setReviewer(event.target.value)}
            />
          </div>
          <div className="inbox-card__field">
            <label htmlFor={`rationale-${group.run_id}`}>
              Why (recorded on the decision)
            </label>
            <textarea
              id={`rationale-${group.run_id}`}
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
            Record decision
          </button>
        </form>
      ) : (
        <p className="inbox-card__result" role="status">
          decision recorded — {result.records.length} review record(s) appended
          by review <code>{result.review_id}</code>; the run&apos;s ledger is
          now at version {result.ledger_version}. The records decided are
          unchanged: a review names them, it never rewrites them.
        </p>
      )}
    </li>
  );
}

export default Decisions;
