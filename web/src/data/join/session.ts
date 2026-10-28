/**
 * A join token on the Targets page, from creation to its end (JoinSession):
 * the one-liner, which holds a live token, is kept in this object only,
 * never in storage, and dropped when the token expires, is used, or is
 * revoked. While the line has never left the page, a token no registration
 * used is revoked whenever the page lets it go (close, another target,
 * leaving the page). Once it is copied it is on its way to an app host:
 * close and another target ask before revoking it (keeping it by default),
 * and unmounting or leaving the page does not revoke it; its TTL ends it,
 * as with the CLI's add-responder. Revoke always revokes.
 */
import { Code, ConnectError } from "@connectrpc/connect";
import type { JoinToken } from "@/gen/orbit/v1/join_pb";
import type { JoinSpecInit } from "./spec";

/** The part of a JoinService client a session uses. */
export interface JoinClient {
  createJoinToken(req: { name: string; ttlSeconds: number; spec: JoinSpecInit }): Promise<{ issued?: JoinToken; secret: string; oneLiner: string }>;
  listJoinTokens(req: object): Promise<{ tokens: JoinToken[] }>;
  revokeJoinToken(req: { id: string }): Promise<unknown>;
}

export type JoinPhase = "idle" | "creating" | "ready" | "used" | "expired" | "revoked" | "failed";

/** A session as the panel shows it. line and secret are set only while ready. */
export interface JoinState {
  phase: JoinPhase;
  issued?: JoinToken;
  /** The one-liner: a live token. Only while ready. */
  line?: string;
  /** The token in it, for the mask. Only while ready. */
  secret?: string;
  /** When the token expires (ms). */
  expiresAt?: number;
  /** The responder that used it. */
  usedBy?: string;
  /** The line has been copied: it may be on its way to an app host. */
  copied?: boolean;
  /** What went wrong, or what became of the token, in words. */
  note?: string;
}

export const JOIN_IDLE: JoinState = { phase: "idle" };

/** The state with its secret parts (the line, the token) dropped. */
function spent(s: JoinState, phase: JoinPhase, note?: string): JoinState {
  return { phase, issued: s.issued, expiresAt: s.expiresAt, usedBy: s.usedBy, note };
}

export class JoinSession {
  private state: JoinState = JOIN_IDLE;

  constructor(
    private readonly client: JoinClient,
    private readonly set: (s: JoinState) => void,
    private readonly now: () => number = Date.now,
  ) {}

  private update(s: JoinState): void {
    this.state = s;
    this.set(s);
  }

  /** Asks for a token. A token the session already holds unused is revoked first. */
  async create(name: string, ttlSeconds: number, spec: JoinSpecInit): Promise<void> {
    await this.close();
    this.update({ phase: "creating" });
    try {
      const res = await this.client.createJoinToken({ name, ttlSeconds, spec });
      const expiresAt = Number((res.issued?.expiresUnixNano ?? 0n) / 1_000_000n);
      this.update({ phase: "ready", issued: res.issued, line: res.oneLiner, secret: res.secret, expiresAt });
    } catch (err) {
      this.update({ phase: "failed", note: ConnectError.from(err).rawMessage });
    }
  }

  /** The state at now: an expired token's line is dropped. */
  tick(): JoinState {
    const s = this.state;
    if (s.phase === "ready" && s.expiresAt !== undefined && this.now() >= s.expiresAt) this.update(spent(s, "expired", "expired: make another"));
    return this.state;
  }

  /** Marks the token used when a registered responder names it (join_token_id); its line is dropped, being dead. */
  sawResponders(rs: readonly { name: string; joinTokenId: string }[]): void {
    const s = this.state;
    const id = s.issued?.id;
    if (!id || (s.phase !== "ready" && s.phase !== "expired")) return;
    const r = rs.find((x) => x.joinTokenId === id);
    if (r) this.update({ ...spent(s, "used"), usedBy: r.name });
  }

  /** The live one-liner, undefined when there is none. */
  line(): string | undefined {
    return this.state.phase === "ready" ? this.state.line : undefined;
  }

  /** Records that the line left the page (copyLine). */
  markCopied(): void {
    if (this.state.phase === "ready") this.update({ ...this.state, copied: true });
  }

  /** What close asks before revoking a copied token: until when it works. */
  keepQuestion(): string {
    const at = this.state.expiresAt ?? this.now();
    const left = Math.max(0, Math.ceil((at - this.now()) / 60_000));
    return `The copied one-liner works until ${new Date(at).toLocaleTimeString()} (${left} min left). Revoke it now?`;
  }

