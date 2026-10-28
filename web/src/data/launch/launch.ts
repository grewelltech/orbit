/**
 * The Launch page's calls, outside React so they can be tested against a
 * server: RenderRun as the form changes (the command and the file to
 * save), PlanRun when asked (the plan, its provenance), and Start, which
 * sends StartRun the plan resolved from exactly what the page shows, and
 * resolves again first when the form has changed since (formKey).
 */
import { Code, ConnectError } from "@connectrpc/connect";
import type { MessageInitShape } from "@bufbuild/protobuf";
import type { PlanRunResponse, RenderRunResponse, StartRunRequestSchema, StartRunResponse } from "@/gen/orbit/v1/run_pb";
import { formKey, planRequest, refusedAt, renderRequest, type LaunchInput, type PlanRequest, type RenderRequest } from "./resolve";

/** The part of a RunService client the page uses. */
export interface LaunchClient {
  planRun(req: PlanRequest, opts?: { signal?: AbortSignal }): Promise<PlanRunResponse>;
  renderRun(req: RenderRequest, opts?: { signal?: AbortSignal }): Promise<RenderRunResponse>;
  startRun(req: MessageInitShape<typeof StartRunRequestSchema>): Promise<StartRunResponse>;
}

/** A refusal the page shows: the server's words, and the field it names when it names one. */
export interface Refusal {
  message: string;
  at?: ReturnType<typeof refusedAt>;
}

export interface LaunchState {
  /** The command and the file to save, for the run file as it is now. */
  rendered?: RenderRunResponse;
  renderError?: Refusal;
  /** The last plan, and the form state it was resolved from. */
  plan?: { key: string; response: PlanRunResponse };
  planError?: Refusal;
  /** The ${VAR}s the target and the run file name with no value in the tab. */
  unresolved: string[];
  busy?: "planning" | "starting";
  /** The run Start started. */
  started?: string;
}

export const LAUNCH_IDLE: LaunchState = { unresolved: [] };

function refusal(err: unknown): Refusal {
  const ce = ConnectError.from(err);
  return { message: ce.rawMessage, at: ce.code === Code.InvalidArgument ? refusedAt(ce.rawMessage) : undefined };
}

export class LaunchRunner {
  private renderSeq = 0;
  private renderAbort: AbortController | null = null;
  private planSeq = 0;

  constructor(
    private readonly client: LaunchClient,
    private readonly setState: (f: (prev: LaunchState) => LaunchState) => void,
  ) {}

  /** Renders input's run file as a command. A newer call wins: an older answer is dropped. */
  async render(input: Pick<LaunchInput, "targetName" | "runYaml" | "template">): Promise<void> {
    this.renderAbort?.abort();
    const ctrl = new AbortController();
    this.renderAbort = ctrl;
    const seq = ++this.renderSeq;
    try {
      const r = await this.client.renderRun(renderRequest(input), { signal: ctrl.signal });
      if (seq === this.renderSeq) this.setState((s) => ({ ...s, rendered: r, renderError: undefined }));
    } catch (err) {
      if (seq === this.renderSeq && !ctrl.signal.aborted) this.setState((s) => ({ ...s, rendered: undefined, renderError: refusal(err) }));
    }
  }

  /**
   * Resolves input into a plan (PlanRun). The plan is kept with the form
   * state it came from; a plan for an older state is never kept.
   */
  async plan(input: LaunchInput): Promise<{ key: string; response: PlanRunResponse } | undefined> {
    const { request, unresolved } = planRequest(input);
    if (!request) {
      this.setState((s) => ({ ...s, unresolved, plan: undefined }));
      return undefined;
    }
    const key = formKey(input);
    const seq = ++this.planSeq;
    this.setState((s) => ({ ...s, unresolved: [], busy: "planning", planError: undefined }));
    try {
      const response = await this.client.planRun(request);
      if (seq !== this.planSeq) return undefined;
      const plan = { key, response };
      this.setState((s) => ({ ...s, plan, busy: undefined }));
      return plan;
    } catch (err) {
      if (seq === this.planSeq) this.setState((s) => ({ ...s, plan: undefined, planError: refusal(err), busy: undefined }));
      return undefined;
    }
  }

  /** Forgets a plan the form no longer matches: an edit, another target, other values. */
  invalidate(): void {
    this.planSeq++;
    this.setState((s) => ({ ...s, plan: undefined, planError: undefined, busy: s.busy === "planning" ? undefined : s.busy }));
  }

  /**
   * Starts the run the page shows: StartRun with the plan PlanRun gave for
   * exactly the current form state, unchanged; resolved again first when
   * the plan held is for another state (or there is none). current is read
   * again before StartRun: an edit made while it resolved stops the start
   * (the page says to start again) rather than send a plan for what the
   * form no longer shows. Returns the run's ID.
   */
  async start(current: () => LaunchInput, held: { key: string; response: PlanRunResponse } | undefined): Promise<string | undefined> {
    const input = current();
    const plan = held && held.key === formKey(input) ? held : await this.plan(input);
    if (!plan) return undefined;
    if (plan.key !== formKey(current())) {
      this.setState((s) => ({ ...s, plan: undefined, planError: { message: "the form changed while the plan was resolved; start again" } }));
      return undefined;
    }
    this.setState((s) => ({ ...s, busy: "starting" }));
    try {
      const r = plan.response;
      if (!r.plan) throw new Error("the server sent no plan");
      const res = await this.client.startRun({ name: r.runName, spec: { case: "plan", value: r.plan }, eventVerbosity: r.eventVerbosity });
      const id = res.run?.runId ?? "";
      this.setState((s) => ({ ...s, busy: undefined, started: id }));
      return id;
    } catch (err) {
      this.setState((s) => ({ ...s, busy: undefined, planError: refusal(err) }));
      return undefined;
    }
  }

  dispose(): void {
    this.renderAbort?.abort();
    this.renderSeq++;
    this.planSeq++;
  }
}
