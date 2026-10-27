/**
 * TokenPrompt — asks for the server's bearer token when an API call is
 * refused as unauthenticated (orbit serve --token-file), and keeps it for the
 * tab (see data/transport). The dashboards poll, so the next call after the
 * token is saved carries it; nothing needs reloading.
 */
import { useEffect, useState, type FormEvent } from "react";
import { authRequired, saveToken, type AuthRefusal } from "@/data/transport";

const TITLES: Record<AuthRefusal, string> = {
  missing: "this server needs a token",
  invalid: "the server rejected this token",
  locked: "too many wrong tokens from this address: the right one still works",
};

export function TokenPrompt() {
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState("");
  const [why, setWhy] = useState<AuthRefusal>("missing");

  useEffect(
    () =>
      authRequired.subscribe((w) => {
        setWhy(w);
        setOpen(true);
      }),
    [],
  );

  if (!open) return null;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!value.trim()) return;
    saveToken(value);
    setValue("");
    setOpen(false);
  };

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="orbit-token-title"
      className="fixed inset-0 z-50 flex items-center justify-center p-4"
      style={{ background: "rgba(0, 0, 0, 0.55)" }}
    >
      <form
        onSubmit={submit}
        className="flex w-full max-w-md flex-col gap-3 p-4"
        style={{
          background: "var(--o-surface)",
          border: "1px solid var(--o-border)",
          borderRadius: "var(--o-radius)",
        }}
      >
        <span id="orbit-token-title" className="o-label" style={{ color: "var(--o-ink)" }}>
          {TITLES[why]}
        </span>
        <span className="text-sm" style={{ color: "var(--o-ink-3)" }}>
          Paste the bearer token orbit serve was started with (the first line of its --token-file). It is kept
          for this tab only.
        </span>
        <input
          type="password"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          autoFocus
          autoComplete="off"
          spellCheck={false}
          aria-label="bearer token"
          className="o-label px-2 py-1"
          style={{
            color: "var(--o-ink-2)",
            background: "var(--o-surface-2)",
            border: "1px solid var(--o-border)",
            borderRadius: "var(--o-radius)",
          }}
        />
        <button
          type="submit"
          disabled={!value.trim()}
          className="o-label cursor-pointer self-end px-3 py-1"
          style={{ color: "var(--o-bg)", background: "var(--o-accent)", borderRadius: "var(--o-radius)" }}
        >
          use token
        </button>
      </form>
    </div>
  );
}
