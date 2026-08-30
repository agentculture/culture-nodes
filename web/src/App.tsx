import { useEffect } from "react";
import {
  Navigate,
  Route,
  Routes,
  useLocation,
} from "react-router-dom";
import AgentStateScript from "./agent-state/AgentStateScript";
import { setAgentState } from "./agent-state/store";
import Header from "./components/Header";
import AuthorWorkflow from "./routes/AuthorWorkflow";
import Decisions from "./routes/Decisions";
import GenerateWorkflow from "./routes/GenerateWorkflow";
import Inbox from "./routes/Inbox";
import JobsTimeline from "./routes/JobsTimeline";
import LedgerView from "./routes/LedgerView";
import Mesh from "./routes/Mesh";
import NodeGraphs from "./routes/NodeGraphs";
import PlanView from "./routes/PlanView";
import RunView from "./routes/RunView";
import RunsBoard from "./routes/RunsBoard";
import RunsList from "./routes/RunsList";
import Statistics from "./routes/Statistics";
import TicketView from "./routes/TicketView";

/**
 * Route path → the leading half of the document title (task t27). Matched
 * longest-prefix-first below, so `/runs/:id/ledger` beats `/runs/:id` beats
 * `/runs`. Every entry is the same word the header nav uses for that view —
 * a tab strip and a browser tab that name the same page differently is the
 * defect this fixes, not two vocabularies to maintain.
 */
const ROUTE_TITLES: ReadonlyArray<readonly [string, string]> = [
  ["/runs", "Runs"],
  ["/board", "Board"],
  ["/jobs", "Jobs"],
  ["/inbox", "Inbox"],
  ["/decisions", "Decisions"],
  ["/mesh", "Mesh"],
  ["/stats", "Statistics"],
  ["/graphs", "Node Graphs"],
  ["/plan", "Plan"],
  ["/workflows/new", "New workflow"],
  ["/workflows/generate", "Generate workflow"],
  ["/tickets", "Ticket"],
];

const APP_TITLE = "Culture Nodes";

/**
 * The title for a path. A run/ticket/plan detail page names its subject —
 * `Run 01J… · Culture Nodes` — because a reader with eight tabs open is
 * choosing between runs, not between the word "Runs" eight times.
 */
export function titleForPath(pathname: string): string {
  const ledger = /^\/runs\/([^/]+)\/ledger$/.exec(pathname);
  if (ledger) return `Ledger ${decodeURIComponent(ledger[1])} · ${APP_TITLE}`;
  const run = /^\/runs\/([^/]+)$/.exec(pathname);
  if (run) return `Run ${decodeURIComponent(run[1])} · ${APP_TITLE}`;
  const ticket = /^\/tickets\/([^/]+)$/.exec(pathname);
  if (ticket) return `Ticket ${decodeURIComponent(ticket[1])} · ${APP_TITLE}`;
  const plan = /^\/plan\/([^/]+)$/.exec(pathname);
  if (plan) return `Plan ${decodeURIComponent(plan[1])} · ${APP_TITLE}`;

  const matches = ROUTE_TITLES.filter(
    ([prefix]) => pathname === prefix || pathname.startsWith(`${prefix}/`),
  ).sort((a, b) => b[0].length - a[0].length);
  if (matches.length > 0) return `${matches[0][1]} · ${APP_TITLE}`;
  if (pathname === "/") return APP_TITLE;
  return `Not found · ${APP_TITLE}`;
}

/**
 * Keeps agent-state's `route` — and the document title — in step with the
 * router. Both are the same fact ("which view is on screen") told to two
 * different readers, so they are set from one effect rather than drifting
 * apart in two.
 */
function RouteWatcher() {
  const location = useLocation();
  useEffect(() => {
    setAgentState({ route: location.pathname });
    document.title = titleForPath(location.pathname);
  }, [location.pathname]);
  return null;
}

export function App() {
  return (
    <>
      <a className="skip-link" href="#main">
        Skip to content
      </a>
      <Header />
      <RouteWatcher />
      <main id="main">
        <Routes>
          <Route path="/" element={<Navigate to="/runs" replace />} />
          <Route path="/runs" element={<RunsList />} />
          <Route path="/board" element={<RunsBoard />} />
          <Route path="/jobs" element={<JobsTimeline />} />
          <Route path="/inbox" element={<Inbox />} />
          <Route path="/decisions" element={<Decisions />} />
          <Route path="/mesh" element={<Mesh />} />
          <Route path="/stats" element={<Statistics />} />
          <Route path="/graphs" element={<NodeGraphs />} />
          <Route path="/plan" element={<PlanView />} />
          <Route path="/plan/:slug" element={<PlanView />} />
          {/* The old Workflows tab's URL — old links/bookmarks survive by
              landing on the sub-tab that renders the same content the
              route used to (task t28, issue #56). */}
          <Route
            path="/workflows"
            element={<Navigate to="/graphs?tab=graphs" replace />}
          />
          <Route path="/workflows/new" element={<AuthorWorkflow />} />
          <Route path="/workflows/generate" element={<GenerateWorkflow />} />
          <Route path="/runs/:id" element={<RunView />} />
          <Route path="/runs/:id/ledger" element={<LedgerView />} />
          <Route path="/tickets/:id" element={<TicketView />} />
          <Route
            path="*"
            element={
              <section className="view-rail">
                <h1>Not found</h1>
                <p className="muted">
                  No view is routed at this path. Try the run list.
                </p>
              </section>
            }
          />
        </Routes>
      </main>
      <AgentStateScript />
    </>
  );
}

export default App;
