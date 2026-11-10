import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Navigate, Route, Routes, useLocation, useParams } from "react-router";
import { App } from "./app/App";
import { Home } from "./app/Home";
import { Launch } from "./app/Launch";
import { NewTest } from "./app/NewTest";
import { Targets } from "./app/Targets";
import { TokenPrompt } from "./app/TokenPrompt";
import { registerOrbitTheme } from "./theme/echarts";
import "./styles/global.css";

// Registered before the first chart mounts; requires the stylesheet above so
// the design tokens resolve.
registerOrbitTheme();

const host = document.getElementById("root");
if (!host) throw new Error("#root not found");

// Routes (ADR-0010): "/" is the run history (Home); "/runs" is the dashboard,
// "/runs/:id" pins a run; "/new" chooses a test kind and "/launch[/:preset]"
// is the editor; "/conformance" opens the editor on the conformance suite;
// "/targets" edits the CLI's target files. The ?mock demo rides "/runs?mock".
function LaunchPreset() {
  const { preset } = useParams();
  // "blank" is the chooser's Custom choice: an explicit empty run file, not a
  // resumed draft. Launch treats preset="" as "start from an empty run".
  return <Launch preset={preset === "blank" ? "" : preset} />;
}

// RootGate preserves the pre-router query-string URLs: the old app put every
// page at "/?…", so a bookmarked or shared link still lands on the right page.
function RootGate() {
  const { search, hash } = useLocation();
  const q = new URLSearchParams(search);
  if (q.has("run")) {
    const id = q.get("run") ?? "";
    return <Navigate replace to={{ pathname: `/runs/${encodeURIComponent(id)}`, search: q.has("mock") ? "?mock" : "" }} />;
  }
  if (q.has("mock")) return <Navigate replace to={{ pathname: "/runs", search: "?mock" }} />;
  if (q.has("conformance")) return <Navigate replace to="/launch/conformance" />;
  if (q.has("launch")) return <Navigate replace to="/launch" />;
  if (q.has("targets") || hash.startsWith("#import=")) return <Navigate replace to={{ pathname: "/targets", hash }} />;
  return <Home />;
}

createRoot(host).render(
  <StrictMode>
    <BrowserRouter>
      <Routes>
        <Route path="/" element={<RootGate />} />
        <Route path="/runs" element={<App />} />
        <Route path="/runs/:id" element={<App />} />
        <Route path="/new" element={<NewTest />} />
        <Route path="/launch" element={<Launch />} />
        <Route path="/launch/:preset" element={<LaunchPreset />} />
        <Route path="/conformance" element={<Launch preset="conformance" />} />
        <Route path="/targets" element={<Targets />} />
        <Route path="*" element={<Navigate replace to="/" />} />
      </Routes>
      <TokenPrompt />
    </BrowserRouter>
  </StrictMode>,
);
