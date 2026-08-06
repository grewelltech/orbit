import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./app/App";
import { Conformance } from "./app/Conformance";
import { registerOrbitTheme } from "./theme/echarts";
import "./styles/global.css";

// Registered before the first chart mounts; requires the stylesheet above so
// the design tokens resolve.
registerOrbitTheme();

const host = document.getElementById("root");
if (!host) throw new Error("#root not found");

// One page or the other by query string. Conformance is a distinct question
// from a load run — "does this core behave to spec" versus "how much can it
// take" — so it gets its own view rather than a tab bar bolted onto the run
// dashboard.
const conformance = new URLSearchParams(window.location.search).has("conformance");

createRoot(host).render(
  <StrictMode>{conformance ? <Conformance /> : <App />}</StrictMode>,
);
