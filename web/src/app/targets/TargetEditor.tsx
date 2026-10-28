/**
 * TargetEditor — every field of one target, in the order a target file has
 * them, from the schema Go generates (schema.gen.ts): a field added to the
 * CLI's target appears here without a change to this file.
 */
import type { Document } from "yaml";
import { Panel } from "@/components/Panel";
import { TARGET_FIELDS, type FieldSpec } from "@/data/targets/schema.gen";
import {
  addElement,
  canonicalPath,
  defaultFor,
  getSlices,
  getValue,
  listLength,
  removeElement,
  setSlices,
  type Issue,
  type Step,
} from "@/data/targets/yamlio";
import type { Editing } from "@/data/targets/useTargets";
import { buttonStyle, FieldRow, ListField, SecretField, TextField } from "./fields";

export interface TargetEditorProps {
  editing: Editing;
  edit: (change: (doc: Document) => void) => void;
  setField: (steps: readonly Step[], value: string | readonly string[]) => void;
  setSecret: (steps: readonly Step[], current: string, value: string) => void;
}

const SECTIONS: { title: string; prefix: string; list?: string }[] = [
  { title: "core", prefix: "core." },
  { title: "subscribers", prefix: "subscribers[].", list: "subscribers" },
  { title: "ran", prefix: "ran." },
  { title: "responders", prefix: "app.responders[].", list: "app.responders" },
];

/** The steps of a field pattern, element i of its list where it has one. */
function stepsOf(pattern: string, i?: number): Step[] {
  const [list, rest] = pattern.includes("[].") ? (pattern.split("[].") as [string, string]) : [undefined, pattern];
  if (list === undefined) return rest.split(".");
  return [...list.split("."), i ?? 0, ...rest.split(".")];
}

/** What the default is, as a placeholder. */
function placeholderFor(f: FieldSpec): string | undefined {
  const d = defaultFor(f.path);
  if (typeof d === "string") return `default ${d}`;
  return undefined;
}

