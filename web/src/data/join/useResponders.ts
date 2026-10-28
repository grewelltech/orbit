/**
 * The Targets page's responders: ListResponders and ListJoinTokens, polled
 * together with backoff (pollWithBackoff), through the dashboard's
 * transport (its token, and the token prompt on a refusal); and Remove and
 * Revoke, after which they are read again at once.
 */
import { useCallback, useEffect, useMemo, useState } from "react";
import { ConnectError, createClient } from "@connectrpc/connect";
import { JoinService, ResponderService, type JoinToken, type RegisteredResponder } from "@/gen/orbit/v1/join_pb";
import { orbitTransport } from "../transport";
import { pollWithBackoff } from "./poll";
import { InFlight, removeResponder, revokeToken } from "./responders";

export interface RespondersState {
  responders: RegisteredResponder[];
  tokens: JoinToken[];
  /** When the lists were last read (ms), undefined before the first answer. */
  at?: number;
  /** Why the last read failed, until one succeeds. */
  error?: string;
}

export function useResponders(baseUrl?: string) {
  const transport = useMemo(() => orbitTransport(baseUrl), [baseUrl]);
  const responders = useMemo(() => createClient(ResponderService, transport), [transport]);
  const joins = useMemo(() => createClient(JoinService, transport), [transport]);
  const [state, setState] = useState<RespondersState>({ responders: [], tokens: [] });
  const [reads, setReads] = useState(0);
  useEffect(() => {
    const ctrl = new AbortController();
    void pollWithBackoff(
      async () => Promise.all([responders.listResponders({}), joins.listJoinTokens({})]),
      ([r, t]) => setState({ responders: r.responders, tokens: t.tokens, at: Date.now() }),
      (err) => setState((s) => ({ ...s, error: ConnectError.from(err).rawMessage })),
      { intervalMs: 5000, maxBackoffMs: 60_000, signal: ctrl.signal },
    );
    return () => ctrl.abort();
  }, [responders, joins, reads]);
  // A Remove in flight: its button is busy, and a second click asks nothing.
  const inflight = useMemo(() => new InFlight(), []);
  const [removing, setRemoving] = useState<string[]>([]);
  const remove = useCallback(
    async (name: string) => {
      if (!inflight.start(name)) return undefined;
      setRemoving(inflight.list());
      try {
        const said = await removeResponder(responders, name, (text) => window.confirm(text));
        if (said !== undefined) setReads((n) => n + 1);
        return said;
      } finally {
        inflight.end(name);
        setRemoving(inflight.list());
      }
    },
    [responders, inflight],
  );
  const revoke = useCallback(
    async (id: string) => {
      const said = await revokeToken(joins, id);
      setReads((n) => n + 1);
      return said;
    },
    [joins],
  );
  return { state, removing, remove, revoke };
}
