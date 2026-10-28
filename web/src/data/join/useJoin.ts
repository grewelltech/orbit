/**
 * The join panel's token (JoinSession), wired to the page: another target
 * closes it, asking before revoking a copied token (close with a confirm);
 * the panel going closes it without asking (a copied token is kept to its
 * TTL); the page going revokes one never copied (joinPageLifecycle, a
 * keepalive revoke), and a back/forward-cache restore checks it again; a
 * 1s tick drives the countdown and drops an expired line; the responders
 * the page polls mark the token used; copy is copyLine.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { JoinService, type RegisteredResponder } from "@/gen/orbit/v1/join_pb";
import { loadToken, orbitTransport } from "../transport";
import { copyLine, JOIN_IDLE, joinPageLifecycle, JoinSession, revokeOnHide, type JoinState } from "./session";
import type { JoinSpecInit } from "./spec";

export function useJoin(targetName: string | undefined, responders: readonly RegisteredResponder[], baseUrl?: string) {
  const client = useMemo(() => createClient(JoinService, orbitTransport(baseUrl)), [baseUrl]);
  const [state, setState] = useState<JoinState>(JOIN_IDLE);
  const session = useMemo(() => new JoinSession(client, setState), [client]);
  // Another target: close, asking before revoking a copied token.
  const shownFor = useRef(targetName);
  useEffect(() => {
    if (shownFor.current === targetName) return;
    shownFor.current = targetName;
    void session.close((question) => window.confirm(question));
  }, [session, targetName]);
  // The panel gone: close without asking (a copied token is kept to its TTL).
  useEffect(() => () => void session.close(), [session]);
  // The page going (a link, a reload, the tab closed): the same, best effort.
  useEffect(
    () => joinPageLifecycle(window, session, (id) => revokeOnHide(fetch, baseUrl ?? window.location.origin, loadToken(), id)),
    [session, baseUrl],
  );
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => {
      session.tick();
      setNow(Date.now());
    }, 1000);
    return () => clearInterval(t);
  }, [session]);
  useEffect(() => session.sawResponders(responders), [session, responders]);
  const make = useCallback((name: string, ttlSeconds: number, spec: JoinSpecInit) => session.create(name, ttlSeconds, spec), [session]);
  const revoke = useCallback(() => session.revoke(), [session]);
  const close = useCallback(() => session.close((question) => window.confirm(question)), [session]);
  const copy = useCallback(() => copyLine(session, navigator.clipboard), [session]);
  const reveal = useCallback(() => session.markRevealed(), [session]);
  const copiedByHand = useCallback(() => session.markCopied(), [session]);
  return { state, now, make, revoke, close, copy, reveal, copiedByHand };
}
