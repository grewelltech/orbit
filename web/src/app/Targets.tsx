/**
 * The targets view: create and edit targets in the browser, import and
 * export them as the CLI's target files.
 *
 * Reached at ?targets. Targets are kept in this browser (localStorage),
 * each as its file's text; keys and tokens are kept in this tab
 * (sessionStorage) unless the user asks for a target's keys to be
 * remembered. The server stores nothing. The CLI's environment overrides
 * ($ORBIT_MCC, $ORBIT_KI …) do not apply here: the browser has no
 * environment.
 */
import { useRef, useState } from "react";
import { Panel } from "@/components/Panel";
import { StatusDot } from "@/components/StatusDot";
import { useTargets, type ExportFile } from "@/data/targets/useTargets";
import { documentRefs, MAX_FILE_BYTES, refName, secretSlots, TOO_LARGE } from "@/data/targets/yamlio";
import { PageLinks } from "./PageLinks";
import { buttonStyle, inputStyle } from "./targets/fields";
import { TargetEditor } from "./targets/TargetEditor";

/** Hands the browser a file to save. */
function download(f: ExportFile) {
  const url = URL.createObjectURL(new Blob([f.text], { type: "application/yaml" }));
  const a = document.createElement("a");
  a.href = url;
  a.download = f.fileName;
  a.click();
  // After the click has had its turn: revoking at once can cancel the save.
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

export function Targets() {
  const t = useTargets();
  const [paste, setPaste] = useState("");
  const [replace, setReplace] = useState(false);
  const [withKeys, setWithKeys] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);
  const e = t.editing;

  const unset = e
    ? documentRefs(e.doc).filter((v) => {
        const secret = secretSlots(e.doc).some((s) => refName(s.value) === v);
        return secret && !e.keys[v];
      })
    : [];

  return (
    <div className="flex h-dvh flex-col overflow-hidden" style={{ background: "var(--o-bg)" }}>
      <header
        className="flex flex-wrap items-center gap-x-6 gap-y-2 border-b px-4 py-2.5"
        style={{ borderColor: "var(--o-border)", background: "var(--o-surface)" }}
      >
        <span style={{ fontFamily: "var(--o-font-mono)", fontWeight: 600, letterSpacing: "var(--o-tracking-title)", color: "var(--o-ink)" }}>ORBIT</span>
        <PageLinks current="targets" />
        <span className="o-label ml-auto" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
          kept in this browser; keys in this tab. The CLI's $ORBIT_* overrides do not apply here.
        </span>
      </header>

      {t.notices.length > 0 && (
        <div className="flex items-start gap-3 border-b px-4 py-2" role="status" style={{ borderColor: "var(--o-border)", background: "var(--o-surface-2)" }}>
          <ul className="flex-1">
            {t.notices.map((n) => (
              <li key={n} className="o-label" style={{ color: "var(--o-amber)", textTransform: "none" }}>
                {n}
              </li>
            ))}
          </ul>
          <button type="button" className="o-label cursor-pointer px-2" style={buttonStyle} onClick={t.dismissNotices}>
            dismiss
          </button>
        </div>
      )}

      <main className="grid min-h-0 flex-1 grid-cols-1 content-start items-start gap-3 overflow-auto p-3 lg:grid-cols-[18rem_1fr]">
        <div className="flex flex-col gap-3">
          <Panel title="targets" meta={`${t.targets.length}`}>
            <ul className="flex flex-col gap-1">
              {t.targets.map((x) => (
                <li key={x.name}>
                  <button
                    type="button"
                    onClick={() => t.select(x.name)}
                    aria-current={x.name === e?.name}
                    className="o-label w-full cursor-pointer px-2 py-1 text-left"
                    style={{
                      ...buttonStyle,
                      textTransform: "none",
                      color: x.name === e?.name ? "var(--o-ink)" : "var(--o-ink-2)",
                      background: x.name === e?.name ? "var(--o-surface-2)" : "transparent",
                    }}
                  >
                    <StatusDot status="idle" label={x.name} />
                  </button>
                </li>
              ))}
            </ul>
            <div className="mt-2 flex flex-wrap gap-2">
              <button type="button" className="o-label cursor-pointer px-2 py-1" style={buttonStyle} onClick={() => t.create()}>
                new
              </button>
              {e && (
                <>
                  <button type="button" className="o-label cursor-pointer px-2 py-1" style={buttonStyle} onClick={t.duplicate}>
                    duplicate
                  </button>
                  <button
                    type="button"
                    className="o-label cursor-pointer px-2 py-1"
                    style={{ ...buttonStyle, color: "var(--o-error)" }}
                    onClick={() => window.confirm(`Delete target ${e.name} and its keys from this browser?`) && t.remove(e.name)}
                  >
                    delete
                  </button>
                </>
              )}
            </div>
          </Panel>

          <Panel title="import">
            <div className="flex flex-col gap-2">
              <input
                ref={fileInput}
                type="file"
                accept=".yaml,.yml,application/yaml,text/yaml"
                aria-label="import a target file"
                className="o-label"
                onChange={async (ev) => {
                  const f = ev.target.files?.[0];
                  // The size before the read: a large file is never loaded.
                  if (f && f.size > MAX_FILE_BYTES) t.notify([`import: ${f.name} is ${TOO_LARGE}`]);
                  else if (f) t.importText(await f.text(), replace);
                  if (fileInput.current) fileInput.current.value = "";
                }}
              />
              <textarea
                value={paste}
                onChange={(ev) => setPaste(ev.target.value)}
                placeholder="or paste a target file"
                aria-label="paste a target file"
                rows={4}
                spellCheck={false}
                className="o-label px-2 py-1"
                style={{ ...inputStyle, textTransform: "none", fontFamily: "var(--o-font-mono)" }}
              />
              <label className="o-label flex items-center gap-2" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
                <input type="checkbox" checked={replace} onChange={(ev) => setReplace(ev.target.checked)} />
                replace a target of the same name (else import under a new name)
              </label>
              <button
                type="button"
                disabled={paste.trim() === ""}
                className="o-label cursor-pointer px-2 py-1"
                style={buttonStyle}
                onClick={() => {
                  t.importText(paste, replace);
                  setPaste("");
                }}
              >
                import pasted
              </button>
            </div>
          </Panel>
        </div>

        {e ? (
          <div className="flex flex-col gap-3">
            <Panel title="keys and export" meta={e.remembered ? "keys remembered in this browser" : "keys held in this tab"}>
              <div className="flex flex-col gap-2">
                {unset.length > 0 && (
                  <span className="o-label" style={{ color: "var(--o-amber)", textTransform: "none" }}>
                    not set in this tab: {unset.map((v) => `\${${v}}`).join(", ")}
                  </span>
                )}
                <label className="o-label flex items-center gap-2" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
                  <input
                    type="checkbox"
                    checked={e.remembered}
                    onChange={(ev) => t.remember(ev.target.checked)}
                  />
                  remember keys in this browser (stored unencrypted; any script on this origin can read them)
                </label>
                {e.remembered && (
                  <span className="o-label" style={{ color: "var(--o-amber)", textTransform: "none" }}>
                    ● this target's keys are stored in this browser
                  </span>
                )}
                <div className="flex flex-wrap items-center gap-2">
                  <button type="button" className="o-label cursor-pointer px-2 py-1" style={buttonStyle} onClick={t.forget}>
                    forget keys
                  </button>
                  <label className="o-label flex items-center gap-2" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
                    <input type="checkbox" checked={withKeys} onChange={(ev) => setWithKeys(ev.target.checked)} />
                    include keys in clear (else {"${ORBIT_KI}/${ORBIT_OPC}"}, as orbit target init writes)
                  </label>
                  <button
                    type="button"
                    className="o-label cursor-pointer px-2 py-1"
                    style={{ color: "var(--o-bg)", background: "var(--o-accent)", borderRadius: "var(--o-radius)" }}
                    onClick={() => {
                      const r = t.exportCurrent(withKeys);
                      if (r.file) download(r.file);
                      setWithKeys(r.withKeys);
                    }}
                  >
                    export {e.name}.yaml
                  </button>
                </div>
              </div>
            </Panel>
            <TargetEditor editing={e} edit={t.edit} setField={t.setField} setSecret={t.setSecret} />
          </div>
        ) : (
          <Panel title="no target">
            <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
              Make a new target, or import a file orbit target init wrote (~/.config/orbit/targets/&lt;name&gt;.yaml).
            </span>
          </Panel>
        )}
      </main>
    </div>
  );
}
