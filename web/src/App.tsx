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
import Inbox from "./routes/Inbox";
import JobsTimeline from "./routes/JobsTimeline";
import LedgerView from "./routes/LedgerView";
import Mesh from "./routes/Mesh";
import NodeGraphs from "./routes/NodeGraphs";
import RunView from "./routes/RunView";
import RunsBoard from "./routes/RunsBoard";
import RunsList from "./routes/RunsList";
import Statistics from "./routes/Statistics";

/** Keeps agent-state's `route` in step with the router. */
function RouteWatcher() {
  const location = useLocation();
  useEffect(() => {
    setAgentState({ route: location.pathname });
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
          <Route path="/mesh" element={<Mesh />} />
          <Route path="/stats" element={<Statistics />} />
          <Route path="/graphs" element={<NodeGraphs />} />
          {/* The old Workflows tab's URL — old links/bookmarks survive by
              landing on the sub-tab that renders the same content the
              route used to (task t28, issue #56). */}
          <Route
            path="/workflows"
            element={<Navigate to="/graphs?tab=graphs" replace />}
          />
          <Route path="/workflows/new" element={<AuthorWorkflow />} />
          <Route path="/runs/:id" element={<RunView />} />
          <Route path="/runs/:id/ledger" element={<LedgerView />} />
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
