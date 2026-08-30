import { useEffect, useState, type FormEvent } from "react";
import { Link, NavLink, useNavigate } from "react-router-dom";
import { getVersion } from "../api/client";
import type { Version } from "../api/types";
import Mark from "../culture-design/mark";

/** The repo's own README — the only documentation this app can link to. */
const DOCS_URL = "https://github.com/agentculture/culture-nodes#readme";

/**
 * The app header, in the AgentCulture house style: the mark, the wordmark in
 * the display face, sticky, sitting on a blurred wash of the page ground
 * (culture-design/tokens.css supplies every value — no colours are invented
 * here, per PRD §8.1).
 *
 * On a narrow/vertical viewport the primary nav collapses behind a Menu
 * disclosure button (issue #12 item 2). The button only *renders visibly*
 * below the layout breakpoint (app.css hides it otherwise); the state and
 * aria wiring live here so the collapse works without any JS-side media
 * query. Links are NavLinks so the current view is marked in the nav
 * (`.is-active`, the hook app.css always had for it) and choosing one
 * closes the collapsed menu.
 *
 * The utility row carries two things a reader otherwise has to leave the app
 * to get (task t27): a link to the README, and which revision of the control
 * plane is answering — read from `GET /v1alpha1/version` once per page load
 * and rendered as the short revision with the API's own `staleness` sentence
 * as its tooltip. A failed or unstamped read says so rather than showing
 * nothing: "revision unknown" is a fact about the deployment, and the header
 * is where issue #104 would have been caught fifteen hours earlier.
 */
export function Header() {
  const [navOpen, setNavOpen] = useState(false);
  const [version, setVersion] = useState<Version | null>(null);
  const [versionFailed, setVersionFailed] = useState(false);
  const [ticketKey, setTicketKey] = useState("");
  const navigate = useNavigate();
  const closeNav = () => setNavOpen(false);
  const navLinkClass = ({ isActive }: { isActive: boolean }) =>
    isActive ? "is-active" : "";

  useEffect(() => {
    const controller = new AbortController();
    getVersion(controller.signal)
      .then((payload) => {
        if (!controller.signal.aborted) setVersion(payload);
      })
      .catch(() => {
        if (!controller.signal.aborted) setVersionFailed(true);
      });
    return () => controller.abort();
  }, []);

  // There is no ticket-list endpoint (openapi.yaml has only
  // `GET /tickets/{id}`), so the nav offers the only affordance the API can
  // honestly back: type a key, open that ticket's page.
  const openTicket = (event: FormEvent) => {
    event.preventDefault();
    const trimmed = ticketKey.trim();
    if (!trimmed) return;
    closeNav();
    navigate(`/tickets/${encodeURIComponent(trimmed)}`);
  };

  return (
    <header className="app-header" id="app-header">
      <Link className="app-header__brand" to="/runs" onClick={closeNav}>
        <Mark size={28} />
        <span className="app-header__wordmark">Culture Nodes</span>
      </Link>
      <button
        type="button"
        className="app-header__menu"
        id="app-header-menu"
        aria-expanded={navOpen}
        aria-controls="app-header-nav"
        onClick={() => setNavOpen((open) => !open)}
      >
        Menu
      </button>
      <nav
        className={`app-header__nav${navOpen ? " is-open" : ""}`}
        id="app-header-nav"
        aria-label="Primary"
      >
        <NavLink to="/runs" className={navLinkClass} onClick={closeNav}>
          Runs
        </NavLink>
        <NavLink to="/board" className={navLinkClass} onClick={closeNav}>
          Board
        </NavLink>
        <NavLink to="/jobs" className={navLinkClass} onClick={closeNav}>
          Jobs
        </NavLink>
        <NavLink to="/inbox" className={navLinkClass} onClick={closeNav}>
          Inbox
        </NavLink>
        <NavLink to="/decisions" className={navLinkClass} onClick={closeNav}>
          Decisions
        </NavLink>
        <NavLink to="/mesh" className={navLinkClass} onClick={closeNav}>
          Mesh
        </NavLink>
        <NavLink to="/stats" className={navLinkClass} onClick={closeNav}>
          Statistics
        </NavLink>
        <NavLink to="/graphs" className={navLinkClass} onClick={closeNav}>
          Node Graphs
        </NavLink>
        <NavLink to="/plan" className={navLinkClass} onClick={closeNav}>
          Plan
        </NavLink>
        <NavLink to="/workflows/generate" className={navLinkClass} onClick={closeNav}>
          Generate
        </NavLink>
        <form className="app-header__ticket" onSubmit={openTicket}>
          <label htmlFor="app-header-ticket-key">Tickets</label>
          <input
            id="app-header-ticket-key"
            className="control control--input app-header__ticket-input"
            value={ticketKey}
            onChange={(event) => setTicketKey(event.target.value)}
            placeholder="SCRUM-6"
            aria-describedby="app-header-ticket-hint"
          />
          <button
            type="submit"
            className="control control--button"
            disabled={ticketKey.trim() === ""}
          >
            Open
          </button>
          <span id="app-header-ticket-hint" className="sr-only">
            There is no ticket listing; open one by its key.
          </span>
        </form>
      </nav>
      <p className="app-header__tagline">
        Every node has a contract. Every result has evidence.
      </p>
      <p className="app-header__utility">
        <a
          className="app-header__docs"
          href={DOCS_URL}
          target="_blank"
          rel="noreferrer"
        >
          Docs
        </a>
        <span
          className="app-header__version"
          id="app-header-version"
          data-revision={version?.revision ?? ""}
          title={versionReadoutTitle(version, versionFailed)}
        >
          {versionReadout(version, versionFailed)}
        </span>
      </p>
    </header>
  );
}

/**
 * The readout itself: version plus the short revision, or an explicit
 * statement that the revision is not established. Never a bare blank — the
 * whole point of the field is that a reader can tell "unstamped" from
 * "not fetched yet".
 */
function versionReadout(version: Version | null, failed: boolean): string {
  if (failed) return "version unavailable";
  if (!version) return "version…";
  if (!version.revision) return `${version.version} · revision unknown`;
  const short = version.revision.slice(0, 7);
  return `${version.version} · ${short}${version.revision_is_dirty ? "+dirty" : ""}`;
}

/**
 * The tooltip is the API's own `staleness` sentence, verbatim. It is the one
 * thing a reader needs and cannot derive — what this answer does and does not
 * establish — and paraphrasing it here would put a second, drifting claim next
 * to the authoritative one.
 */
function versionReadoutTitle(version: Version | null, failed: boolean): string {
  if (failed) {
    return "GET /v1alpha1/version did not answer, so which revision is serving this page is unknown";
  }
  if (!version) return "reading GET /v1alpha1/version…";
  return version.staleness;
}

export default Header;
