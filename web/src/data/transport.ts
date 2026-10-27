/**
 * The dashboard's Connect transport: every API call carries the server's
 * bearer token when the operator has given one, and a call the server refuses
 * as unauthenticated asks for it.
 *
 * The dashboard's static files are served without a token (they hold no
 * secrets); its API calls need one when `orbit serve` runs with --token-file.
 * The token lives in sessionStorage — per tab, gone when the tab closes — so
 * it is never written to disk by the browser.
 */
import { Code, ConnectError, type Interceptor, type Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { Emitter } from "./source";

const TOKEN_KEY = "orbit.token";

/** Where the token is kept; sessionStorage in the browser, a fake in tests. */
export interface TokenStore {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

function browserStore(): TokenStore | undefined {
  try {
    return window.sessionStorage;
  } catch {
    return undefined;
  }
}

/** The stored token, or "" when none or when storage is unavailable. */
export function loadToken(store: TokenStore | undefined = browserStore()): string {
  try {
    return store?.getItem(TOKEN_KEY)?.trim() ?? "";
  } catch {
    return "";
  }
}

/** Stores the token (an empty one forgets it); a no-op when storage is unavailable. */
export function saveToken(token: string, store: TokenStore | undefined = browserStore()): void {
  try {
    const t = token.trim();
    if (t) store?.setItem(TOKEN_KEY, t);
    else store?.removeItem(TOKEN_KEY);
  } catch {
    /* private window, blocked storage: the prompt asks again next time */
  }
}

/**
 * Why the server refused a call: no token was sent, the one sent is wrong,
 * or this address sent too many wrong ones and must wait (the right token
 * still passes).
 */
export type AuthRefusal = "missing" | "invalid" | "locked";

/** Fires when the server refuses a call for its token. */
export const authRequired = new Emitter<AuthRefusal>();

function refusal(err: unknown, sent: boolean): AuthRefusal | null {
  const code = ConnectError.from(err).code;
  if (code === Code.ResourceExhausted) return "locked"; // only the auth layer answers this
  if (code === Code.Unauthenticated) return sent ? "invalid" : "missing";
  return null;
}

/**
 * The interceptor: sets Authorization from the token getter, and reports a
 * refusal for the token to onRefused — whether it arrives as the response
 * (unary, or a stream refused before it starts) or from inside a stream.
 */
export function authInterceptor(token: () => string, onRefused: (why: AuthRefusal) => void): Interceptor {
  return (next) => async (req) => {
    const t = token();
    if (t) req.header.set("Authorization", `Bearer ${t}`);
    const report = (err: unknown) => {
      const why = refusal(err, t !== "");
      if (why) onRefused(why);
    };
    try {
      const res = await next(req);
      if (!res.stream) return res;
      const messages = res.message;
      return {
        ...res,
        message: (async function* () {
          try {
            yield* messages;
          } catch (err) {
            report(err);
            throw err;
          }
        })(),
      };
    } catch (err) {
      report(err);
      throw err;
    }
  };
}

/** The transport every dashboard client uses; same-origin by default (the embedded UI). */
export function orbitTransport(baseUrl?: string): Transport {
  return createConnectTransport({
    baseUrl: baseUrl ?? window.location.origin,
    interceptors: [authInterceptor(() => loadToken(), (why) => authRequired.emit(why))],
  });
}
