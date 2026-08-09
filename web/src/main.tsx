import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";

// The org's two variable faces, self-hosted — tokens.css names
// "Fraunces Variable" and "Albert Sans Variable" in --font-display /
// --font-body, and these are the packages agentculture.org itself ships.
import "@fontsource-variable/fraunces";
import "@fontsource-variable/albert-sans";

// The design layer, imported once and globally. Everything downstream reads
// its custom properties; nothing redefines them.
import "./culture-design/tokens.css";
import "@xyflow/react/dist/style.css";
import "./styles/app.css";

import App from "./App";

const container = document.getElementById("root");
if (!container) {
  throw new Error("index.html is missing its #root container");
}

createRoot(container).render(
  <StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </StrictMode>,
);
