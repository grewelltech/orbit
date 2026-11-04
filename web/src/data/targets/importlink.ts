/**
 * Preloading targets from a `#import=` URL fragment.
 *
 * A tool that stands up a core — the LXD testbed is the one that does — can
 * hand its ready-made targets to the Targets page in a single link:
 * `?targets#import=<base64url of a target file>`. The payload rides in the
 * URL fragment, which a browser never puts in an HTTP request, so a key
 * carried in it never reaches the dashboard's access log (ADR-0008 keeps the
 * server out of the key path). The link itself still carries the key to
 * anyone it is shared with, so the page never imports silently: it decodes
 * the payload, names what the link holds and whether it carries keys, and
 * imports only when the viewer says so.
 */
import { MAX_FILE_BYTES, nameOf, parseTargets, refName, secretSlots, TOO_LARGE, utf8Length } from "./yamlio";

/** The fragment key this module reads: `#import=<base64url>`. */
export const IMPORT_PREFIX = "import=";

/** A decoded `#import=` payload, ready to show in the confirm banner. */
export interface PendingImport {
  /** The target file the link carries, as YAML text. */
  text: string;
  /** The names of the targets in it, for the banner. */
  names: string[];
  /** True when any target carries a Ki, OPc or token in clear (not a ${VAR}). */
  hasKeys: boolean;
}

/** base64url (RFC 4648 §5, no padding) back to UTF-8 text; throws on bad input. */
function decodeBase64Url(s: string): string {
  const b64 = s.replace(/-/g, "+").replace(/_/g, "/");
  const pad = (4 - (b64.length % 4)) % 4;
  const bin = atob(b64 + "=".repeat(pad));
  const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
  return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
}

/** True when the document holds a secret field with a literal value, not a ${VAR}. */
function documentHasKeys(doc: Parameters<typeof secretSlots>[0]): boolean {
  return secretSlots(doc).some((s) => refName(s.value) === undefined && s.value !== "");
}

/**
 * Reads a page's `location.hash`. A `#import=` fragment is decoded and
 * described; anything else (no fragment, another fragment) is undefined, and
 * a fragment that does not decode or parse is a refusal to show as a notice.
 * The caller clears the hash either way, so a reload does not re-prompt.
 */
export function readPendingImport(hash: string): PendingImport | { refusal: string } | undefined {
  const frag = hash.startsWith("#") ? hash.slice(1) : hash;
  if (!frag.startsWith(IMPORT_PREFIX)) return undefined;
  const encoded = frag.slice(IMPORT_PREFIX.length);
  if (encoded === "") return undefined;

  let text: string;
  try {
    text = decodeBase64Url(encoded);
  } catch {
    return { refusal: "import link: the payload is not valid base64url" };
  }
  if (utf8Length(text) > MAX_FILE_BYTES) return { refusal: `import link: the payload is ${TOO_LARGE}` };

  let parsed;
  try {
    parsed = parseTargets(text);
  } catch (e) {
    const msg = (e instanceof Error ? e.message : String(e)).split("\n")[0] ?? "";
    return { refusal: `import link: ${msg}` };
  }
  if (parsed.length === 0) return { refusal: "import link: the payload holds no target" };

  return {
    text,
    names: parsed.map((p) => nameOf(p.doc)),
    hasKeys: parsed.some((p) => documentHasKeys(p.doc)),
  };
}
