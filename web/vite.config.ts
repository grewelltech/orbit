import { fileURLToPath, URL } from "node:url";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// Built assets land in internal/webui/dist, where Go's embed.FS picks them up.
// The directory is committed so `go build` and `go install` work without Node.
const OUT_DIR = "../internal/webui/dist";

// Dev proxy target: a live `orbit serve` (internal/cli.DefaultListen).
const API = process.env.ORBIT_API ?? "http://127.0.0.1:8412";

// Connect RPCs are POSTed to /<proto package>.<Service>/<Method>.
const CONNECT_ROUTE = /^\/orbit\.v1\./;

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  build: {
    outDir: OUT_DIR,
    emptyOutDir: true,
    // Charts are the bulk of the bundle; splitting them keeps the shell's
    // first paint independent of ECharts parsing.
    rollupOptions: {
      output: {
        manualChunks: (id: string) => (id.includes("node_modules/echarts") || id.includes("node_modules/zrender") ? "echarts" : undefined),
      },
    },
  },
  server: {
    proxy: {
      [CONNECT_ROUTE.source]: { target: API, changeOrigin: true },
      "/metrics": { target: API, changeOrigin: true },
      "/healthz": { target: API, changeOrigin: true },
    },
  },
});
