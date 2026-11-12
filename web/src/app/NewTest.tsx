/**
 * NewTest — the chooser at "/new": pick what kind of test to start, then open
 * the Launch editor seeded with the matching template (TEMPLATES in
 * runschema.gen). It is a thin front door over the existing Launch flow — the
 * choice is routed as /launch/:preset, which Launch opens pre-filled.
 */
import { Link } from "react-router";
import { PageLinks } from "./PageLinks";

interface Choice {
  id: string;
  title: string;
  blurb: string;
  to: string;
}

const CHOICES: Choice[] = [
  {
    id: "conformance",
    title: "Conformance",
    blurb: "Run the conformance suite against the target's core — every group, gated on no FAIL, CRASH, BLOCKED, ERROR, DEVIATION or WARN outside the expected-failures list.",
    to: "/launch/conformance",
  },
  {
    id: "load",
    title: "Performance · attach storm",
    blurb: "Subscribers attached and released at a steady rate (the load template), gated on attach success.",
    to: "/launch/load",
  },
  {
    id: "fleet",
    title: "Performance · fleet",
    blurb: "Subscribers attached and kept for the run's duration, with user-plane probing (the fleet template).",
    to: "/launch/fleet",
  },
  {
    id: "blank",
    title: "Custom / mixed",
    blurb: "Start from a blank run file and add population and conformance workloads yourself.",
    to: "/launch/blank",
  },
];

export function NewTest() {
  return (
    <div className="flex h-dvh flex-col overflow-hidden">
      <header
        className="flex items-center gap-6 border-b px-4 py-2.5"
        style={{ borderColor: "var(--o-border)" }}
      >
        <span className="o-label" style={{ color: "var(--o-accent)", fontWeight: 700, letterSpacing: "0.12em" }}>
          orbit
        </span>
        <PageLinks current="launch" />
      </header>

      <main className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4">
        <div>
          <h1 className="o-label" style={{ color: "var(--o-ink)", textTransform: "none", fontSize: "1rem", fontWeight: 600 }}>
            What kind of test?
          </h1>
          <p className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
            Each choice opens the launch editor pre-filled; change anything before you start.
          </p>
        </div>

        <ul className="grid list-none grid-cols-1 gap-3 p-0 md:grid-cols-2" aria-label="test kinds">
          {CHOICES.map((c) => (
            <li key={c.id}>
              <Link
                to={c.to}
                className="flex h-full flex-col gap-1.5 border p-4 transition-colors hover:border-[var(--o-border-accent)]"
                style={{ borderColor: "var(--o-border)", background: "var(--o-surface)", borderRadius: "var(--o-radius)" }}
              >
                <span className="o-label" style={{ color: "var(--o-accent)", textTransform: "none", fontWeight: 600 }}>
                  {c.title}
                </span>
                <span className="o-label" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
                  {c.blurb}
                </span>
              </Link>
            </li>
          ))}
        </ul>
      </main>
    </div>
  );
}
