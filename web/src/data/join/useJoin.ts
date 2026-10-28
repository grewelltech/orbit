/**
 * The join panel's token (JoinSession), wired to the page: another target
 * or leaving the panel revokes an unused token (close), and so does the
 * page going (joinPageLifecycle, a keepalive revoke); a 1s tick drives the
 * countdown and drops an expired line; the responders the page polls mark
 * the token used.
 */
import { useCallback, useEffect, useMemo, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { JoinService, type RegisteredResponder } from "@/gen/orbit/v1/join_pb";
import { loadToken, orbitTransport } from "../transport";
import { JOIN_IDLE, joinPageLifecycle, JoinSession, revokeOnHide, type JoinState } from "./session";
import type { JoinSpecInit } from "./spec";

export function useJoin(targetName: string | undefined, responders: readonly RegisteredResponder[], baseUrl?: string) {
  const client = useMemo(() => createClient(JoinService, orbitTransport(baseUrl)), [baseUrl]);
  const [state, setState] = useState<JoinState>(JOIN_IDLE);
  const session = useMemo(() => new JoinSession(client, setState), [client]);
  // Another target, or the panel gone: an unused token is revoked.
  useEffect(() => () => void session.close(), [session, targetName]);
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
  const close = useCallback(() => session.close(), [session]);
  return { state, now, make, revoke, close };
}
