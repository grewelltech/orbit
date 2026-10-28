import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./app/App";
import { Launch } from "./app/Launch";
import { Targets } from "./app/Targets";
import { TokenPrompt } from "./app/TokenPrompt";
import { registerOrbitTheme } from "./theme/echarts";
import "./styles/global.css";

// Registered before the first chart mounts; requires the stylesheet above so
// the design tokens resolve.
registerOrbitTheme();

const host = document.getElementById("root");
if (!host) throw new Error("#root not found");

// One page or another by query string. Targets (?targets) are the CLI's
// target files, kept and edited in the browser; Launch (?launch) builds a
// run against one and starts it, and ?conformance opens it on the
// conformance suite. A run's verdicts show on its run view (?run=<id>).
const query = new URLSearchParams(window.location.search);
const page = query.has("conformance") ? (
  <Launch preset="conformance" />
) : query.has("targets") ? (
  <Targets />
) : query.has("launch") ? (
  <Launch />
) : (
  <App />
);

createRoot(host).render(
  <StrictMode>
    {page}
    <TokenPrompt />
  </StrictMode>,
);
