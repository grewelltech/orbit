/**
 * LaunchChooser — the first step of /launch: pick the kind of run to build.
 *
 * Each choice opens the Launch editor (/launch/edit) on a template; "mixed"
 * opens it on an empty run file, to which population and conformance
 * workloads are added. /launch/edit?... without a preset resumes the tab's
 * draft.
 */
import { PageLinks } from "./PageLinks";

export const KINDS: { id: string; title: string; blurb: string; href: string }[] = [
  { id: "population", title: "population: attach storm", blurb: "subscribers attached and released at a steady rate (template load)", href: "/launch/load" },
  { id: "fleet", title: "population: fleet", blurb: "subscribers attached and kept for the run's duration (template fleet)", href: "/launch/fleet" },
  { id: "conformance", title: "conformance", blurb: "the conformance suite against the target's core", href: "/conformance" },
  { id: "mixed", title: "mixed", blurb: "an empty run file: add population and conformance workloads yourself", href: "/launch/mixed" },
];

export function LaunchChooser() {
  return (
    <div className="flex h-dvh flex-col overflow-hidden">
      <header className="flex items-center gap-6 border-b border-[var(--o-border)] px-4 py-2.5">
        <span className="o-label" style={{ color: "var(--o-ink)" }}>
          orbit
        </span>
        <PageLinks current="launch" />
      </header>
      <main className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-3">
        <h1 className="o-label" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
          what kind of run?
        </h1>
        <ul className="grid list-none grid-cols-1 gap-3 p-0 md:grid-cols-2" aria-label="run kinds">
          {KINDS.map((k) => (
            <li key={k.id}>
              <a
                href={k.href}
                className="flex h-full flex-col gap-1 border p-3"
                style={{ borderColor: "var(--o-border)", background: "var(--o-surface)", borderRadius: "var(--o-radius)" }}
              >
                <span className="o-label" style={{ color: "var(--o-accent)", textTransform: "none" }}>
                  {k.title}
                </span>
                <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
                  {k.blurb}
                </span>
              </a>
            </li>
          ))}
        </ul>
        <a href="/launch/edit" className="o-label" style={{ color: "var(--o-ink-3)" }}>
          continue the draft in this tab
        </a>
      </main>
    </div>
  );
}
