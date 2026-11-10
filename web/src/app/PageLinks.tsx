/**
 * PageLinks — the dashboard's pages, routes (main.tsx): home,
 * the run dashboard (/runs), launch and targets (/conformance opens launch on the
 * conformance suite). The current page is marked, not
 * linked.
 */
export type Page = "home" | "dashboard" | "launch" | "targets";

const PAGES: { page: Page; label: string; href: string }[] = [
  { page: "home", label: "home", href: "/" },
  { page: "dashboard", label: "runs", href: "/runs" },
  { page: "launch", label: "launch", href: "/launch" },
  { page: "targets", label: "targets", href: "/targets" },
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
          <a key={p.page} href={p.href} className="o-label" style={{ color: "var(--o-ink-3)" }}>
            {p.label}
          </a>
        ),
      )}
    </nav>
  );
}
