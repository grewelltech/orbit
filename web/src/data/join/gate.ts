/**
 * Whether the Targets page may ask for a join token. The server issues one
 * only over TLS, on that very connection, whatever the peer: loopback too
 * (JoinService.CreateJoinToken refuses plaintext), so https:// is required
 * everywhere, unlike keys (keysBlocked), which loopback may send in the
 * clear. From another host the page needs the API token in this tab, as
 * keysBlocked asks; loopback lookalikes are not loopback.
 */
import { keysBlocked } from "../targets/probe";

export function joinBlocked(loc: { protocol: string; hostname: string }, token: string): string | undefined {
  if (loc.protocol !== "https:") {
    return "a join token is a credential, issued only over TLS: serve with TLS (orbit serve --tls auto) and open the dashboard over https://";
  }
  if (keysBlocked(loc, token) !== undefined) {
    return "no API token in this tab: from another host, join tokens are issued only with the server's token (orbit serve --token-file)";
  }
  return undefined;
}
