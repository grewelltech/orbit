/**
 * The join one-liner as the page shows it by default: the token in its URL
 * masked, so a screen share or a look over the shoulder does not take it.
 * The pin (the server's key) is not secret and shows.
 */
export const MASK = "••••••••";

/** line with every occurrence of secret masked; line as it is when secret is "". */
export function maskOneLiner(line: string, secret: string): string {
  return secret === "" ? line : line.split(secret).join(MASK);
}