  /**
   * After a back/forward-cache restore (pageshow, persisted): the token may
   * have been revoked or used while the page was away, or have expired; a
   * ready token no longer pending is dropped and the panel reset.
   */
  async recheck(): Promise<void> {
    const s = this.tick();
    const id = s.issued?.id;
    if (!id || s.phase !== "ready") return;
    try {
      const { tokens } = await this.client.listJoinTokens({});
      if (!tokens.some((t) => t.id === id)) this.update({ phase: "idle", note: "the join token is no longer live (used, revoked or expired while the page was away)" });
    } catch {
      /* unreachable now: the next tick or poll tells */
    }
  }

  /** Revokes the token now; FailedPrecondition is a registration that came first. */
  async revoke(): Promise<void> {
    const s = this.state;
    const id = s.issued?.id;
    if (!id || s.phase !== "ready") return;
    try {
      await this.client.revokeJoinToken({ id });
      this.update(spent(s, "revoked", "revoked"));
    } catch (err) {
      const e = ConnectError.from(err);
      if (e.code === Code.FailedPrecondition) this.update(spent(s, "used", "used: the host joined"));
      else this.update({ ...s, note: `not revoked: ${e.rawMessage}` });
    }
  }

  /**
   * Lets the token go. One never copied is revoked when no registration
   * used it (still pending in ListJoinTokens), as the CLI's revokeUnused
   * does. A copied one is revoked only when ask (keepQuestion) says yes;
   * with no ask (the panel unmounting) it is kept to its TTL. Then the line
   * is dropped. A failure to revoke is told, not thrown: the TTL ends it.
   */
  async close(ask?: (question: string) => boolean): Promise<void> {
    const s = this.state;
    const id = s.issued?.id;
    if (!id || s.phase !== "ready") {
      if (s.phase !== "idle") this.update(JOIN_IDLE);
      return;
    }
    if (s.copied && !(ask?.(this.keepQuestion()) ?? false)) {
      const at = s.expiresAt === undefined ? "its expiry" : new Date(s.expiresAt).toLocaleTimeString();
      this.update({ phase: "idle", note: `kept: the copied one-liner works until it is used or expires at ${at}` });
      return;
    }
    let note: string | undefined;
    try {
      const { tokens } = await this.client.listJoinTokens({});
      if (tokens.some((t) => t.id === id)) await this.client.revokeJoinToken({ id });
    } catch (err) {
      const e = ConnectError.from(err);
      if (e.code !== Code.FailedPrecondition) note = `the unused join token ${id} could not be revoked (${e.rawMessage}); it expires on its own`;
    }
    this.update(note ? { phase: "idle", note } : JOIN_IDLE);
  }

  /** The token to revoke when the page goes (pagehide): its ID while it is live, unused and never copied, else undefined. */
  liveID(): string | undefined {
    return this.state.phase === "ready" && !this.state.copied ? this.state.issued?.id : undefined;
  }
}

/**
 * Revokes join token id as the page goes (pagehide), where a Connect call
 * may not finish: a keepalive fetch, with the API token. Best effort: the
 * TTL ends the token if this is lost.
 */
export function revokeOnHide(fetcher: typeof fetch, baseUrl: string, token: string, id: string): void {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (token) headers.Authorization = `Bearer ${token}`;
  void fetcher(`${baseUrl}/orbit.v1.JoinService/RevokeJoinToken`, { method: "POST", keepalive: true, headers, body: JSON.stringify({ id }) }).catch(() => undefined);
}

/**
 * The join panel's page lifecycle, as its hook wires it: when the page goes
 * (pagehide), a live token never copied is revoked (revokeOnHide); when the
 * back/forward cache restores the page (pageshow, persisted), the session
 * is checked again (recheck). Returns the remover.
 */
export function joinPageLifecycle(
  win: Pick<EventTarget, "addEventListener" | "removeEventListener">,
  session: Pick<JoinSession, "liveID" | "recheck">,
  revoke: (id: string) => void,
): () => void {
  const hide = () => {
    const id = session.liveID();
    if (id) revoke(id);
  };
  const show = (e: Event) => {
    if ((e as Partial<PageTransitionEvent>).persisted === true) void session.recheck();
  };
  win.addEventListener("pagehide", hide);
  win.addEventListener("pageshow", show);
  return () => {
    win.removeEventListener("pagehide", hide);
    win.removeEventListener("pageshow", show);
  };
}

/** The part of the Clipboard API copyLine uses. */
export interface LineClipboard {
  writeText(text: string): Promise<void>;
}

/**
 * Copies the session's one-liner, the real one whether or not it is shown
 * revealed, and records that it left the page (markCopied). False, with
 * nothing written, when there is no live line or the write fails.
 */
export async function copyLine(session: Pick<JoinSession, "line" | "markCopied">, clipboard: LineClipboard): Promise<boolean> {
  const line = session.line();
  if (line === undefined) return false;
  try {
    await clipboard.writeText(line);
  } catch {
    return false;
  }
  session.markCopied();
  return true;
}
