import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { fromJson } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { ListJoinTokensResponseSchema, ListRespondersResponseSchema, RegisteredResponderSchema } from "@/gen/orbit/v1/join_pb";
import { ageText, NOT_SHOWN, removeConfirm, removeResponder, responderRows, revokeToken, SHOWN, tokenRows } from "./responders";

/**
 * Real ListResponders and ListJoinTokens answers, written by Go
 * (internal/server TestRespondersFixture -update-responders): app1 online,
 * app2 offline, app3 never seen, and app4's token pending, at
 * 2026-10-28T12:02:00Z.
 */
const read = (name: string) => JSON.parse(readFileSync(join(__dirname, "testdata", `${name}.json`), "utf8"));
const responders = fromJson(ListRespondersResponseSchema, read("responders")).responders;
const tokens = fromJson(ListJoinTokensResponseSchema, read("tokens")).tokens;
const now = Date.UTC(2026, 9, 28, 12, 2, 5);

describe("the responders the Targets page shows", () => {
  it("every RegisteredResponder field is reviewed: shown or not; a new one fails here until it is", () => {
    const fields = RegisteredResponderSchema.fields.map((f) => f.localName).sort();
    expect(fields).toEqual([...SHOWN, ...NOT_SHOWN].sort());
  });

  it("one row per responder, by name, in the server's own state, with how long ago it was seen", () => {
    expect(responderRows(responders, now)).toEqual([
      { name: "app1", state: "online", seen: "5s ago", control: "198.51.100.20:9551", data: "198.51.100.20 udp 32768-60999", version: "v0.9.1" },
      { name: "app2", state: "offline", seen: "2m ago", control: "198.51.100.30:9551", data: "198.51.100.30 udp 32768-60999", version: "v0.9.0" },
      { name: "app3", state: "never seen", seen: "never", control: "198.51.100.40:9551", data: "198.51.100.40 udp 32768-60999", version: "v0.9.0" },
    ]);
  });

  it("a row holds no field the page does not show (ids, peers, the token that registered it)", () => {
    const text = JSON.stringify(responderRows(responders, now));
    for (const r of responders) for (const v of [r.id, r.source, r.registeredFrom, r.joinTokenId].filter((x) => x !== "")) expect(text).not.toContain(v);
  });

  it("ages: seconds, minutes, hours, never", () => {
    const at = (s: number) => BigInt(now - s * 1000) * 1_000_000n;
    expect([ageText(at(0), now), ageText(at(59), now), ageText(at(60), now), ageText(at(3599), now), ageText(at(3600), now), ageText(0n, now)]).toEqual([
      "0s ago",
      "59s ago",
      "1m ago",
      "59m ago",
      "1h ago",
      "never",
    ]);
  });
});

describe("Remove", () => {
  it("asks first, naming what it does; a declined confirm sends nothing", async () => {
    const sent: string[] = [];
    const client = { removeResponder: async (r: { name: string }) => void sent.push(r.name) };
    const asked: string[] = [];
    expect(await removeResponder(client, "app2", (t) => (asked.push(t), false))).toBeUndefined();
    expect(sent).toEqual([]);
    expect(asked).toEqual([removeConfirm("app2")]);
    expect(asked[0]).toContain("credential and loom token stop working");
    expect(asked[0]).toContain("rerun the join to re-add it");
    expect(await removeResponder(client, "app2", () => true)).toBe("removed");
    expect(sent).toEqual(["app2"]);
  });

  it("a refusal says why, in the server's words", async () => {
    const client = { removeResponder: async () => Promise.reject(new ConnectError("no responder app9", Code.NotFound)) };
    expect(await removeResponder(client, "app9", () => true)).toBe("not removed: no responder app9");
  });
});

describe("the pending join tokens", () => {
  it("one row per pending token, its name (or the hostname it will take) and time left; used ones are not pending", () => {
    const rows = tokenRows(tokens, now);
    expect(rows.map((r) => r.name)).toEqual(["app4"]);
    expect(rows[0]?.expiresIn).toBe("9m 55s"); // created at 12:02:00, the default 10m
    expect(tokenRows(tokens, now + 10 * 60 * 1000)[0]?.expiresIn).toBe("expired");
    const expires = Number((tokens[0]?.expiresUnixNano ?? 0n) / 1_000_000n);
    expect(tokenRows(tokens, expires - 1000)[0]?.expiresIn).toBe("1s");
    expect(tokenRows(tokens, expires)[0]?.expiresIn).toBe("expired"); // at the instant, not "0s"
  });

  it("Revoke: revoked; used when a registration came first; otherwise why not", async () => {
    expect(await revokeToken({ revokeJoinToken: async () => ({}) }, "jt-1")).toBe("revoked");
    expect(await revokeToken({ revokeJoinToken: async () => Promise.reject(new ConnectError("was used", Code.FailedPrecondition)) }, "jt-1")).toBe(
      "used: the host joined",
    );
    expect(await revokeToken({ revokeJoinToken: async () => Promise.reject(new ConnectError("unknown", Code.NotFound)) }, "jt-1")).toBe("not revoked: unknown");
  });
});
