import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Navigate, Route, Routes, useLocation, useParams } from "react-router";
import { App } from "./app/App";
import { Home } from "./app/Home";
import { Launch } from "./app/Launch";
import { LaunchChooser } from "./app/LaunchChooser";
import { Targets } from "./app/Targets";
import { TokenPrompt } from "./app/TokenPrompt";
import { registerOrbitTheme } from "./theme/echarts";
import "./styles/global.css";

// Registered before the first chart mounts; requires the stylesheet above so
// the design tokens resolve.
registerOrbitTheme();

const host = document.getElementById("root");
if (!host) throw new Error("#root not found");

// Pages are routes: / (home: run overview), /runs (run dashboard; ?run=<id> pins a run), /launch (kind chooser; /launch/edit is the editor, /launch/<template|mixed> opens it on one),
// /conformance (launch on the conformance suite) and /targets. The older
// query-string URLs (?launch, ?targets, ?conformance) redirect to them.
function LaunchPreset() {
  const { kind } = useParams();
  // "mixed" is an empty run file; any other name is a template.
  return <Launch preset={kind === "mixed" ? "" : kind} />;
}

function Root() {
  const { search, hash } = useLocation();
  const q = new URLSearchParams(search);
  for (const page of ["conformance", "targets", "launch"]) {
    if (q.has(page)) {
      q.delete(page);
      const rest = q.toString();
      return <Navigate replace to={{ pathname: `/${page}`, search: rest ? `?${rest}` : "", hash }} />;
    }
  }
  // Pre-routing links to the dashboard (/?run=<id>, /?mock) keep working.
  if (q.has("run") || q.has("mock")) return <Navigate replace to={{ pathname: "/runs", search, hash }} />;
  return <Home />;
}

createRoot(host).render(
  <StrictMode>
    <BrowserRouter>
      <Routes>
        <Route path="/" element={<Root />} />
        <Route path="/runs" element={<App />} />
        <Route path="/launch" element={<LaunchChooser />} />
        <Route path="/launch/edit" element={<Launch />} />
        <Route path="/launch/:kind" element={<LaunchPreset />} />
        <Route path="/conformance" element={<Launch preset="conformance" />} />
        <Route path="/targets" element={<Targets />} />
        <Route path="*" element={<Navigate replace to="/" />} />
      </Routes>
    </BrowserRouter>
    <TokenPrompt />
  </StrictMode>,
);
