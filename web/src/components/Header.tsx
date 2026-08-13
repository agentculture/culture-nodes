import { useState } from "react";
import { Link, NavLink } from "react-router-dom";
import Mark from "../culture-design/mark";

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
 */
export function Header() {
  const [navOpen, setNavOpen] = useState(false);
  const closeNav = () => setNavOpen(false);
  const navLinkClass = ({ isActive }: { isActive: boolean }) =>
    isActive ? "is-active" : "";

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
        <NavLink to="/mesh" className={navLinkClass} onClick={closeNav}>
          Mesh
        </NavLink>
        <NavLink to="/stats" className={navLinkClass} onClick={closeNav}>
          Statistics
        </NavLink>
        <NavLink to="/workflows" className={navLinkClass} onClick={closeNav}>
          Workflows
        </NavLink>
      </nav>
      <p className="app-header__tagline">
        Every node has a contract. Every result has evidence.
      </p>
    </header>
  );
}

export default Header;
