import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { JoinTokenSchema, type JoinToken } from "@/gen/orbit/v1/join_pb";
import * as act from "../targets/actions";
import type { KeyValueStore, StoredTarget } from "../targets/store";
import { parseTargets, render } from "../targets/yamlio";
import { nameResponderIn, usesResponder } from "./addToTarget";
import { joinBlocked } from "./gate";
import { MASK, maskOneLiner } from "./mask";
import { copyLine, joinPageLifecycle, JoinSession, revokeOnHide, type JoinClient, type JoinState } from "./session";
import { joinSpecFor } from "./spec";

/** A one-liner as CreateJoinToken writes it (join.go), with a stand-in token. */
const SECRET = "STAND-IN-join-token-0123456789";
const LINE = `curl -q -fsS --proto '=https' -k --pinnedpubkey 'sha256//pIn+key/AAAA=' 'https://192.0.2.10:8412/join/${SECRET}' | sudo sh`;

describe("whether the page may ask for a join token", () => {
  const rows: [string, { protocol: string; hostname: string }, string, string | undefined][] = [
    ["https on loopback, no token", { protocol: "https:", hostname: "127.0.0.1" }, "", undefined],
    ["https on localhost", { protocol: "https:", hostname: "localhost" }, "", undefined],
    ["http on loopback: the server refuses plaintext, loopback too", { protocol: "http:", hostname: "127.0.0.1" }, "t", "issued only over TLS"],
    ["http from another host", { protocol: "http:", hostname: "192.0.2.10" }, "t", "issued only over TLS"],
    ["https from another host, no token", { protocol: "https:", hostname: "orbit.lab" }, "", "no API token in this tab"],
    ["https from another host, a token", { protocol: "https:", hostname: "orbit.lab" }, "t", undefined],
    ["a loopback lookalike is another host", { protocol: "https:", hostname: "127.0.0.1.evil.example" }, "", "no API token in this tab"],
  ];
  for (const [name, loc, token, want] of rows) {
    it(name, () => {
      const got = joinBlocked(loc, token);
      if (want === undefined) expect(got).toBeUndefined();
      else expect(got).toContain(want);
    });
  }
});

describe("the spec a target gives a join token", () => {
  const doc = (core: string) => {
    const [p] = parseTargets(`version: 1\nkind: target\nname: lab1\ncore:\n    amf: 192.0.2.1:38412\n${core}`);
    if (!p) throw new Error("no target");
    return p.doc;
  };
  it("core.ue_pools, upf_n6 and n6_subnet, as add-responder -t takes them", () => {
    const d = doc("    ue_pools: [10.250.0.0/16, 10.251.0.0/16]\n    upf_n6: 198.51.100.1\n    n6_subnet: 198.51.100.0/24\n");
    expect(joinSpecFor(d, {}, " 192.0.2.10:8412 ")).toEqual({
      spec: { uePools: ["10.250.0.0/16", "10.251.0.0/16"], upfN6: "198.51.100.1", n6Subnet: "198.51.100.0/24", managementAddress: "192.0.2.10:8412" },
    });
  });
  it("a field the target lacks is named", () => {
    expect(joinSpecFor(doc("    ue_pools: [10.250.0.0/16]\n"), {}, "")).toEqual({ missing: { fields: ["core.upf_n6", "core.n6_subnet"], vars: [] } });
  });
  it("a ${VAR} is filled from this tab's values, or named when it has none", () => {
    const d = doc("    ue_pools: [10.250.0.0/16]\n    upf_n6: ${UPF}\n    n6_subnet: ${N6}\n");
    expect(joinSpecFor(d, { UPF: "198.51.100.1" }, "")).toEqual({ missing: { fields: [], vars: ["N6"] } });
    expect(joinSpecFor(d, { UPF: "198.51.100.1", N6: "198.51.100.0/24" }, "")).toMatchObject({ spec: { upfN6: "198.51.100.1", n6Subnet: "198.51.100.0/24" } });
  });
});

