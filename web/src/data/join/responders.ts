/**
 * The registered responders and the pending join tokens, as the Targets
 * page shows them (ResponderService.ListResponders, JoinService.
 * ListJoinTokens): neither carries a secret, and the page shows only the
 * fields listed in SHOWN, so a field added to RegisteredResponder is not
 * rendered until it is reviewed here (the allow-list row in responders.test.ts).
 */
import { Code, ConnectError } from "@connectrpc/connect";
import type { JoinToken, RegisteredResponder } from "@/gen/orbit/v1/join_pb";

/** The RegisteredResponder fields the page shows. */
export const SHOWN = ["name", "state", "lastSeenUnixNano", "controlAddress", "dataIp", "dataPortLow", "dataPortHigh", "version"] as const;

/** The RegisteredResponder fields the page does not show; none is secret (reviewed). */
export const NOT_SHOWN = ["id", "source", "registeredUnixNano", "registeredFrom", "joinTokenId"] as const;

/** A responder as a row shows it: only SHOWN fields, read one by one. */
export interface ResponderRow {
  name: string;
  /** The server's state: online, offline, or never seen. */
  state: string;
  /** How long ago its last heartbeat was ("never" for none). */
  seen: string;
  control: string;
  data: string;
  version: string;
}

/** How long ago a time was, at now (ms), as a row says it: "never" for 0. */
export function ageText(unixNano: bigint, nowMs: number): string {
  if (unixNano === 0n) return "never";
  const s = Math.max(0, Math.floor((nowMs - Number(unixNano / 1_000_000n)) / 1000));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  return `${Math.floor(s / 3600)}h ago`;
}

/** The rows for ListResponders' answer, by name. */
export function responderRows(list: readonly RegisteredResponder[], nowMs: number): ResponderRow[] {
  return [...list]
    .sort((a, b) => a.name.localeCompare(b.name))
    .map((r) => ({
      name: r.name,
      state: r.state,
      seen: ageText(r.lastSeenUnixNano, nowMs),
      control: r.controlAddress,
      data: r.dataPortLow || r.dataPortHigh ? `${r.dataIp} udp ${r.dataPortLow}-${r.dataPortHigh}` : r.dataIp,
      version: r.version,
    }));
}

/** What the Remove confirm says, before anything is sent. */
export function removeConfirm(name: string): string {
  return `Remove responder ${name}? Its credential and loom token stop working, and runs that name it are refused; rerun the join to re-add it.`;
}

/** The part of a ResponderService client Remove uses. */
export interface RemoveClient {
  removeResponder(req: { name: string }): Promise<unknown>;
}

/**
 * Removes responder name after confirm (removeConfirm's text) says yes: a
 * declined confirm sends nothing (undefined). Otherwise "removed"; "already
 * removed" when the server no longer has it (NotFound: removed elsewhere,
 * or by a second click that got through); or why not, in the server's
 * words.
 */
export async function removeResponder(client: RemoveClient, name: string, confirm: (text: string) => boolean): Promise<string | undefined> {
  if (!confirm(removeConfirm(name))) return undefined;
  try {
    await client.removeResponder({ name });
    return "removed";
  } catch (err) {
    const e = ConnectError.from(err);
    if (e.code === Code.NotFound) return "already removed";
    return `not removed: ${e.rawMessage}`;
  }
}

/**
 * The keys with an action in flight (a Remove, by responder name): a second
 * start for a key while one runs is refused, so a double click asks once.
 */
export class InFlight {
  private readonly keys = new Set<string>();

  /** Starts key; false when it is already in flight. */
  start(key: string): boolean {
    if (this.keys.has(key)) return false;
    this.keys.add(key);
    return true;
  }

  end(key: string): void {
    this.keys.delete(key);
  }

  list(): string[] {
    return [...this.keys];
  }
}

/** A pending join token as a row shows it. */
export interface TokenRow {
  id: string;
  /** The responder's name, or how it will be named. */
  name: string;
  /** How long it has left at now ("expired" once it has none). */
  expiresIn: string;
}

/** The rows for ListJoinTokens' answer (pending tokens only), soonest to expire first. */
export function tokenRows(tokens: readonly JoinToken[], nowMs: number): TokenRow[] {
  return [...tokens]
    .sort((a, b) => Number(a.expiresUnixNano - b.expiresUnixNano))
    .map((t) => {
      const left = Math.floor((Number(t.expiresUnixNano / 1_000_000n) - nowMs) / 1000);
      return {
        id: t.id,
        name: t.name || "the app host's hostname",
        expiresIn: left <= 0 ? "expired" : left < 60 ? `${left}s` : `${Math.floor(left / 60)}m ${left % 60}s`,
      };
    });
}

/** The part of a JoinService client Revoke uses. */
export interface RevokeClient {
  revokeJoinToken(req: { id: string }): Promise<unknown>;
}

/**
 * Revokes pending token id: "revoked"; "used: the host joined" when a
 * registration used it first (FailedPrecondition: nothing to revoke); or
 * why not, in the server's words.
 */
export async function revokeToken(client: RevokeClient, id: string): Promise<string> {
  try {
    await client.revokeJoinToken({ id });
    return "revoked";
  } catch (err) {
    const e = ConnectError.from(err);
    if (e.code === Code.FailedPrecondition) return "used: the host joined";
    return `not revoked: ${e.rawMessage}`;
  }
}
