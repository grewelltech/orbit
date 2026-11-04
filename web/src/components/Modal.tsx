/**
 * Modal — a centered overlay for content that is worth keeping off the main
 * view until asked for. Closes on Escape, on a backdrop click, and on the
 * close button; the dialog traps nothing but takes initial focus so Escape
 * and Tab land somewhere sensible.
 */
import { useEffect, useRef, type ReactNode } from "react";

export function Modal({
  label,
  onClose,
  children,
}: {
  /** Accessible name for the dialog (not drawn — the content carries its own title). */
  label: string;
  onClose: () => void;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    ref.current?.focus();
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-[var(--o-bg)]/80 p-4 backdrop-blur-sm sm:p-8"
      role="presentation"
      // A mousedown that both starts and ends on the backdrop closes; starting
      // inside the dialog and releasing outside (a drag-select) does not.
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        ref={ref}
        role="dialog"
        aria-modal="true"
        aria-label={label}
        tabIndex={-1}
        className="w-full max-w-4xl outline-none"
      >
        <div className="mb-2 flex justify-end">
          <button
            type="button"
            onClick={onClose}
            className="o-label cursor-pointer border px-2 py-0.5 transition-colors"
            style={{
              color: "var(--o-ink-2)",
              background: "var(--o-surface)",
              borderColor: "var(--o-border)",
              borderRadius: "var(--o-radius)",
              transitionDuration: "var(--o-dur-fast)",
            }}
            aria-label="close"
          >
            close ✕
          </button>
        </div>
        {children}
      </div>
    </div>
  );
}