describe("the one-liner, masked", () => {
  it("the token never shows; the pin, the server and the command do", () => {
    const m = maskOneLiner(LINE, SECRET);
    expect(m).not.toContain(SECRET);
    expect(m).toContain(`/join/${MASK}'`);
    expect(m).toContain("--pinnedpubkey 'sha256//pIn+key/AAAA='");
    expect(m).toContain("| sudo sh");
  });
  it("every occurrence", () => expect(maskOneLiner(`${SECRET} ${SECRET}`, SECRET)).toBe(`${MASK} ${MASK}`));
  it("no secret, nothing masked", () => expect(maskOneLiner(LINE, "")).toBe(LINE));
});

describe("use <name> in <target>", () => {
  const mem = (): KeyValueStore & { data: Map<string, string> } => {
    const data = new Map<string, string>();
    return { data, getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v), removeItem: (k) => void data.delete(k) };
  };
  it("names the responder in app.responders, by name only, through editTarget's gate; a second time changes nothing", () => {
    const s = { local: mem(), session: mem() };
    const targets: StoredTarget[] = [{ name: "lab1", yaml: "version: 1\nkind: target\nname: lab1\ncore:\n    amf: 192.0.2.1:38412\n" }];
    const r = act.editTarget(s, targets, "lab1", nameResponderIn("app5"));
    expect(r?.targets[0]?.yaml).toContain("responders:");
    const [p] = parseTargets(r?.targets[0]?.yaml ?? "");
    if (!p) throw new Error("no target");
    expect(usesResponder(p.doc, "app5")).toBe(true);
    expect(r?.targets[0]?.yaml).not.toContain("token");
    const again = act.editTarget(s, r?.targets ?? [], "lab1", nameResponderIn("app5"));
    expect(again?.targets[0]?.yaml).toBe(r?.targets[0]?.yaml);
    const doc = p.doc;
    nameResponderIn("app5")(doc);
    expect(render(doc).match(/name: app5/g)?.length).toBe(1);
  });
});

/** A JoinService that answers as the server does, recording what it was asked. */
function server(opts: { createFails?: string; revokeCode?: Code; listFails?: boolean } = {}) {
  const calls: string[] = [];
  let pending: JoinToken[] = [];
  let n = 0;
  const client: JoinClient = {
    async createJoinToken(req) {
      calls.push(`create ${req.name} ${req.ttlSeconds}`);
      if (opts.createFails) throw new ConnectError(opts.createFails, Code.FailedPrecondition);
      const issued = create(JoinTokenSchema, { id: `jt-${++n}`, name: req.name, expiresUnixNano: BigInt(Date.UTC(2026, 9, 28, 12, 10)) * 1_000_000n });
      pending.push(issued);
      return { issued, secret: SECRET, oneLiner: LINE };
    },
    async listJoinTokens() {
      calls.push("list");
      if (opts.listFails) throw new ConnectError("unreachable", Code.Unavailable);
      return { tokens: pending };
    },
    async revokeJoinToken(req) {
      calls.push(`revoke ${req.id}`);
      if (opts.revokeCode !== undefined) throw new ConnectError("no", opts.revokeCode);
      pending = pending.filter((t) => t.id !== req.id);
      return {};
    },
  };
  return { client, calls, use: (id: string) => (pending = pending.filter((t) => t.id !== id)) };
}

const SPEC = { uePools: ["10.250.0.0/16"], upfN6: "198.51.100.1", n6Subnet: "198.51.100.0/24", managementAddress: "" };

