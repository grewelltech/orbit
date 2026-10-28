/**
 * The Launch form's inputs: each a view of one path in the run document,
 * writing straight into it through the page's edit (which the run-file
 * gate checks before the edit is kept).
 */
import type { Document } from "yaml";
import { getValue, removeValue, setValue, type Step } from "@/data/targets/yamlio";
import { flag, setFlag } from "@/data/launch/form";
import { FieldRow, ListField, TextField } from "../targets/fields";

export type Edit = (change: (d: Document) => void) => void;

/** A scalar field: text, a number or a duration as the run file writes it. */
export function RunText({
  doc,
  edit,
  steps,
  label,
  placeholder,
  problem,
}: {
  doc: Document;
  edit: Edit;
  steps: Step[];
  label: string;
  placeholder?: string;
  problem?: string;
}) {
  const v = getValue(doc, steps);
  const path = steps.join(".");
  return (
    <FieldRow label={label} path={path} problem={problem}>
      <TextField
        value={typeof v === "string" ? v : ""}
        ariaLabel={path}
        placeholder={placeholder}
        onCommit={(text) => edit((d) => (text.trim() === "" ? removeValue(d, steps) : setValue(d, steps, text.trim())))}
      />
    </FieldRow>
  );
}

/** A list of values, comma-separated. */
export function RunList({ doc, edit, steps, label, placeholder }: { doc: Document; edit: Edit; steps: Step[]; label: string; placeholder?: string }) {
  const v = getValue(doc, steps);
  const path = steps.join(".");
  return (
    <FieldRow label={label} path={path}>
      <ListField value={Array.isArray(v) ? v : typeof v === "string" ? [v] : []} ariaLabel={path} placeholder={placeholder} onCommit={(list) => edit((d) => setValue(d, steps, list))} />
    </FieldRow>
  );
}

/** A bool field: true written, false left out (the run file's default). */
export function RunCheck({ doc, edit, steps, label }: { doc: Document; edit: Edit; steps: Step[]; label: string }) {
  const path = steps.join(".");
  return (
    <FieldRow label={label} path={path}>
      <input type="checkbox" aria-label={path} checked={flag(doc, steps)} onChange={(e) => edit((d) => setFlag(d, steps, e.target.checked))} />
    </FieldRow>
  );
}

/** One of a few values; "" leaves the field out. */
export function RunChoice({ doc, edit, steps, label, options }: { doc: Document; edit: Edit; steps: Step[]; label: string; options: readonly string[] }) {
  const v = getValue(doc, steps);
  const path = steps.join(".");
  return (
    <FieldRow label={label} path={path}>
      <select
        aria-label={path}
        className="o-label w-fit px-2 py-0.5"
        style={{ color: "var(--o-ink-2)", background: "var(--o-surface-2)", border: "1px solid var(--o-border)", textTransform: "none" }}
        value={typeof v === "string" ? v : ""}
        onChange={(e) => edit((d) => (e.target.value ? setValue(d, steps, e.target.value) : removeValue(d, steps)))}
      >
        <option value="">(default)</option>
        {options.map((o) => (
          <option key={o} value={o}>
            {o}
          </option>
        ))}
      </select>
    </FieldRow>
  );
}
