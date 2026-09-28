/**
 * What a human task is asking, read out of its resolved context (issue #332,
 * task t43).
 *
 * The API resolves a task's `request.context_refs` server-side and returns
 * the values as `resolved_context` (internal/api/humantaskcontext.go). This
 * module turns those values into the handful of facts a person needs to
 * decide: the question, the PR and where it lives, the ticket, each finding
 * with a link to the line, and the agent's own account. It is pure — no
 * fetches — so the card renders from one list response.
 *
 * It reads only the field names the two real task shapes carry (the
 * pr-upkeep `human-merges-pr` approval and the declaration `human.ask`
 * blocked-step task) and returns absent rather than guessing: a value it does
 * not recognise is not re-labelled as a PR or a ticket. Every link is built
 * only from values that match the shape of what they claim to be (an
 * `owner/repo`, a number, a hex sha, a Jira key), so a stray string in a run
 * input can never become an arbitrary href.
 */

import type { HumanTask, HumanTaskContextValue } from "../api/types";

/**
 * The Jira site a ticket links to when the task's data does not name one.
 * One constant, in one place: every Jira link in the inbox goes through
 * `jiraBrowseHref` below.
 */
export const JIRA_SITE_DEFAULT = "https://agentculture.atlassian.net";

const GITHUB = "https://github.com";

export interface TaskFinding {
  source?: string;
  severity?: string;
  kind?: string;
  rule?: string;
  message?: string;
  file?: string;
  line?: number;
  /** The file at the PR's head commit, when both are known. */
  fileHref?: string;
}

export interface TaskNarrative {
  /** Which binding it came from and what it is, e.g. "fix — summary". */
  label: string;
  text: string;
}

export interface TaskReadiness {
  sonarGate?: string;
  openIssues?: number;
  hotspots?: number;
  unresolvedThreads?: number;
  totalThreads?: number;
  ciChecks?: { name: string; state: string }[];
  failures: string[];
}

export interface TaskContextFacts {
  question: string;
  /** True when `question` was written by the task, not derived here. */
  questionGiven: boolean;
  repository?: string;
  prNumber?: number;
  prHref?: string;
  prTitle?: string;
  headSha?: string;
  ticketKey?: string;
  ticketHref?: string;
  workItem?: string;
  blockedStep?: string;
  findings: TaskFinding[];
  narratives: TaskNarrative[];
  evidence: string[];
  readiness?: TaskReadiness;
  /** Refs that did not resolve, with the server's reason. */
  unresolved: { name: string; reason: string }[];
  /** True when any value was shrunk to fit the API's size bound. */
  truncated: boolean;
}

type Obj = Record<string, unknown>;

const isObj = (value: unknown): value is Obj =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const str = (value: unknown): string | undefined =>
  typeof value === "string" && value.trim() !== "" ? value : undefined;

