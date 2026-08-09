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
import LedgerView from "./routes/LedgerView";
import RunView from "./routes/RunView";
import RunsList from "./routes/RunsList";

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
          <Route path="/runs/:id" element={<RunView />} />
          <Route path="/runs/:id/ledger" element={<LedgerView />} />
          <Route
            path="*"
            element={
              <section className="container">
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
