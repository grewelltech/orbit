import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * The hook-to-helper wiring the tested helpers rely on. The helpers are
 * pinned by their own rows, but a hook that stops calling one would leave
 * them passing; with no DOM in these tests, the hooks' source is read and
 * the calls are asserted where they must be (inside a useEffect, with its
 * cleanup). Comments are stripped first, so a call left only in a comment
 * does not count.
 */
const src = (path: string) =>
  readFileSync(join(__dirname, "..", path), "utf8")
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/(^|[^:])\/\/.*$/gm, "$1");

/** The text of each useEffect(…) call in s, its parentheses balanced. */
function effects(s: string): string[] {
  const out: string[] = [];
  for (let i = s.indexOf("useEffect("); i >= 0; i = s.indexOf("useEffect(", i + 1)) {
    let depth = 0;
    for (let j = i + "useEffect".length; j < s.length; j++) {
      if (s[j] === "(") depth++;
      if (s[j] === ")" && --depth === 0) {
        out.push(s.slice(i, j + 1));
        break;
      }
    }
  }
  return out;
}

const effectMatching = (s: string, ...res: RegExp[]) => effects(s).some((e) => res.every((re) => re.test(e)));

describe("hook-to-helper wiring", () => {
  it("useProbe: the page lifecycle (pagehide ends the stream, a bfcache restore resets it) is wired in an effect", () => {
    expect(effectMatching(src("data/targets/useProbe.ts"), /^useEffect\(\s*\(\)\s*=>\s*probePageLifecycle\(\s*window\s*,\s*runner\s*,/)).toBe(true);
  });

  it("useProbe: leaving the page or another target ends the stream (the effect's cleanup disposes the runner)", () => {
    expect(effectMatching(src("data/targets/useProbe.ts"), /return\s*\(\)\s*=>\s*runner\.dispose\(\)/, /\[\s*runner\s*,\s*targetName\s*\]\s*\)$/)).toBe(true);
  });

  it("useLaunch: unmounting disposes the Launch runner", () => {
    expect(effectMatching(src("data/launch/useLaunch.ts"), /^useEffect\(\s*\(\)\s*=>\s*\(\)\s*=>\s*runner\.dispose\(\)/)).toBe(true);
  });

  it("SuiteResults: the suite watch is disposed when the run changes or the view goes", () => {
    expect(effectMatching(src("app/SuiteResults.tsx"), /new SuiteWatch\(/, /return\s*\(\)\s*=>\s*w\.dispose\(\)/)).toBe(true);
  });

  it("Launch: whether it may start is startStateFor's StartState, judged by whyNotStart, and both start buttons obey it", () => {
    const s = src("app/Launch.tsx");
    expect(s).toMatch(/const\s*\{\s*state:\s*start\b[^}]*\}\s*=\s*startStateFor\(/);
    expect(s).toMatch(/const\s+why\s*=\s*whyNotStart\(\s*start\s*\)/);
    expect(s.match(/disabled=\{why !== undefined/g)?.length ?? 0).toBeGreaterThanOrEqual(2);
  });

  it("the reader sees through no comment: a call left only in a comment is not wiring", () => {
    expect(effects(src("data/targets/useProbe.ts")).join("\n")).not.toContain("Leaving the page");
  });
});
