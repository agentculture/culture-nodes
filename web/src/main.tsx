import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";

// The org's two variable faces, self-hosted — tokens.css names
// "Fraunces Variable" and "Albert Sans Variable" in --font-display /
// --font-body, and these are the packages the AgentCulture project itself ships.
import "@fontsource-variable/fraunces";
import "@fontsource-variable/albert-sans";

// The design layer, imported once and globally. Everything downstream reads
// its custom properties; nothing redefines them.
import "./culture-design/tokens.css";
import "@xyflow/react/dist/style.css";
import "./styles/app.css";

import App from "./App";
import { SharedEventsProvider } from "./hooks/useSharedEvents";

const container = document.getElementById("root");
if (!container) {
  throw new Error("index.html is missing its #root container");
}

createRoot(container).render(
  <StrictMode>
    {/*
      One app-wide EventSource (task t27, c48/h41): SharedEventsProvider
      holds the cross-run stream open for the app's real lifetime so route
      transitions — where every view-level subscriber can briefly reach
      zero between one view's unmount and the next view's mount — never
      tear the connection down and reopen it. See
      hooks/useSharedEvents.tsx for the manager this pins.
    */}
    <SharedEventsProvider>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </SharedEventsProvider>
  </StrictMode>,
);
