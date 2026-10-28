/**
 * PageLinks — the dashboard's pages, chosen by query string (main.tsx): the
 * run dashboard, launch and targets (?conformance opens launch on the
 * conformance suite). The current page is marked, not
 * linked.
 */
export type Page = "dashboard" | "launch" | "targets";

const PAGES: { page: Page; label: string; href: string }[] = [
  { page: "dashboard", label: "runs", href: "?" },
  { page: "launch", label: "launch", href: "?launch" },
  { page: "targets", label: "targets", href: "?targets" },
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
