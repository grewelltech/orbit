import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * The hook-to-helper wiring the tested helpers rely on. The helpers are
 * pinned by their own rows, but a hook that stops calling one would leave
 * them passing; with no DOM in these tests, the hooks' source is read and
 * each pinned effect is taken apart: its callback and its deps (asserted
 * whole), an expression body that must be exactly the helper's call (so
 * `… && undefined` dropping the remover fails), a block body whose cleanup
 * must be its last statement and its only return after the helper is made.
 * Comments and the insides of string and template literals are stripped
 * first, so a call left in either does not count; a helper must be imported
 * unaliased from its module and not defined again in the file.
 */
function strip(s: string): string {
  return s
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/(^|[^:])\/\/.*$/gm, "$1")
    .replace(/"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'|`(?:[^`\\]|\\.)*`/g, '""');
}
const read = (path: string) => readFileSync(join(__dirname, "..", path), "utf8");
const src = (path: string) => strip(read(path));
/** s with all whitespace removed, for exact comparison. */
const tight = (s: string) => s.replace(/\s+/g, "");

/** s split at its top-level commas (outside (), [] and {}). */
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
  /** The callback's body: an expression, or a block's inside. */
  body: string;
  block: boolean;
  /** The deps array, whitespace removed. */
  deps: string;
}

/** Each useEffect(callback, deps) in s, taken apart. */
function effects(s: string): Effect[] {
  const out: Effect[] = [];
  for (let i = s.indexOf("useEffect("); i >= 0; i = s.indexOf("useEffect(", i + 1)) {
    let depth = 0;
    for (let j = i + "useEffect".length; j < s.length; j++) {
      if (s[j] === "(") depth++;
      if (s[j] === ")" && --depth === 0) {
        const [cb = "", deps = ""] = topSplit(s.slice(i + "useEffect(".length, j));
        const arrow = cb.match(/^\(\)\s*=>\s*([\s\S]*)$/);
        const body = (arrow?.[1] ?? "").trim();
        const block = body.startsWith("{") && body.endsWith("}");
        out.push({ body: block ? body.slice(1, -1).trim() : body, block, deps: tight(deps) });
        break;
      }
    }
  }
  return out;
}

/** Whether name is imported unaliased from module in s (the unstripped source: the module is a literal), and not defined there again. */
function imported(s: string, name: string, module: string): boolean {
  const fromModule = new RegExp(`import\\s*\\{[^}]*\\b${name}\\b(?!\\s+as\\b)[^}]*\\}\\s*from\\s*"${module.replace(/[./]/g, "\\$&")}"`).test(s);
  const shadowed = new RegExp(`\\b(function|const|let|var|class)\\s+${name}\\b`).test(s) || new RegExp(`\\bas\\s+${name}\\b`).test(s);
  return fromModule && !shadowed;
}

/** A block's cleanup: the block must end with `return <cleanup>;`, the only return after marker (or in the block, with no marker). */
function cleanupIs(e: Effect, cleanup: string, marker?: string): boolean {
  if (!e.block) return false;
  const from = marker === undefined ? 0 : e.body.indexOf(marker);
  if (from < 0) return false;
  const tail = e.body.slice(from);
  return (tail.match(/\breturn\b/g)?.length ?? 0) === 1 && tight(tail).endsWith(tight(`return ${cleanup};`));
}

/** Each <button>…</button> in s. */
function buttons(s: string): string[] {
  const out: string[] = [];
  for (let i = s.indexOf("<button"); i >= 0; i = s.indexOf("<button", i + 1)) {
    const end = s.indexOf("</button>", i);
    if (end > i) out.push(s.slice(i, end));
  }
  return out;
}

/** The expression of attribute name={…} in a JSX element's text, its braces balanced. */
function attr(el: string, name: string): string | undefined {
  const at = el.search(new RegExp(`\\b${name}=\\{`));
  if (at < 0) return undefined;
  const open = el.indexOf("{", at);
  let depth = 0;
  for (let j = open; j < el.length; j++) {
    if (el[j] === "{") depth++;
    if (el[j] === "}" && --depth === 0) return el.slice(open + 1, j);
  }
  return undefined;
}

