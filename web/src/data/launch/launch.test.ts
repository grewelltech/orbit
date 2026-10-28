import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createClient, createRouterTransport } from "@connectrpc/connect";
import { PlanRunResponseSchema, RenderRunResponseSchema, RunService, StartRunResponseSchema, type StartRunRequest } from "@/gen/orbit/v1/run_pb";
import type { ResolveRequest } from "@/gen/orbit/v1/target_pb";
import { LAUNCH_IDLE, LaunchRunner, type LaunchState } from "./launch";
import type { LaunchInput } from "./resolve";

const base: LaunchInput = {
  targetName: "lab1",
  targetYaml: 'kind: target\nname: lab1\nsubscribers:\n  - {name: main, ki: "${ORBIT_KI}"}\n',
  runYaml: "kind: run\nworkloads:\n  - name: load\n    attach: {count: 1}\n",
  template: "load",
  values: { ORBIT_KI: "k" },
};

/** A RunService that records what it is sent; PlanRun echoes the run file's count into the plan. */
function server(opts: { renderDelay?: (req: { runYaml: string }) => number; planError?: string } = {}) {
  const plans: ResolveRequest[] = [];
  const starts: StartRunRequest[] = [];
  const transport = createRouterTransport(({ service }) => {
    service(RunService, {
      async renderRun(req) {
        await new Promise((r) => setTimeout(r, opts.renderDelay?.(req) ?? 0));
        return create(RenderRunResponseSchema, { command: `orbit ${req.runYaml.length}`, runYaml: req.runYaml });
      },
      planRun(req) {
        plans.push(req);
        if (opts.planError) throw new ConnectError(opts.planError, Code.InvalidArgument);
        const count = Number(/count: (\d+)/.exec(req.runYaml)?.[1] ?? 0);
        return create(PlanRunResponseSchema, {
          plan: { workloads: [{ name: "load", kind: { case: "population", value: { attach: { count } } } }] },
          runName: "r",
        });
      },
      startRun(req) {
        starts.push(req);
        return create(StartRunResponseSchema, { run: { runId: "run-1" } });
      },
    });
  });
  return { client: createClient(RunService, transport), plans, starts };
}

const runner = (client: ReturnType<typeof server>["client"]) => {
  const box = { s: LAUNCH_IDLE as LaunchState };
  return { box, r: new LaunchRunner(client, (f) => (box.s = f(box.s))) };
};
const count = (s: StartRunRequest) => (s.spec.case === "plan" ? s.spec.value.workloads[0]?.kind.value : undefined);

describe("LaunchRunner", () => {
  it("renders keyless, and an older render's answer never replaces a newer one", async () => {
    const { client } = server({ renderDelay: (req) => (req.runYaml.includes("slow") ? 40 : 0) });
    const { box, r } = runner(client);
    const slow = r.render({ ...base, runYaml: base.runYaml + "# slow\n" });
    await r.render(base);
    await slow;
    expect(box.s.rendered?.runYaml).toBe(base.runYaml);
  });

  it("an older render's answer is dropped even when the server ignores the abort", async () => {
    // A client that answers late and never honours the signal.
    const client = {
      renderRun: (req: { runYaml?: string }) =>
        new Promise<ReturnType<typeof create<typeof RenderRunResponseSchema>>>((res) =>
          setTimeout(() => res(create(RenderRunResponseSchema, { runYaml: req.runYaml ?? "" })), req.runYaml?.includes("slow") ? 40 : 0),
        ),
      planRun: () => Promise.reject(new Error("unused")),
      startRun: () => Promise.reject(new Error("unused")),
    };
    const box = { s: LAUNCH_IDLE as LaunchState };
    const r = new LaunchRunner(client, (f) => (box.s = f(box.s)));
    const slow = r.render({ ...base, runYaml: base.runYaml + "# slow\n" });
    await r.render(base);
    await slow;
    expect(box.s.rendered?.runYaml).toBe(base.runYaml);
  });

  it("holds the plan back while a ${VAR} has no value, and sends none", async () => {
    const { client, plans } = server();
    const { box, r } = runner(client);
    expect(await r.plan({ ...base, values: {} })).toBeUndefined();
    expect(box.s.unresolved).toEqual(["ORBIT_KI"]);
    expect(plans).toHaveLength(0);
  });

  it("starts with the plan resolved for exactly this form state, unchanged", async () => {
    const { client, plans, starts } = server();
    const { box, r } = runner(client);
    const held = await r.plan(base);
    expect(await r.start(() => base, held)).toBe("run-1");
    expect(plans).toHaveLength(1); // not resolved again
    expect(starts[0]!.spec.case).toBe("plan");
    expect(starts[0]!.spec.value).toEqual(held!.response.plan);
    expect(starts[0]!.name).toBe("r");
    expect(box.s.started).toBe("run-1");
  });

  it("resolves again before starting when the form changed since the plan", async () => {
    const { client, plans, starts } = server();
    const { r } = runner(client);
    const held = await r.plan(base);
    const edited = { ...base, runYaml: base.runYaml.replace("count: 1", "count: 9") };
    await r.start(() => edited, held);
    expect(plans).toHaveLength(2);
    expect(count(starts[0]!)).toMatchObject({ attach: { count: 9 } });
  });

  it("resolves again when a value changed (another key in the tab)", async () => {
    const { client, plans } = server();
    const { r } = runner(client);
    const held = await r.plan(base);
    await r.start(() => ({ ...base, values: { ORBIT_KI: "k2" } }), held);
    expect(plans).toHaveLength(2);
    expect(plans[1]!.vars).toEqual([expect.objectContaining({ name: "ORBIT_KI", value: "k2" })]);
  });

  it("stops, sending nothing, when the form changes while the plan resolves", async () => {
    const { client, starts } = server();
    const { box, r } = runner(client);
    let now = base;
    const started = r.start(() => now, undefined);
    now = { ...base, runYaml: base.runYaml.replace("count: 1", "count: 2") };
    expect(await started).toBeUndefined();
    expect(starts).toHaveLength(0);
    expect(box.s.planError?.message).toContain("start again");
  });

  it("a start whose plan a newer one replaced says so, and sends nothing", async () => {
    const { client, starts } = server();
    const { box, r } = runner(client);
    const started = r.start(() => base, undefined);
    void r.plan({ ...base, runYaml: base.runYaml.replace("count: 1", "count: 4") }); // a newer plan, from another edit
    expect(await started).toBeUndefined();
    expect(starts).toHaveLength(0);
    expect(box.s.planError?.message).toContain("newer plan");
  });

  it("a refused plan names the field, and nothing starts", async () => {
    const { client, starts } = server({ planError: "invalid run: workloads[load].attach.count: -1 is not a non-negative integer (run file lab1.run.yaml:4)" });
    const { box, r } = runner(client);
    expect(await r.start(() => base, undefined)).toBeUndefined();
    expect(starts).toHaveLength(0);
    expect(box.s.planError?.at?.path).toBe("workloads[load].attach.count");
  });

  it("forgets a plan on invalidate, and a plan resolving then is dropped", async () => {
    const { client } = server();
    const { box, r } = runner(client);
    const p = r.plan(base);
    r.invalidate();
    expect(await p).toBeUndefined();
    expect(box.s.plan).toBeUndefined();
  });
});
