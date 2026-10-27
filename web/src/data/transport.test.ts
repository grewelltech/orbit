import { describe, expect, it } from "vitest";
import { Code, ConnectError, createClient, createRouterTransport } from "@connectrpc/connect";
import { RunService } from "@/gen/orbit/v1/run_pb";
import { authInterceptor, loadToken, saveToken, type TokenStore } from "./transport";

const TOKEN = "t".repeat(32);

function memStore(): TokenStore & { data: Map<string, string> } {
  const data = new Map<string, string>();
  return {
    data,
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
    removeItem: (k) => void data.delete(k),
  };
}

const broken: TokenStore = {
  getItem: () => {
    throw new Error("blocked");
  },
  setItem: () => {
    throw new Error("blocked");
  },
  removeItem: () => {
    throw new Error("blocked");
  },
};

function record(refusals: { n: number; why?: string[] }, why: string) {
  refusals.n++;
  (refusals.why ??= []).push(why);
}

/**
 * A RunService that, like `orbit serve --token-file`, refuses any call
 * without the right bearer token, and records the headers it saw.
 */
function server(token: () => string, refusals: { n: number; why?: string[] }) {
  const seen: (string | null)[] = [];
  const check = (h: Headers) => {
    seen.push(h.get("Authorization"));
    if (h.get("Authorization") !== `Bearer ${TOKEN}`) {
      throw new ConnectError("a bearer token is required", Code.Unauthenticated);
    }
  };
  const transport = createRouterTransport(
    (router) =>
      router.service(RunService, {
        listRuns: (_req, ctx) => {
          check(ctx.requestHeader);
          return { runs: [] };
        },
        async *runEvents(_req, ctx) {
          check(ctx.requestHeader);
          yield { seq: 1n };
        },
      }),
    { transport: { interceptors: [authInterceptor(token, (why) => record(refusals, why))] } },
  );
  return { client: createClient(RunService, transport), seen };
}

describe("authInterceptor", () => {
  it("sends the stored token as a bearer token", async () => {
    const refusals: { n: number; why?: string[] } = { n: 0 };
    const { client, seen } = server(() => TOKEN, refusals);
    await client.listRuns({});
    expect(seen).toEqual([`Bearer ${TOKEN}`]);
    expect(refusals.n).toBe(0);
  });

  it("sends no Authorization header without a token, and asks for one on the refusal", async () => {
    // The 401 path: the prompt opens on Unauthenticated, and the error still
    // reaches the caller so its own retry/poll logic runs as before.
    const refusals: { n: number; why?: string[] } = { n: 0 };
    const { client, seen } = server(() => "", refusals);
    await expect(client.listRuns({})).rejects.toMatchObject({ code: Code.Unauthenticated });
    expect(seen).toEqual([null]);
    expect(refusals.why).toEqual(["missing"]);
  });

  it("asks for the token when a stream is refused", async () => {
    const refusals: { n: number; why?: string[] } = { n: 0 };
    const { client } = server(() => "wrong", refusals);
    const drain = async () => {
      for await (const _ of client.runEvents({})) {
        /* refused before the first message */
      }
    };
    await expect(drain()).rejects.toMatchObject({ code: Code.Unauthenticated });
    expect(refusals.why).toEqual(["invalid"]);
  });

  it("reports a lockout, so the prompt can say the right token still works", async () => {
    const refusals = { n: 0, why: [] as string[] };
    const transport = createRouterTransport(
      (router) =>
        router.service(RunService, {
          listRuns: () => {
            throw new ConnectError("too many invalid bearer tokens", Code.ResourceExhausted);
          },
        }),
      { transport: { interceptors: [authInterceptor(() => "wrong", (why) => refusals.why.push(why))] } },
    );
    await expect(createClient(RunService, transport).listRuns({})).rejects.toMatchObject({
      code: Code.ResourceExhausted,
    });
    expect(refusals.why).toEqual(["locked"]);
  });

  it("does not ask for a token on other errors", async () => {
    const refusals: { n: number; why?: string[] } = { n: 0 };
    const transport = createRouterTransport(
      (router) =>
        router.service(RunService, {
          listRuns: () => {
            throw new ConnectError("no", Code.FailedPrecondition);
          },
        }),
      { transport: { interceptors: [authInterceptor(() => TOKEN, () => refusals.n++)] } },
    );
    await expect(createClient(RunService, transport).listRuns({})).rejects.toMatchObject({
      code: Code.FailedPrecondition,
    });
    expect(refusals.n).toBe(0);
  });

  it("uses a token saved after a refusal on the next call", async () => {
    const store = memStore();
    const refusals: { n: number; why?: string[] } = { n: 0 };
    const { client } = server(() => loadToken(store), refusals);
    await expect(client.listRuns({})).rejects.toBeTruthy();
    saveToken(`  ${TOKEN}\n`, store);
    await expect(client.listRuns({})).resolves.toBeTruthy();
    expect(refusals.n).toBe(1);
  });
});

describe("token storage", () => {
  it("round-trips a trimmed token and forgets an empty one", () => {
    const store = memStore();
    expect(loadToken(store)).toBe("");
    saveToken(` ${TOKEN} `, store);
    expect(loadToken(store)).toBe(TOKEN);
    saveToken("", store);
    expect(store.data.size).toBe(0);
  });

  it("degrades to no token when storage is unavailable", () => {
    expect(loadToken(broken)).toBe("");
    expect(() => saveToken(TOKEN, broken)).not.toThrow();
    expect(loadToken(undefined)).toBe("");
  });
});
