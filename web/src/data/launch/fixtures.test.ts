/**
 * The Launch page's requests for a few form states, kept as fixtures the
 * server's tests replay through PlanRun and StartRun against a mock core
 * (internal/server/launch_e2e_test.go): the page's mapping and the
 * server's resolver are tested together, without a browser. A fixture
 * holds each ${VAR}'s name, never a value: the Go test supplies the
 * mock core's address and keys.
 *
 * Regenerate: UPDATE_FIXTURES=1 npx vitest run src/data/launch/fixtures.test.ts
 */
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import type { Document } from "yaml";
import { setValue } from "../targets/yamlio";
import { addWorkload, setCategories, setGate, setUntil, startFrom, GATE_PRESETS } from "./form";
import { planRequest, refsOf } from "./resolve";
import { quickTargetText } from "./useLaunch";
import { renderRun } from "./runyaml";

/** A target as the Targets page keeps one: its AMF and keys as ${VAR}s. */
const TARGET = `version: 1
kind: target
name: lab1
core:
    amf: \${LAB_AMF}
    plmn:
        mcc: "208"
        mnc: "93"
    slices:
        - sst: 1
          sd: "010203"
subscribers:
    - name: main
      first_supi: "208930100007500"
      count: 10
      ki: \${ORBIT_KI}
      opc: \${ORBIT_OPC}
ran:
    n2_source_ips:
        - 127.0.0.1
        - 127.0.0.2
    n3_ips:
        - 127.0.0.1
    gnb_ids:
        first: 7
        bits: 24
`;

const STATES: Record<string, { template: string; build: () => Document; target?: string }> = {
  load: {
    template: "load",
    build: () => {
      const d = startFrom("load");
      setValue(d, ["workloads", 0, "attach", "count"], "3");
      setValue(d, ["workloads", 0, "attach", "rate"], "10");
      setGate(d, GATE_PRESETS[1]!.gate("load"));
      return d;
    },
  },
  fleet: {
    template: "fleet",
    build: () => {
      const d = startFrom("fleet");
      setValue(d, ["workloads", 0, "attach", "count"], "2");
      setUntil(d, { kind: "duration", duration: "2s" });
      return d;
    },
  },
  conformance: {
    template: "conformance",
    build: () => {
      const d = startFrom("conformance");
      setCategories(d, 0, ["negative-ie"]);
      setValue(d, ["workloads", 0, "conformance", "per_test_timeout_ms"], "3000");
      return d;
    },
  },
  // ?conformance with a target made on the page from an AMF and a PLMN:
  // no keys, so the checks that need a UE SKIP.
  "conformance-amf-only": {
    template: "conformance",
    target: quickTargetText({ name: "core", amf: "\${LAB_AMF}", mcc: "208", mnc: "93", sst: "1", sd: "010203" }),
    build: () => {
      const d = startFrom("conformance");
      setValue(d, ["workloads", 0, "conformance", "per_test_timeout_ms"], "3000");
      return d;
    },
  },
  mixed: {
    template: "",
    build: () => {
      const d = startFrom(undefined);
      const p = addWorkload(d, "population");
      setValue(d, ["workloads", p, "attach", "count"], "2");
      const s = addWorkload(d, "conformance");
      setCategories(d, s, ["negative-ie"]);
      setValue(d, ["workloads", s, "conformance", "per_test_timeout_ms"], "3000");
      setUntil(d, { kind: "complete" });
      setGate(d, GATE_PRESETS[0]!.gate("pop"));
      setGate(d, GATE_PRESETS[3]!.gate("suite"));
      return d;
    },
  },
};

const dir = join(__dirname, "testdata");

describe("the Launch requests the server's tests replay", () => {
  for (const [name, st] of Object.entries(STATES)) {
    it(name, () => {
      const runYaml = renderRun(st.build());
      const targetYaml = st.target ?? TARGET;
      const refs = refsOf({ targetYaml, runYaml });
      const values = Object.fromEntries(refs.map((r) => [r, "x"]));
      const { request } = planRequest({ targetName: "lab1", targetYaml, runYaml, template: st.template, values });
      expect(request).toBeDefined();
      // Names only: the Go test gives the values.
      const fixture = `${JSON.stringify({ targetYaml: request!.targetYaml, runYaml: request!.runYaml, template: request!.template, vars: refs }, null, 2)}\n`;
      const path = join(dir, `${name}.json`);
      if (process.env.UPDATE_FIXTURES) writeFileSync(path, fixture);
      expect(readFileSync(path, "utf8"), `${path} is stale: UPDATE_FIXTURES=1 npx vitest run src/data/launch/fixtures.test.ts`).toBe(fixture);
    });
  }
});