export function TargetEditor({ editing, edit, setField, setSecret }: TargetEditorProps) {
  const { doc, issues, keys, remembered } = editing;
  const problems = new Map<string, string>();
  for (const i of issues) if (!problems.has(i.path)) problems.set(i.path, i.message);

  const field = (f: FieldSpec, i?: number) => {
    const steps = stepsOf(f.path, i);
    const path = canonicalPath(doc, steps);
    const key = f.path.slice(f.path.lastIndexOf(".") + 1);
    const label = f.path.startsWith("core.plmn.") ? `plmn ${key}` : f.path.includes("gnb_ids.") ? `gnb_ids ${key}` : key;
    const problem = problems.get(path);
    if (f.type === "slices") {
      const slices = getSlices(doc, steps);
      const shown = slices.length > 0 ? slices : [];
      return (
        <FieldRow key={path} label="slices" path={path} problem={problem ?? [...problems].find(([p]) => p.startsWith(path + "["))?.[1]}>
          <span className="flex flex-col gap-1">
            {shown.map((s, n) => (
              <span key={n} className="grid grid-cols-[4rem_1fr_auto] gap-1">
                <TextField value={s.sst} ariaLabel={`slice ${n} sst`} placeholder="sst" onCommit={(v) => edit((d) => setSlices(d, steps, shown.map((x, m) => (m === n ? { ...x, sst: v } : x))))} />
                <TextField value={s.sd} ariaLabel={`slice ${n} sd`} placeholder="sd (6 hex digits, optional)" onCommit={(v) => edit((d) => setSlices(d, steps, shown.map((x, m) => (m === n ? { ...x, sd: v } : x))))} />
                <button type="button" className="o-label cursor-pointer px-2" style={buttonStyle} aria-label={`remove slice ${n}`} onClick={() => edit((d) => setSlices(d, steps, shown.filter((_, m) => m !== n)))}>
                  ×
                </button>
              </span>
            ))}
            <span className="flex items-center gap-2">
              <button type="button" className="o-label cursor-pointer px-2 py-0.5" style={buttonStyle} onClick={() => edit((d) => setSlices(d, steps, [...shown, { sst: "1" }]))}>
                add slice
              </button>
              {shown.length === 0 && (
                <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
                  default sst 1
                </span>
              )}
            </span>
          </span>
        </FieldRow>
      );
    }
    const v = getValue(doc, steps);
    if (f.secret) {
      const docValue = typeof v === "string" ? v : "";
      const variable = /^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$/.exec(docValue)?.[1];
      return (
        <FieldRow key={path} label={key} path={path} problem={problem}>
          <SecretField
            docValue={docValue}
            held={variable ? keys[variable] : undefined}
            remembered={remembered}
            ariaLabel={path}
            placeholder={key === "token" ? "the responder's loom token, kept out of the file" : "32 hex digits, kept out of the file"}
            onValue={(value) => setSecret(steps, docValue, value)}
          />
        </FieldRow>
      );
    }
    if (f.type === "strings") {
      return (
        <FieldRow key={path} label={key} path={path} problem={problem ?? [...problems].find(([p]) => p.startsWith(path + "["))?.[1]}>
          <ListField value={Array.isArray(v) ? v : typeof v === "string" ? [v] : []} ariaLabel={path} onCommit={(list) => setField(steps, list)} />
        </FieldRow>
      );
    }
    return (
      <FieldRow key={path} label={label} path={path} problem={problem}>
        <TextField value={typeof v === "string" ? v : ""} ariaLabel={path} placeholder={placeholderFor(f)} onCommit={(value) => setField(steps, value)} />
      </FieldRow>
    );
  };

  const nameProblem = problems.get("name");

  return (
    <Panel title={`target ${editing.name}`} meta={issues.length === 0 ? "no problems found" : `${issues.length} problem${issues.length === 1 ? "" : "s"}`}>
      <div className="flex flex-col gap-4">
        <FieldRow label="name" path="name" problem={nameProblem}>
          <TextField value={(getValue(doc, ["name"]) as string | undefined) ?? ""} ariaLabel="name" onCommit={(v) => setField(["name"], v)} />
        </FieldRow>
        {/* Every problem, listed here as well as at its field: some (an unknown key) have no field. */}
        {issues.map((i: Issue) => (
          <span key={i.path + i.message} className="o-label" role="alert" style={{ color: "var(--o-error)", textTransform: "none" }}>
            {i.path}: {i.message}
          </span>
        ))}
        {SECTIONS.map((sec) => {
          const fs = TARGET_FIELDS.filter((f) => f.editable && f.path.startsWith(sec.prefix));
          if (!sec.list) {
            return (
              <section key={sec.title} aria-label={sec.title}>
                <h3 className="o-label pb-1" style={{ color: "var(--o-ink-2)" }}>
                  {sec.title}
                </h3>
                {fs.map((f) => field(f))}
              </section>
            );
          }
          const listSteps = sec.list.split(".");
          const n = listLength(doc, listSteps);
          return (
            <section key={sec.title} aria-label={sec.title}>
              <h3 className="flex items-center gap-3 pb-1">
                <span className="o-label" style={{ color: "var(--o-ink-2)" }}>
                  {sec.title}
                </span>
                <button
                  type="button"
                  className="o-label cursor-pointer px-2 py-0.5"
                  style={buttonStyle}
                  onClick={() => edit((d) => void addElement(d, listSteps, sec.list === "subscribers" ? (n === 0 ? "main" : `pool${n + 1}`) : n === 0 ? "n6" : `n6-${n + 1}`))}
                >
                  add
                </button>
                {n > 0 && (
                  <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
                    a probe uses the first
                  </span>
                )}
              </h3>
              {Array.from({ length: n }, (_, i) => (
                <div key={i} className="mb-2 border-l pl-3" style={{ borderColor: "var(--o-border)" }}>
                  {fs.map((f) => field(f, i))}
                  <button type="button" className="o-label cursor-pointer px-2 py-0.5" style={buttonStyle} onClick={() => edit((d) => removeElement(d, listSteps, i))}>
                    remove
                  </button>
                </div>
              ))}
            </section>
          );
        })}
      </div>
    </Panel>
  );
}
