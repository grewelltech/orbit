/**
 * Where the browser keeps targets and their keys.
 *
 * - Targets: localStorage `orbit.targets.v1`, each as its YAML text, so a
 *   reload keeps comments and key order. A secret field there is always a
 *   ${VAR} reference, never a key: the blob can be exported, shared or read
 *   by another tab without carrying one.
 * - Keys held for this tab: sessionStorage `orbit.targets.secrets`, by
 *   target and variable. Gone when the tab closes; another tab never sees
 *   them.
 * - Keys remembered in this browser, only when the user asks for it, per
 *   target: localStorage `orbit.targets.keys.<name>`. Stored unencrypted,
 *   readable by any script on this origin; Forget removes them.
 *
 * Nothing is sent to or kept by the server.
 *
 * The invariant covers the fields the schema names as keys (ki, opc,
 * token) and anything named so at any depth. A key under a misspelled
 * field (kii:) is outside it by design: the CLI refuses such a file as an
 * unknown key, and the page lists it as a problem.
 */
import { moveSecrets, parseTargets, refName, render, secretSlots, sharesNodes } from "./yamlio";

/** The Storage operations used; window.localStorage / sessionStorage in the browser, a fake in tests. */
export interface KeyValueStore {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

export const TARGETS_KEY = "orbit.targets.v1";
export const SECRETS_KEY = "orbit.targets.secrets";
export const KEYS_PREFIX = "orbit.targets.keys.";

/** One stored target: its name and its document's text. */
export interface StoredTarget {
  name: string;
  yaml: string;
}

interface Blob {
  version: 1;
  targets: StoredTarget[];
}

/** Variable → value, for one target. */
export type KeyValues = Record<string, string>;

function read(store: KeyValueStore | undefined, key: string): string | null {
  try {
    return store?.getItem(key) ?? null;
  } catch {
    return null;
  }
}

function write(store: KeyValueStore | undefined, key: string, value: string | null): void {
  try {
    if (value === null) store?.removeItem(key);
    else store?.setItem(key, value);
  } catch {
    /* private window, blocked or full storage: the page keeps working in memory */
  }
}

/**
 * yaml with every literal secret replaced by its ${VAR}, the values
 * dropped: what the targets blob may hold. Text that does not parse is
 * kept out (undefined).
 */
export function withoutSecrets(yaml: string): string | undefined {
  try {
    // Fail closed: exactly one target, standing alone (not a list), with
    // no alias or merge key (parseTargets refuses those), so every secret
    // field is one moveSecrets sees.
    const all = parseTargets(yaml);
    const t = all[0];
    if (all.length !== 1 || !t || t.text === undefined) return undefined;
    const clean = moveSecrets(t.doc).length === 0 ? yaml : render(t.doc, t.style);
    // And check what comes out, rather than trust the cleaning: one target,
    // nothing shared, every secret a ${VAR}.
    const again = parseTargets(clean);
    const [c] = again;
    if (again.length !== 1 || !c || sharesNodes(c.doc) || secretSlots(c.doc).some((s) => refName(s.value) === undefined)) return undefined;
    return clean;
  } catch {
    return undefined;
  }
}

/**
 * Parses the targets blob. Each target's text is cleaned of any literal
 * secret (withoutSecrets), so a blob written by anything else (another
 * tab, an older page, a hand edit) cannot bring a key into the page.
 */
export function parseBlob(raw: string | null): StoredTarget[] {
  if (!raw) return [];
  try {
    const b = JSON.parse(raw) as Partial<Blob>;
    if (b.version !== 1 || !Array.isArray(b.targets)) return [];
    const out: StoredTarget[] = [];
    for (const t of b.targets) {
      if (typeof t?.name !== "string" || typeof t.yaml !== "string") continue;
      const yaml = withoutSecrets(t.yaml);
      if (yaml !== undefined) out.push({ name: t.name, yaml });
    }
    return out;
  } catch {
    return [];
  }
}

/** The stored targets. */
export function loadTargets(local: KeyValueStore | undefined): StoredTarget[] {
  return parseBlob(read(local, TARGETS_KEY));
}

/** Stores the targets, each cleaned of literal secrets first. */
export function saveTargets(local: KeyValueStore | undefined, targets: readonly StoredTarget[]): void {
  const clean: StoredTarget[] = [];
  for (const t of targets) {
    const yaml = withoutSecrets(t.yaml);
    if (yaml !== undefined) clean.push({ name: t.name, yaml });
  }
  write(local, TARGETS_KEY, JSON.stringify({ version: 1, targets: clean } satisfies Blob));
}

function parseValues(raw: string | null): KeyValues {
  if (!raw) return {};
  try {
    const v = JSON.parse(raw) as unknown;
    if (!v || typeof v !== "object" || Array.isArray(v)) return {};
    const out: KeyValues = {};
    for (const [k, s] of Object.entries(v)) if (typeof s === "string") out[k] = s;
    return out;
  } catch {
    return {};
  }
}

/** The keys held in this tab for target name. */
export function tabKeys(session: KeyValueStore | undefined, name: string): KeyValues {
  const all = read(session, SECRETS_KEY);
  if (!all) return {};
  try {
    const v = (JSON.parse(all) as Record<string, unknown>)[name];
    return parseValues(v === undefined ? null : JSON.stringify(v));
  } catch {
    return {};
  }
}

/** Replaces the keys held in this tab for target name; an empty set removes the entry. */
export function setTabKeys(session: KeyValueStore | undefined, name: string, values: KeyValues): void {
  let all: Record<string, KeyValues> = {};
  try {
    all = (JSON.parse(read(session, SECRETS_KEY) ?? "{}") as Record<string, KeyValues>) ?? {};
  } catch {
    all = {};
  }
  if (Object.keys(values).length === 0) delete all[name];
  else all[name] = values;
  write(session, SECRETS_KEY, Object.keys(all).length === 0 ? null : JSON.stringify(all));
}

/** The keys remembered in this browser for target name (the opt-in). */
export function rememberedKeys(local: KeyValueStore | undefined, name: string): KeyValues {
  return parseValues(read(local, KEYS_PREFIX + name));
}

/** Whether target name has keys remembered in this browser. */
export function remembers(local: KeyValueStore | undefined, name: string): boolean {
  return read(local, KEYS_PREFIX + name) !== null;
}

/** Remembers target name's keys in this browser (the opt-in). */
export function rememberKeys(local: KeyValueStore | undefined, name: string, values: KeyValues): void {
  write(local, KEYS_PREFIX + name, JSON.stringify(values));
}

/** Forget: removes target name's keys from this browser and from this tab. */
export function forgetKeys(local: KeyValueStore | undefined, session: KeyValueStore | undefined, name: string): void {
  write(local, KEYS_PREFIX + name, null);
  setTabKeys(session, name, {});
}

/** A target's keys as the page uses them: this tab's over the remembered ones. */
export function keysFor(local: KeyValueStore | undefined, session: KeyValueStore | undefined, name: string): KeyValues {
  return { ...rememberedKeys(local, name), ...tabKeys(session, name) };
}

/** Moves a target's keys, in both places, to a new name. */
export function renameKeys(local: KeyValueStore | undefined, session: KeyValueStore | undefined, from: string, to: string): void {
  if (from === to) return;
  const tab = tabKeys(session, from);
  setTabKeys(session, from, {});
  if (Object.keys(tab).length > 0) setTabKeys(session, to, tab);
  if (remembers(local, from)) {
    rememberKeys(local, to, rememberedKeys(local, from));
    write(local, KEYS_PREFIX + from, null);
  }
}

/** What a storage event from another tab changed, if anything this page reads. */
export type StorageChange = { kind: "targets"; targets: StoredTarget[] } | { kind: "keys"; name: string } | undefined;

/**
 * Reads a `storage` event from another tab. Only the targets blob (cleaned
 * of secrets, as parseBlob does) and a remembered-keys entry count; the
 * page re-reads the latter from localStorage itself. Nothing else, and
 * nothing that would put a key in this tab's session, comes this way.
 */
export function storageChange(ev: { key: string | null; newValue: string | null; storageArea?: unknown }, local?: unknown): StorageChange {
  if (local !== undefined && ev.storageArea !== undefined && ev.storageArea !== local) return undefined;
  if (ev.key === TARGETS_KEY) return { kind: "targets", targets: parseBlob(ev.newValue) };
  if (ev.key?.startsWith(KEYS_PREFIX)) return { kind: "keys", name: ev.key.slice(KEYS_PREFIX.length) };
  return undefined;
}

/** window.localStorage, or undefined where storage is blocked. */
export function browserLocal(): KeyValueStore | undefined {
  try {
    return window.localStorage;
  } catch {
    return undefined;
  }
}

/** window.sessionStorage, or undefined where storage is blocked. */
export function browserSession(): KeyValueStore | undefined {
  try {
    return window.sessionStorage;
  } catch {
    return undefined;
  }
}
