import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * useJoin's and useResponders' wiring to their helpers, read from their
 * source (no DOM here): the helpers' own rows pass whether or not a hook
 * calls them. Comments and the insides of string and template literals are
 * stripped; each pinned effect is split into its callback and deps (deps
 * asserted whole), an expression body must be exactly the helper's call,
 * a block's cleanup its last statement and only return, and a helper must
 * be imported unaliased and not defined again (the guard #237 put on the
 * other hooks).
 */
function strip(s: string): string {
  return s
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/(^|[^:])\/\/.*$/gm, "$1")
    .replace(/"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'|`(?:[^`\\]|\\.)*`/g, '""');
}
const read = (file: string) => readFileSync(join(__dirname, file), "utf8");
const tight = (s: string) => s.replace(/\s+/g, "");

function topSplit(s: string): string[] {
  const out: string[] = [];
  let depth = 0;
  let from = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s[i];
    if (c === "(" || c === "[" || c === "{") depth++;
    else if (c === ")" || c === "]" || c === "}") depth--;
    else if (c === "," && depth === 0) {
      out.push(s.slice(from, i));
      from = i + 1;
    }
  }
  out.push(s.slice(from));
  return out.map((x) => x.trim()).filter((x) => x !== "");
}

interface Effect {
  body: string;
  block: boolean;
  deps: string;
}

function effects(s: string): Effect[] {
  const out: Effect[] = [];
  for (let i = s.indexOf("useEffect("); i >= 0; i = s.indexOf("useEffect(", i + 1)) {
    let depth = 0;
    for (let j = i + "useEffect".length; j < s.length; j++) {
      if (s[j] === "(") depth++;
      if (s[j] === ")" && --depth === 0) {
        const [cb = "", deps = ""] = topSplit(s.slice(i + "useEffect(".length, j));
        const body = (cb.match(/^\(\)\s*=>\s*([\s\S]*)$/)?.[1] ?? "").trim();
        const block = body.startsWith("{") && body.endsWith("}");
        out.push({ body: block ? body.slice(1, -1).trim() : body, block, deps: tight(deps) });
        break;
      }
    }
  }
  return out;
}

function imported(s: string, name: string, module: string): boolean {
  const fromModule = new RegExp(`import\\s*\\{[^}]*\\b${name}\\b(?!\\s+as\\b)[^}]*\\}\\s*from\\s*"${module.replace(/[./]/g, "\\$&")}"`).test(s);
  const shadowed = new RegExp(`\\b(function|const|let|var|class)\\s+${name}\\b`).test(s) || new RegExp(`\\bas\\s+${name}\\b`).test(s);
  return fromModule && !shadowed;
}

function cleanupIs(e: Effect, cleanup: string, marker?: string): boolean {
  if (!e.block) return false;
  const from = marker === undefined ? 0 : e.body.indexOf(marker);
  if (from < 0) return false;
  const tail = e.body.slice(from);
  return (tail.match(/\breturn\b/g)?.length ?? 0) === 1 && tight(tail).endsWith(tight(`return ${cleanup};`));
}

describe("useJoin's wiring", () => {
  const raw = read("useJoin.ts");
  const fx = effects(strip(raw));
  it("the page going revokes a live token: an effect that is exactly joinPageLifecycle(…) with a keepalive revoke, on [session, baseUrl]", () => {
    expect(
      fx.some(
        (e) =>
          !e.block &&
          tight(e.body) === "joinPageLifecycle(window,session,(id)=>revokeOnHide(fetch,baseUrl??window.location.origin,loadToken(),id))" &&
          e.deps === "[session,baseUrl]",
      ),
    ).toBe(true);
    expect(imported(raw, "joinPageLifecycle", "./session")).toBe(true);
    expect(imported(raw, "revokeOnHide", "./session")).toBe(true);
  });
  it("another target, or the panel gone, closes the session (an unused token revoked), on [session, targetName]", () => {
    expect(fx.some((e) => !e.block && tight(e.body) === "()=>voidsession.close()" && e.deps === "[session,targetName]")).toBe(true);
  });
  it("the countdown ticks the session, its interval cleared as the only return, on [session]", () => {
    expect(fx.some((e) => e.body.includes("session.tick()") && cleanupIs(e, "() => clearInterval(t)", "setInterval(") && e.deps === "[session]")).toBe(true);
  });
  it("the polled responders mark the token used, on [session, responders]", () => {
    expect(fx.some((e) => !e.block && tight(e.body) === "session.sawResponders(responders)" && e.deps === "[session,responders]")).toBe(true);
  });
});

describe("useResponders' wiring", () => {
  const r = strip(read("useResponders.ts"));
  it("a Remove already in flight asks nothing: the guard comes before the confirm, and ends in finally", () => {
    const start = r.indexOf("if (!inflight.start(name)) return undefined;");
    const ask = r.indexOf("removeResponder(responders, name,");
    const end = r.indexOf("finally {");
    expect(start).toBeGreaterThan(0);
    expect(ask).toBeGreaterThan(start);
    expect(end).toBeGreaterThan(ask);
    expect(r.slice(end)).toMatch(/^finally\s*\{\s*inflight\.end\(name\);/);
  });
});

describe("the reader", () => {
  it("strips comments and literals; takes effects apart", () => {
    expect(strip('a(/* x */ "joinPageLifecycle(") // y').trim()).toBe('a( "")');
    expect(effects("useEffect(() => h(x) && undefined, [])")).toEqual([{ body: "h(x) && undefined", block: false, deps: "[]" }]);
  });
});
