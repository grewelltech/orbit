import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./app/App";
import { registerOrbitTheme } from "./theme/echarts";
import "./styles/global.css";

// Registered before the first chart mounts; requires the stylesheet above so
// the design tokens resolve.
registerOrbitTheme();

const host = document.getElementById("root");
if (!host) throw new Error("#root not found");

createRoot(host).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
