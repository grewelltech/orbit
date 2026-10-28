/**
 * PageLinks — the dashboard's pages, chosen by query string (main.tsx): the
 * run dashboard, targets and conformance. The current page is marked, not
 * linked.
 */
export type Page = "dashboard" | "targets" | "conformance";

const PAGES: { page: Page; label: string; href: string }[] = [
  { page: "dashboard", label: "runs", href: "?" },
  { page: "targets", label: "targets", href: "?targets" },
  { page: "conformance", label: "conformance", href: "?conformance" },
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