describe("hook-to-helper wiring", () => {
  const useProbe = read("data/targets/useProbe.ts");
  const probeEffects = effects(strip(useProbe));

  it("useProbe: the page lifecycle is an effect that is exactly probePageLifecycle(…), its remover the cleanup, on [runner]", () => {
    expect(probeEffects.some((e) => !e.block && tight(e.body) === "probePageLifecycle(window,runner,()=>setState(IDLE))" && e.deps === "[runner]")).toBe(true);
    expect(imported(useProbe, "probePageLifecycle", "./probe")).toBe(true);
  });

  it("useProbe: another target or unmounting ends the stream: the effect's only return is the dispose, on [runner, targetName]", () => {
    expect(probeEffects.some((e) => cleanupIs(e, "() => runner.dispose()") && e.deps === "[runner,targetName]")).toBe(true);
  });

  it("useLaunch: unmounting disposes the Launch runner, on [runner]", () => {
    expect(effects(src("data/launch/useLaunch.ts")).some((e) => !e.block && tight(e.body) === "()=>runner.dispose()" && e.deps === "[runner]")).toBe(true);
  });

  it("SuiteResults: once the watch is made, the effect's only return is its dispose, on [client, runId]", () => {
    const s = read("app/SuiteResults.tsx");
    expect(effects(strip(s)).some((e) => cleanupIs(e, "() => w.dispose()", "new SuiteWatch(") && e.deps === "[client,runId]")).toBe(true);
    expect(imported(s, "SuiteWatch", "@/data/suite")).toBe(true);
  });

  it("Launch: whether it may start is startStateFor's StartState, judged by whyNotStart, and both start buttons obey it", () => {
    const raw = read("app/Launch.tsx");
    const s = strip(raw);
    expect(s).toMatch(/const\s*\{\s*state:\s*start\b[^}]*\}\s*=\s*startStateFor\(/);
    expect(s).toMatch(/const\s+why\s*=\s*whyNotStart\(\s*start\s*\)\s*;/);
    // Both start buttons (start, check the plan): their whole disabled
    // expression, not a prefix of it.
    const disabled = buttons(s)
      .filter((b) => /^\(\)=>voidl\.(start|plan)\(\)$/.test(tight(attr(b, "onClick") ?? "")))
      .map((b) => tight(attr(b, "disabled") ?? ""));
    expect(disabled).toEqual(["why!==undefined||s.busy!==undefined", "why!==undefined||s.busy!==undefined"]);
    expect(imported(raw, "startStateFor", "@/data/launch/resolve")).toBe(true);
    expect(imported(raw, "whyNotStart", "@/data/launch/resolve")).toBe(true);
  });

  it("the reader: a button's attributes come out whole", () => {
    const b = buttons('<button disabled={a || (b && c)} onClick={() => f({ x: 1 })}>x</button><button>y</button>');
    expect(b).toHaveLength(2);
    expect(attr(b[0] ?? "", "disabled")).toBe("a || (b && c)");
    expect(attr(b[0] ?? "", "onClick")).toBe("() => f({ x: 1 })");
    expect(attr(b[1] ?? "", "disabled")).toBeUndefined();
  });

  it("the reader: comments and literals are stripped; deps and bodies come apart", () => {
    expect(strip('a(/* x */ "probePageLifecycle(") // y').trim()).toBe('a( "")');
    expect(effects("useEffect(() => { f(); return () => g(); }, [a, b]);")).toEqual([{ body: "f(); return () => g();", block: true, deps: "[a,b]" }]);
    expect(effects("useEffect(() => h(x, y) && undefined, [])")).toEqual([{ body: "h(x, y) && undefined", block: false, deps: "[]" }]);
  });
});
