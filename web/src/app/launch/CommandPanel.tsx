/**
 * The run as the CLI runs it, from the server (RenderRun): the command to
 * copy and the run file to save, keyless both. The run file can be edited
 * here as YAML, or imported whole; either goes through the run-file gate.
 */
import { useEffect, useRef, useState } from "react";
import { Panel } from "@/components/Panel";
import type { LaunchState } from "@/data/launch/launch";
import { readImportFile } from "@/data/targets/importexport";
import { buttonStyle, inputStyle } from "../targets/fields";

/** Hands the browser a file to save. */
function download(name: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: "application/yaml" }));
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

export function CommandPanel({
  state,
  runYaml,
  targetName,
  replaceText,
  onNotice,
}: {
  state: LaunchState;
  runYaml: string;
  targetName: string;
  /** Replaces the run file; keepTemplate for an edit of this file, not another one imported. */
  replaceText: (text: string, keepTemplate: boolean) => boolean;
  onNotice: (n: string) => void;
}) {
  const [text, setText] = useState(runYaml);
  const [copied, setCopied] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);
  useEffect(() => setText(runYaml), [runYaml]);
  const r = state.rendered;
  const fileName = r?.fileName || `${targetName}.run.yaml`;

  return (
    <Panel title="as the CLI runs it" meta={r?.fileName ? "orbit run, with the file below" : r ? "orbit test" : undefined}>
      <div className="flex flex-col gap-2">
        {state.renderError ? (
          <span className="o-label" role="alert" style={{ color: "var(--o-error)", textTransform: "none" }}>
            {state.renderError.message}
          </span>
        ) : (
          <pre className="o-num overflow-x-auto whitespace-pre-wrap break-all p-2" style={{ ...inputStyle, minHeight: "2.5rem" }} aria-label="command">
            {r?.command ?? "…"}
          </pre>
        )}
        <div className="flex flex-wrap items-center gap-2">
          <button
            type="button"
            disabled={!r}
            className="o-label cursor-pointer px-2 py-1 disabled:opacity-50"
            style={buttonStyle}
            onClick={() => {
              if (!r) return;
              void navigator.clipboard?.writeText(r.command).then(() => {
                setCopied(true);
                setTimeout(() => setCopied(false), 1500);
              });
            }}
          >
            {copied ? "copied" : "copy as CLI"}
          </button>
          <button type="button" disabled={!r} className="o-label cursor-pointer px-2 py-1 disabled:opacity-50" style={buttonStyle} onClick={() => r && download(fileName, r.runYaml)}>
            export YAML ({fileName})
          </button>
          <button type="button" className="o-label cursor-pointer px-2 py-1" style={buttonStyle} onClick={() => fileInput.current?.click()}>
            import YAML
          </button>
          <input
            ref={fileInput}
            type="file"
            accept=".yaml,.yml"
            className="hidden"
            onChange={async (e) => {
              const f = e.target.files?.[0];
              e.target.value = "";
              if (!f) return;
              const got = await readImportFile(f);
              if ("refusal" in got) return onNotice(got.refusal.replace("a target file", "a run file"));
              replaceText(got.text, false);
            }}
          />
          <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
            the same plan as here with no $ORBIT_* exported (--explain shows what those change)
          </span>
        </div>
        <label className="flex flex-col gap-1">
          <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
            the run file (edit here for what the form has no control for; kept when you leave the box)
          </span>
          <textarea
            aria-label="run file"
            className="o-num min-h-48 p-2"
            style={inputStyle}
            spellCheck={false}
            value={text}
            onChange={(e) => setText(e.target.value)}
            onBlur={() => {
              if (text !== runYaml) replaceText(text, true);
            }}
          />
        </label>
      </div>
    </Panel>
  );
}
