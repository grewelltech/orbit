/**
 * PageLinks — the top-level navigation, as router links (ADR-0010): home (run
 * history), runs (the dashboard), new test (the chooser) and targets. The
 * current page is marked, not linked.
 */
import { Link } from "react-router";

export type Page = "home" | "dashboard" | "launch" | "targets";

const PAGES: { page: Page; label: string; to: string }[] = [
  { page: "home", label: "home", to: "/" },
  { page: "dashboard", label: "runs", to: "/runs" },
  { page: "launch", label: "new test", to: "/new" },
  { page: "targets", label: "targets", to: "/targets" },
];

export function PageLinks({ current }: { current: Page }) {
  return (
    <nav aria-label="pages" className="flex items-center gap-3">
      {PAGES.map((p) =>
        p.page === current ? (
          <span key={p.page} className="o-label" aria-current="page" style={{ color: "var(--o-ink)" }}>
            {p.label}
          </span>
        ) : (
          <Link key={p.page} to={p.to} className="o-label" style={{ color: "var(--o-ink-3)" }}>
            {p.label}
          </Link>
        ),
      )}
    </nav>
  );
}
