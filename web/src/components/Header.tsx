import { Link } from "react-router-dom";
import Mark from "../culture-design/mark";

/**
 * The app header, in the AgentCulture house style: the mark, the wordmark in
 * the display face, sticky, sitting on a blurred wash of the page ground
 * (culture-design/tokens.css supplies every value — no colours are invented
 * here, per PRD §8.1).
 */
export function Header() {
  return (
    <header className="app-header" id="app-header">
      <Link className="app-header__brand" to="/runs">
        <Mark size={28} />
        <span className="app-header__wordmark">Culture Nodes</span>
      </Link>
      <p className="app-header__tagline">
        Every node has a contract. Every result has evidence.
      </p>
    </header>
  );
}

export default Header;
