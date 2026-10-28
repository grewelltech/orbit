/**
 * The editor's inputs: one per target field, typed as the schema says, each
 * writing straight into the document.
 */
import { useEffect, useState, type CSSProperties } from "react";
import { refName } from "@/data/targets/yamlio";

export const inputStyle: CSSProperties = {
  color: "var(--o-ink-2)",
  background: "var(--o-surface-2)",
  border: "1px solid var(--o-border)",
  borderRadius: "var(--o-radius)",
};

export const buttonStyle: CSSProperties = {
  color: "var(--o-ink-2)",
  border: "1px solid var(--o-border)",
  borderRadius: "var(--o-radius)",
};

/** A labelled row: the field's key, its input, and its problem if any. */
export function FieldRow({ label, path, problem, children }: { label: string; path: string; problem?: string; children: React.ReactNode }) {
  return (
    <label className="grid grid-cols-[10rem_1fr] items-start gap-2 py-1" data-path={path}>
      <span className="o-label pt-1" style={{ color: problem ? "var(--o-error)" : "var(--o-ink-3)", textTransform: "none" }} title={path}>
        {label}
      </span>
      <span className="flex min-w-0 flex-col gap-0.5">
        {children}
        {problem && (
          <span className="o-label" role="alert" style={{ color: "var(--o-error)", textTransform: "none" }}>
            {problem}
          </span>
        )}
      </span>
    </label>
  );
}

/**
 * A text input that keeps what is typed and commits it as it changes; it
 * follows the document when the document changes under it (an import,
 * another tab).
 */
export function TextField({
  value,
  onCommit,
  placeholder,
  ariaLabel,
  type = "text",
}: {
  value: string;
  onCommit: (v: string) => void;
  placeholder?: string;
  ariaLabel: string;
  type?: "text" | "password";
}) {
  const [text, setText] = useState(value);
  useEffect(() => setText(value), [value]);
  return (
    <input
      type={type}
      value={text}
      placeholder={placeholder}
      aria-label={ariaLabel}
      spellCheck={false}
      autoComplete="off"
      onChange={(e) => {
        setText(e.target.value);
        onCommit(e.target.value);
      }}
      className="o-label w-full px-2 py-1"
      style={{ ...inputStyle, textTransform: "none" }}
    />
  );
}

/** A list of strings as one comma-separated input. */
export function ListField({ value, onCommit, ariaLabel, placeholder }: { value: string[]; onCommit: (v: string[]) => void; ariaLabel: string; placeholder?: string }) {
  return (
    <TextField
      value={value.join(", ")}
      ariaLabel={ariaLabel}
      placeholder={placeholder ?? "comma-separated"}
      onCommit={(v) =>
        onCommit(
          v
            .split(",")
            .map((s) => s.trim())
            .filter((s) => s !== ""),
        )
      }
    />
  );
}

/**
 * A key or token: the document holds its ${VAR}; the value is held in this
 * tab (or this browser, when the target remembers keys) and never written
 * into the document.
 */
export function SecretField({
  docValue,
  held,
  remembered,
  onValue,
  ariaLabel,
  placeholder,
}: {
  docValue: string;
  held: string | undefined;
  remembered: boolean;
  onValue: (v: string) => void;
  ariaLabel: string;
  placeholder: string;
}) {
  const variable = refName(docValue);
  const where = held === undefined || held === "" ? "not set" : remembered ? "remembered in this browser" : "held in this tab";
  return (
    <span className="flex flex-col gap-0.5">
      <TextField type="password" value={held ?? ""} onCommit={onValue} ariaLabel={ariaLabel} placeholder={placeholder} />
      <span className="o-label" style={{ color: held ? "var(--o-ink-3)" : "var(--o-amber)", textTransform: "none" }}>
        {variable ? `\${${variable}}` : "no variable yet"} · {where}
      </span>
    </span>
  );
}
