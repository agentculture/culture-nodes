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
import IdentityGate from "./components/IdentityGate";
import { useWhoami } from "./hooks/useWhoami";
import AuthorWorkflow from "./routes/AuthorWorkflow";
import Decisions from "./routes/Decisions";
import Design from "./routes/Design";
import DesignCanvas from "./routes/DesignCanvas";
import GenerateWorkflow from "./routes/GenerateWorkflow";
import Home from "./routes/Home";
import Inbox from "./routes/Inbox";
import LedgerView from "./routes/LedgerView";
import Mesh from "./routes/Mesh";
import PlanView from "./routes/PlanView";
import RunView from "./routes/RunView";
import Runs, { type RunsView } from "./routes/Runs";
import Statistics from "./routes/Statistics";
import TicketView from "./routes/TicketView";

/**
 * Route path → the leading half of the document title (task t27). Matched
 * longest-prefix-first below, so `/runs/:id/ledger` beats `/runs/:id` beats
 * `/runs`. Every entry is the same word the header nav uses for that view —
 * a tab strip and a browser tab that name the same page differently is the
 * defect this fixes, not two vocabularies to maintain.
 *
 * A moved URL is titled as the view it *lands on*, not as the view it used
 * to be (task t9): `/board` reads "Runs" because a redirect is what happens
 * there, and dropping the entry instead would flash "Not found" in the tab
 * on the way through.
 */
const ROUTE_TITLES: ReadonlyArray<readonly [string, string]> = [
  ["/runs", "Runs"],
  ["/board", "Runs"],
  ["/jobs", "Runs"],
  ["/inbox", "Inbox"],
  ["/decisions", "Decisions"],
  ["/mesh", "Mesh"],
  ["/stats", "Statistics"],
  ["/design", "Design"],
  ["/design/canvas", "Design canvas"],
  ["/design/new", "New workflow"],
  ["/design/generate", "Generate workflow"],
  ["/graphs", "Design"],
  ["/plan", "Ledger-and-plan"],
  ["/workflows", "Design"],
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

/**
 * `/board` and `/jobs` after task t9: the same runs dataset, now a
 * projection of one `/runs` page.
 *
 * A plain `<Navigate to="/runs?view=board">` would answer the path and drop
 * the query, which is exactly the case that matters — the time-range filter
 * writes `since`/`until` into the URL (issue #23), so those are the links
 * people bookmarked and pasted. Carrying the existing params through and
 * only setting `view` keeps a bookmarked range pointing at the same window
 * it did before the URL moved.
 */
function RedirectToRunsProjection({ view }: { view: RunsView }) {
  const { search } = useLocation();
  const params = new URLSearchParams(search);
  params.set("view", view);
  return <Navigate to={{ pathname: "/runs", search: `?${params}` }} replace />;
}

/**
 * What `/` is (task t17, spec c25).
 *
 * It used to be a redirect to `/runs` — a table of engine rows, which is
 * right for an operator and wrong for the person a Jira comment just sent
 * here. A signed-in person now lands on the page that says what is waiting
 * on a human; everyone else keeps the run table, because a LAN reader with
 * no Access identity has no "your work" to show and a redirect is a better
 * answer than an empty page.
 *
 * Only a 401 redirects. `loading` renders Home rather than bouncing to /runs
 * and back the moment whoami answers — two navigations a person can see —
 * and `unavailable` renders it for the reason IdentityGate states: a whoami
 * that failed for any other cause is not a fact about who is here, so it may
 * not read as "signed out". Redirecting on it also cost the landing its
 * settled state: the second navigation restarted the load, so `#agent-state`
 * was back at "loading" for a whole extra round trip after the page had
 * already finished one.
 */
function Landing() {
  const whoami = useWhoami();
  if (whoami.status === "unauthenticated") return <Navigate to="/runs" replace />;
  return <Home />;
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
        {/* Identity is derived from the signed-in principal (task t9): an
            unbound login sees a named full-page state instead of the routed
            view, a missing one sees "sign in required" above it. */}
        <IdentityGate>
        <Routes>
          <Route path="/" element={<Landing />} />
          {/* One page, three projections (task t9). The two URLs that used
              to be separate destinations still answer, carrying whatever
              range the reader had bookmarked with them. */}
          <Route path="/runs" element={<Runs />} />
          <Route
            path="/board"
            element={<RedirectToRunsProjection view="board" />}
          />
          <Route
            path="/jobs"
            element={<RedirectToRunsProjection view="jobs" />}
          />
          <Route path="/inbox" element={<Inbox />} />
          <Route path="/decisions" element={<Decisions />} />
          <Route path="/mesh" element={<Mesh />} />
          <Route path="/stats" element={<Statistics />} />
          <Route path="/design" element={<Design />} />
          <Route path="/design/canvas" element={<DesignCanvas />} />
          {/* The retired view's URL (task t8). /graphs named a view that
              drew cards, not graphs; /workflows named the tab that became
              its sub-tab. Both land on Design, so old links and bookmarks
              reach the view that renders what they were looking for. */}
          <Route path="/graphs" element={<Navigate to="/design" replace />} />
          <Route path="/plan" element={<PlanView />} />
          <Route path="/plan/:slug" element={<PlanView />} />
          {/* Authoring lives under the view that composes workflows (task
              t9, PRD §8.6 Design). The /workflows/* URLs the authoring
              slice shipped with are kept as redirects, not retired. */}
          <Route path="/design/new" element={<AuthorWorkflow />} />
          <Route path="/design/generate" element={<GenerateWorkflow />} />
          <Route
            path="/workflows"
            element={<Navigate to="/design" replace />}
          />
          <Route
            path="/workflows/new"
            element={<Navigate to="/design/new" replace />}
          />
          <Route
            path="/workflows/generate"
            element={<Navigate to="/design/generate" replace />}
          />
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
        </IdentityGate>
      </main>
      <AgentStateScript />
    </>
  );
}

export default App;