const REPO_RE = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const SHA_RE = /^[0-9a-f]{7,40}$/i;
const TICKET_RE = /^[A-Z][A-Z0-9]+-\d+$/;
const WORK_ITEM_RE = /^gh:([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)#(\d+)$/;

function positiveInt(value: unknown): number | undefined {
  if (typeof value === "number" && Number.isInteger(value) && value > 0)
    return value;
  if (typeof value === "string" && /^\d+$/.test(value)) {
    const n = Number(value);
    return n > 0 ? n : undefined;
  }
  return undefined;
}

export function githubPullHref(repository: string, number: number): string {
  return `${GITHUB}/${repository}/pull/${number}`;
}

/** A file at a commit, or undefined when any part is not the right shape. */
export function githubBlobHref(
  repository: string,
  sha: string,
  path: string,
  line?: number,
): string | undefined {
  if (!REPO_RE.test(repository) || !SHA_RE.test(sha)) return undefined;
  const segments = path.replace(/^\.?\//, "").split("/");
  if (segments.some((s) => s === "" || s === "." || s === "..")) return undefined;
  const encoded = segments.map(encodeURIComponent).join("/");
  return `${GITHUB}/${repository}/blob/${sha}/${encoded}${line ? `#L${line}` : ""}`;
}

/** `https://<site>/browse/<KEY>`; the site must be an https origin. */
export function jiraBrowseHref(key: string, site?: string): string | undefined {
  if (!TICKET_RE.test(key)) return undefined;
  let origin = JIRA_SITE_DEFAULT;
  if (site) {
    try {
      const url = new URL(site.includes("://") ? site : `https://${site}`);
      if (url.protocol === "https:") origin = url.origin;
    } catch {
      /* an unparseable site falls back to the default */
    }
  }
  return `${origin}/browse/${key}`;
}

function humanize(id: string): string {
  const words = id.replace(/[-_]+/g, " ").trim();
  return words ? words[0].toUpperCase() + words.slice(1) : id;
}

function readFinding(raw: unknown): TaskFinding | null {
  if (!isObj(raw)) return null;
  const finding: TaskFinding = {
    source: str(raw.source),
    severity: str(raw.severity),
    kind: str(raw.kind),
    rule: str(raw.rule),
    message: str(raw.title) ?? str(raw.message),
    file: str(raw.file) ?? str(raw.path),
    line: positiveInt(raw.line),
  };
  return finding.message || finding.rule || finding.file ? finding : null;
}

function readReadiness(value: Obj): TaskReadiness | undefined {
  if (value.record !== "readiness" && !("sonar" in value && "threads" in value))
    return undefined;
  const sonar = isObj(value.sonar) ? value.sonar : {};
  const threads = isObj(value.threads) ? value.threads : {};
  const num = (v: unknown) => (typeof v === "number" ? v : undefined);
  const ci = Array.isArray(value.ci)
    ? value.ci.flatMap((check) =>
        isObj(check) && str(check.name) && str(check.state)
          ? [{ name: check.name as string, state: check.state as string }]
          : [],
      )
    : undefined;
  const failures = Array.isArray(value.failures)
    ? value.failures.flatMap((f) =>
        isObj(f) && str(f.source) ? [`${f.source}: ${str(f.detail) ?? "unread"}`] : [],
      )
    : [];
  return {
    sonarGate: str(sonar.gate),
    openIssues: num(sonar.open_issues),
    hotspots: num(sonar.hotspots),
    unresolvedThreads: num(threads.unresolved),
    totalThreads: num(threads.total),
    ciChecks: ci,
    failures,
  };
}

/**
 * Read the facts out of a task. `value` entries are walked in the order the
 * API lists them (`from` first, then bindings by name); the first value to
 * name a field wins.
 */
export function taskContextFacts(task: HumanTask): TaskContextFacts {
  const facts: TaskContextFacts = {
    question: "",
    questionGiven: false,
    findings: [],
    narratives: [],
    evidence: [],
    unresolved: [],
    truncated: false,
  };
  let jiraSite: string | undefined;
  let question: string | undefined;

  const entries: HumanTaskContextValue[] = task.resolved_context ?? [];
  for (const entry of entries) {
    if (entry.unresolved) {
      facts.unresolved.push({ name: entry.name, reason: entry.unresolved });
      continue;
    }
    if (entry.truncated) facts.truncated = true;
    const value = entry.value;
    const label = entry.name === "from" ? "" : entry.name;

    if (typeof value === "string") {
      if (value.trim()) facts.narratives.push({ label: label || "context", text: value });
      continue;
    }
    if (!isObj(value)) continue;

    question ??= str(value.question) ?? str(value.instruction);
    const repository = str(value.repository);
    if (!facts.repository && repository && REPO_RE.test(repository))
      facts.repository = repository;
    facts.prNumber ??= positiveInt(value.number) ?? positiveInt(value.pull_request);
    facts.prTitle ??= str(value.pr_title) ?? str(value.title);
    const sha = str(value.head_sha);
    if (!facts.headSha && sha && SHA_RE.test(sha)) facts.headSha = sha;
    facts.workItem ??= str(value.work_item);
    facts.blockedStep ??= str(value.blocked_step);
    jiraSite ??= str(value.jira_site) ?? str(value.jira_base_url);
    const ticket =
      str(value.ticket) ?? str(value.ticket_key) ?? str(value.issue_key) ?? str(value.jira_key);
    if (!facts.ticketKey && ticket && TICKET_RE.test(ticket)) facts.ticketKey = ticket;

    if (Array.isArray(value.findings)) {
      for (const raw of value.findings) {
        const finding = readFinding(raw);
        if (finding) facts.findings.push(finding);
      }
    }
    const summary = str(value.summary);
    if (summary)
      facts.narratives.push({ label: label ? `${label} — summary` : "summary", text: summary });
    const report = value.agent_report;
    if (typeof report === "string" && report.trim()) {
      facts.narratives.push({ label: "agent report", text: report });
    } else if (isObj(report)) {
      const reason = str(report.reason) ?? str(report.summary);
      if (reason) facts.narratives.push({ label: "agent report — reason", text: reason });
      if (Array.isArray(report.evidence))
        for (const item of report.evidence)
          if (typeof item === "string" && item.trim()) facts.evidence.push(item);
    }
    facts.readiness ??= readReadiness(value);
  }

  // A work item names the PR when the input carries no repository/number.
  const workItem = facts.workItem ? WORK_ITEM_RE.exec(facts.workItem) : null;
  if (workItem) {
    facts.repository ??= workItem[1];
    facts.prNumber ??= Number(workItem[2]);
  }
  if (facts.repository && facts.prNumber)
    facts.prHref = githubPullHref(facts.repository, facts.prNumber);
  if (facts.ticketKey) facts.ticketHref = jiraBrowseHref(facts.ticketKey, jiraSite);
  if (facts.repository && facts.headSha) {
    for (const finding of facts.findings)
      if (finding.file)
        finding.fileHref = githubBlobHref(
          facts.repository,
          facts.headSha,
          finding.file,
          finding.line,
        );
  }

  if (question) {
    facts.question = question;
    facts.questionGiven = true;
  } else {
    const node = task.request?.audit?.node_id;
    const declaration = task.firing?.declaration_name;
    const subject = facts.prNumber ? `PR #${facts.prNumber}` : undefined;
    if (subject && node && /merge/i.test(node)) {
      facts.question = `Merge decision for ${subject}`;
    } else {
      const what = humanize(declaration ?? node ?? task.kind);
      facts.question = subject ? `${what} — ${subject}` : what;
    }
  }
  return facts;
}
