/**
 * The "what and where" half of a human-task card (issue #332, task t43):
 * the question, the facts the decision is about with a link for everything
 * addressable, and — behind a collapsed disclosure — the audit ids and raw
 * context refs that used to be the card's main content.
 *
 * The facts come from `taskContextFacts` (domain/human-task-context.ts), which
 * reads the API's server-side resolution of the task's context refs. There is
 * no markdown renderer in this app, so an agent's summary or reason renders
 * as preformatted text — legible, and never HTML the page did not write.
 *
 * The PR's LIVE state (open / merged / closed) is not shown: the control
 * plane has no GitHub read to ask with until the GitHub App token work
 * (#331). The link is the way to check it.
 */
import type { ReactNode } from "react";
import type { HumanTask, HumanTaskBinding } from "../api/types";
import type { TaskContextFacts } from "../domain/human-task-context";

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <dt>{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}

function External({ href, children }: { href: string; children: ReactNode }) {
  return (
    <a href={href} target="_blank" rel="noopener noreferrer">
      {children}
    </a>
  );
}

/** The question, then the facts. Renders nothing it was not given. */
export function HumanTaskFacts({ facts }: { facts: TaskContextFacts }) {
  const readiness = facts.readiness;
  return (
    <>
      <h3 className="inbox-card__question">{facts.question}</h3>
      <dl className="inbox-card__request inbox-card__facts">
        {facts.prHref && facts.repository && facts.prNumber ? (
          <Fact label="pull request">
            <External href={facts.prHref}>
              {facts.repository}#{facts.prNumber}
            </External>
            {facts.prTitle ? <> — {facts.prTitle}</> : null}
          </Fact>
        ) : null}
        {facts.ticketKey && facts.ticketHref ? (
          <Fact label="ticket">
            <External href={facts.ticketHref}>{facts.ticketKey}</External>
          </Fact>
        ) : null}
        {facts.blockedStep ? (
          <Fact label="blocked step">
            <code>{facts.blockedStep}</code>
          </Fact>
        ) : null}
        {facts.findings.length > 0 ? (
          <Fact label={facts.findings.length === 1 ? "finding" : "findings"}>
            <ul className="inbox-card__findings">
              {facts.findings.map((finding, index) => {
                const where = finding.file
                  ? `${finding.file}${finding.line ? `:${finding.line}` : ""}`
                  : null;
                const meta = [finding.source, finding.severity, finding.kind, finding.rule]
                  .filter(Boolean)
                  .join(" · ");
                return (
                  <li key={index}>
                    {meta ? <span className="inbox-card__finding-meta">{meta}</span> : null}
                    {finding.message ? <div>{finding.message}</div> : null}
                    {where ? (
                      <div>
                        {finding.fileHref ? (
                          <External href={finding.fileHref}>{where}</External>
                        ) : (
                          <code>{where}</code>
                        )}
                      </div>
                    ) : null}
                  </li>
                );
              })}
            </ul>
          </Fact>
        ) : null}
        {facts.narratives.map((narrative, index) => (
          <Fact key={index} label={narrative.label}>
            <pre className="inbox-card__narrative">{narrative.text}</pre>
          </Fact>
        ))}
        {facts.evidence.length > 0 ? (
          <Fact label="agent evidence">
            <ul className="inbox-card__findings">
              {facts.evidence.map((item, index) => (
                <li key={index}>{item}</li>
              ))}
            </ul>
          </Fact>
        ) : null}
        {readiness ? (
          <Fact label="readiness">
            {readiness.sonarGate !== undefined ? (
              <div>
                SonarCloud gate {readiness.sonarGate}
                {readiness.openIssues !== undefined
                  ? `, ${readiness.openIssues} open issues`
                  : ""}
                {readiness.hotspots !== undefined ? `, ${readiness.hotspots} hotspots` : ""}
              </div>
            ) : null}
            {readiness.unresolvedThreads !== undefined ? (
              <div>
                {readiness.unresolvedThreads} unresolved review threads
                {readiness.totalThreads !== undefined ? ` of ${readiness.totalThreads}` : ""}
              </div>
            ) : null}
            {readiness.ciChecks && readiness.ciChecks.length > 0 ? (
              <div>
                CI:{" "}
                {readiness.ciChecks.map((check) => `${check.name} ${check.state}`).join(", ")}
              </div>
            ) : null}
            {readiness.failures.map((failure, index) => (
              <div key={index} className="muted">
                not read — {failure}
              </div>
            ))}
          </Fact>
        ) : null}
        {facts.unresolved.map((item) => (
          <Fact key={item.name} label={item.name}>
            <span className="muted">not available — {item.reason}</span>
          </Fact>
        ))}
        {facts.truncated ? (
          <p className="muted">
            Some values were shortened to fit; the run view has them whole.
          </p>
        ) : null}
      </dl>
    </>
  );
}

/** Shorten a sha256 digest the way the Workflows table does. */
function shortDigest(digest: string): string {
  return digest.length > 21 ? `${digest.slice(0, 20)}…` : digest;
}

/**
 * Render one context ref the way the workflow declares it: a pointer as the
 * pointer, a literal (issue #73) as the declared value.
 */
function renderBinding(ref: HumanTaskBinding): string {
  return typeof ref === "string" ? ref : JSON.stringify(ref.literal);
}

/**
 * The audit trail, collapsed: node, token, workflow digest, the edge the
 * task arrived by, the ledger guard, and the context refs exactly as the
 * workflow declares them. Kept (a decider can still open it) and out of the
 * way (it is not what the decision is about).
 */
export function HumanTaskAudit({
  task,
  ledgerGuard,
}: {
  task: HumanTask;
  /** The ledger-guard row's content, or undefined for a card without one. */
  ledgerGuard?: ReactNode;
}) {
  const request = task.request ?? {};
  const audit = request.audit;
  const refs = request.context_refs;
  return (
    <details className="inbox-card__audit-details">
      <summary>audit</summary>
      <dl className="inbox-card__audit">
        {audit?.node_id ? (
          <Fact label="node">
            <code>{audit.node_id}</code>
          </Fact>
        ) : null}
        {audit?.token_id ? (
          <Fact label="token">
            <code>{audit.token_id}</code>
          </Fact>
        ) : null}
        {audit?.workflow_digest ? (
          <Fact label="workflow">
            <code title={audit.workflow_digest}>{shortDigest(audit.workflow_digest)}</code>
          </Fact>
        ) : null}
        {audit?.from_node ? (
          <Fact label="arrived via">
            {audit.from_node} → {audit.from_outcome}
          </Fact>
        ) : null}
        {ledgerGuard !== undefined ? <Fact label="ledger guard">{ledgerGuard}</Fact> : null}
        {refs?.from ? (
          <Fact label="input from">
            <code>{refs.from}</code>
          </Fact>
        ) : null}
        {refs?.bindings ? (
          <Fact label="bindings">
            <ul className="inbox-card__bindings">
              {Object.entries(refs.bindings).map(([name, ref]) => (
                <li key={name}>
                  {name}: <code>{renderBinding(ref)}</code>
                </li>
              ))}
            </ul>
          </Fact>
        ) : null}
      </dl>
    </details>
  );
}