describe("a join token's life on the page", () => {
  // Nothing about a token is ever stored: storage that fails the test if touched.
  let touched: string[] = [];
  const spy = (): Storage => ({
    length: 0,
    clear: () => void touched.push("clear"),
    getItem: (k: string) => (touched.push(`get ${k}`), null),
    key: () => null,
    removeItem: (k: string) => void touched.push(`remove ${k}`),
    setItem: (k: string) => void touched.push(`set ${k}`),
  });
  beforeEach(() => {
    touched = [];
    Object.assign(globalThis, { localStorage: spy(), sessionStorage: spy() });
  });
  afterEach(() => {
    delete (globalThis as { localStorage?: Storage }).localStorage;
    delete (globalThis as { sessionStorage?: Storage }).sessionStorage;
  });

  const at = Date.UTC(2026, 9, 28, 12, 0);
  const session = (srv: ReturnType<typeof server>, clock = { now: at }) => {
    const seen: JoinState[] = [];
    return { s: new JoinSession(srv.client, (x) => seen.push(x), () => clock.now), seen, clock };
  };

  it("created: the line held in memory only, nothing stored, the expiry kept", async () => {
    const srv = server();
    const { s, seen } = session(srv);
    await s.create("app5", 600, SPEC);
    expect(seen.map((x) => x.phase)).toEqual(["creating", "ready"]);
    expect(seen.at(-1)).toMatchObject({ phase: "ready", line: LINE, secret: SECRET, expiresAt: Date.UTC(2026, 9, 28, 12, 10) });
    expect(srv.calls).toEqual(["create app5 600"]);
    expect(touched).toEqual([]);
  });

  it("at its expiry the line is dropped", async () => {
    const srv = server();
    const { s, clock } = session(srv);
    await s.create("", 600, SPEC);
    clock.now = Date.UTC(2026, 9, 28, 12, 9, 59);
    expect(s.tick().phase).toBe("ready");
    clock.now = Date.UTC(2026, 9, 28, 12, 10);
    const st = s.tick();
    expect(st.phase).toBe("expired");
    expect(st.line).toBeUndefined();
    expect(st.secret).toBeUndefined();
  });

  it("used: a responder naming it as its join token; the line is dropped, and close revokes nothing", async () => {
    const srv = server();
    const { s } = session(srv);
    await s.create("app5", 600, SPEC);
    s.sawResponders([{ name: "app4", joinTokenId: "jt-other" }]);
    expect(s.tick().phase).toBe("ready");
    s.sawResponders([{ name: "app5", joinTokenId: "jt-1" }]);
    const st = s.tick();
    expect(st).toMatchObject({ phase: "used", usedBy: "app5" });
    expect(st.line).toBeUndefined();
    await s.close();
    expect(srv.calls).toEqual(["create app5 600"]);
  });

  it("close: an unused token is revoked (still pending), then dropped", async () => {
    const srv = server();
    const { s, seen } = session(srv);
    await s.create("app5", 600, SPEC);
    await s.close();
    expect(srv.calls).toEqual(["create app5 600", "list", "revoke jt-1"]);
    expect(seen.at(-1)).toEqual({ phase: "idle" });
  });

  it("close: a token used meanwhile (no longer pending) is not revoked", async () => {
    const srv = server();
    const { s } = session(srv);
    await s.create("app5", 600, SPEC);
    srv.use("jt-1");
    await s.close();
    expect(srv.calls).toEqual(["create app5 600", "list"]);
  });

  it("close: a revoke that fails is told, not thrown; one a registration beat is not a failure", async () => {
    const failing = server({ listFails: true });
    const a = session(failing);
    await a.s.create("app5", 600, SPEC);
    await a.s.close();
    expect(a.seen.at(-1)?.note).toContain("could not be revoked");
    const beaten = server({ revokeCode: Code.FailedPrecondition });
    const b = session(beaten);
    await b.s.create("app5", 600, SPEC);
    await b.s.close();
    expect(b.seen.at(-1)).toEqual({ phase: "idle" });
  });

  it("Revoke: revoked, its line dropped; used when a registration came first", async () => {
    const srv = server();
    const { s } = session(srv);
    await s.create("app5", 600, SPEC);
    await s.revoke();
    expect(s.tick()).toMatchObject({ phase: "revoked" });
    expect(s.tick().line).toBeUndefined();
    const beaten = server({ revokeCode: Code.FailedPrecondition });
    const b = session(beaten);
    await b.s.create("app5", 600, SPEC);
    await b.s.revoke();
    expect(b.s.tick()).toMatchObject({ phase: "used", note: "used: the host joined" });
  });

  it("a second token revokes the first, unused, before it is made", async () => {
    const srv = server();
    const { s } = session(srv);
    await s.create("app5", 600, SPEC);
    await s.create("app6", 600, SPEC);
    expect(srv.calls).toEqual(["create app5 600", "list", "revoke jt-1", "create app6 600"]);
  });

  it("a refused create says why", async () => {
    const { s } = session(server({ createFails: "join needs orbit serve with TLS" }));
    await s.create("app5", 600, SPEC);
    expect(s.tick()).toEqual({ phase: "failed", note: "join needs orbit serve with TLS" });
  });

  it("the page going (pagehide) revokes a live, unused token; nothing once it is not live; the remover takes it off", async () => {
    const srv = server();
    const { s } = session(srv);
    const revoked: string[] = [];
    const win = new EventTarget();
    const remove = joinPageLifecycle(win, s, (id) => revoked.push(id));
    win.dispatchEvent(new Event("pagehide"));
    expect(revoked).toEqual([]);
    await s.create("app5", 600, SPEC);
    win.dispatchEvent(new Event("pagehide"));
    expect(revoked).toEqual(["jt-1"]);
    s.sawResponders([{ name: "app5", joinTokenId: "jt-1" }]);
    win.dispatchEvent(new Event("pagehide"));
    expect(revoked).toEqual(["jt-1"]);
    remove();
  });

  // Once copied, the line is on its way to an app host: only Revoke, or a
  // yes to close's question, ends it before its TTL.
  const copied = async (srv: ReturnType<typeof server>) => {
    const x = session(srv);
    await x.s.create("app5", 600, SPEC);
    const wrote: string[] = [];
    expect(await copyLine(x.s, { writeText: async (t) => void wrote.push(t) })).toBe(true);
    expect(wrote).toEqual([LINE]);
    return x;
  };

  it("copy, then close, keep (the default): not revoked; the panel says until when it works", async () => {
    const srv = server();
    const { s, seen } = await copied(srv);
    const asked: string[] = [];
    await s.close((q) => (asked.push(q), false));
    expect(srv.calls).toEqual(["create app5 600"]);
    expect(asked[0]).toMatch(/^The one-liner, shown or copied, works until .+ \(10 min left\)\. Revoke it now\?$/);
    expect(seen.at(-1)?.note).toContain("kept: the one-liner, shown or copied, works until it is used or expires");
    expect(seen.at(-1)?.line).toBeUndefined();
  });

  it("copy, then close, revoke: revoked", async () => {
    const srv = server();
    const { s } = await copied(srv);
    await s.close(() => true);
    expect(srv.calls).toEqual(["create app5 600", "list", "revoke jt-1"]);
  });

  it("copy, then the panel goes (close with no question): not revoked", async () => {
    const srv = server();
    const { s } = await copied(srv);
    await s.close();
    expect(srv.calls).toEqual(["create app5 600"]);
  });

  it("copy, then pagehide: not revoked; no copy, then pagehide: revoked", async () => {
    const win = new EventTarget();
    const revoked: string[] = [];
    const a = await copied(server());
    joinPageLifecycle(win, a.s, (id) => revoked.push(`copied ${id}`));
    win.dispatchEvent(new Event("pagehide"));
    expect(revoked).toEqual([]);
    const win2 = new EventTarget();
    const srv = server();
    const b = session(srv);
    await b.s.create("app6", 600, SPEC);
    joinPageLifecycle(win2, b.s, (id) => revoked.push(id));
    win2.dispatchEvent(new Event("pagehide"));
    expect(revoked).toEqual(["jt-1"]);
  });

  it("Revoke always revokes, copied or not", async () => {
    const srv = server();
    const { s } = await copied(srv);
    await s.revoke();
    expect(srv.calls).toEqual(["create app5 600", "revoke jt-1"]);
    expect(s.tick()).toMatchObject({ phase: "revoked" });
  });

  it("copyLine: the real line (whatever the panel shows); nothing written when there is no line or the write fails", async () => {
    const { s } = session(server());
    const wrote: string[] = [];
    expect(await copyLine(s, { writeText: async (t) => void wrote.push(t) })).toBe(false);
    expect(wrote).toEqual([]);
    await s.create("app5", 600, SPEC);
    expect(await copyLine(s, { writeText: async () => Promise.reject(new Error("denied")) })).toBe(false);
    expect(s.tick().copied).toBeUndefined();
    expect(await copyLine(s, { writeText: async (t) => void wrote.push(t) })).toBe(true);
    expect(wrote).toEqual([LINE]);
    expect(wrote[0]).not.toContain(MASK);
    expect(s.tick().copied).toBe(true);
  });

  it("a back/forward-cache restore checks the token again: gone from pending, the panel resets; still pending, it stays", async () => {
    const srv = server();
    const { s } = session(srv);
    await s.create("app5", 600, SPEC);
    const win = new EventTarget();
    joinPageLifecycle(win, s, () => undefined);
    win.dispatchEvent(Object.assign(new Event("pageshow"), { persisted: false }));
    await Promise.resolve();
    expect(srv.calls).toEqual(["create app5 600"]);
    win.dispatchEvent(Object.assign(new Event("pageshow"), { persisted: true }));
    await new Promise((r) => setTimeout(r, 0));
    expect(s.tick().phase).toBe("ready");
    srv.use("jt-1");
    win.dispatchEvent(Object.assign(new Event("pageshow"), { persisted: true }));
    await new Promise((r) => setTimeout(r, 0));
    expect(s.tick()).toMatchObject({ phase: "idle" });
    expect(s.tick().note).toContain("no longer live");
  });

  // Revealed, the line can be selected and copied by hand: it counts as
  // copied, so nothing revokes it behind the user's back.
  it("reveal, then close: asks (and keep keeps it)", async () => {
    const srv = server();
    const { s } = session(srv);
    await s.create("app5", 600, SPEC);
    s.markRevealed();
    const asked: string[] = [];
    await s.close((q) => (asked.push(q), false));
    expect(asked).toHaveLength(1);
    expect(srv.calls).toEqual(["create app5 600"]);
  });

  it("reveal, then pagehide or the panel going: kept; never revealed nor copied: revoked silently", async () => {
    const revoked: string[] = [];
    const srv = server();
    const { s } = session(srv);
    await s.create("app5", 600, SPEC);
    s.markRevealed();
    const win = new EventTarget();
    joinPageLifecycle(win, s, (id) => revoked.push(id));
    win.dispatchEvent(new Event("pagehide"));
    await s.close();
    expect(revoked).toEqual([]);
    expect(srv.calls).toEqual(["create app5 600"]);
    const plain = server();
    const b = session(plain);
    await b.s.create("app6", 600, SPEC);
    const asked: string[] = [];
    await b.s.close((q) => (asked.push(q), true));
    expect(asked).toEqual([]);
    expect(plain.calls).toEqual(["create app6 600", "list", "revoke jt-1"]);
  });

  it("a copy by hand from the shown line (the copy event) counts as copied", async () => {
    const srv = server();
    const { s } = session(srv);
    await s.create("app5", 600, SPEC);
    s.markCopied();
    expect(s.tick().copied).toBe(true);
    expect(s.liveID()).toBeUndefined();
  });

  it("a bfcache recheck that cannot reach the server says so, and does not leave the line shown as ready", async () => {
    const srv = server({ listFails: true });
    const { s } = session(srv);
    await s.create("app5", 600, SPEC);
    await s.recheck();
    const st = s.tick();
    expect(st.phase).toBe("idle");
    expect(st.line).toBeUndefined();
    expect(st.note).toContain("couldn't check this token: unreachable");
  });

  it("revokeOnHide: a keepalive POST to RevokeJoinToken, with the API token when there is one", () => {
    const sent: { url: string; init: RequestInit }[] = [];
    const fetcher = ((url: string, init: RequestInit) => (sent.push({ url, init }), Promise.resolve(new Response("{}")))) as unknown as typeof fetch;
    revokeOnHide(fetcher, "https://orbit.lab:8412", "tok", "jt-1");
    revokeOnHide(fetcher, "https://127.0.0.1:8412", "", "jt-2");
    expect(sent[0]?.url).toBe("https://orbit.lab:8412/orbit.v1.JoinService/RevokeJoinToken");
    expect(sent[0]?.init).toMatchObject({ method: "POST", keepalive: true, body: JSON.stringify({ id: "jt-1" }) });
    expect((sent[0]?.init.headers as Record<string, string>).Authorization).toBe("Bearer tok");
    expect((sent[1]?.init.headers as Record<string, string>).Authorization).toBeUndefined();
  });
});
