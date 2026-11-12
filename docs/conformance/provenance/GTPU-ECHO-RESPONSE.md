# GTPU-ECHO-RESPONSE

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/29_series/29.281/](https://www.3gpp.org/ftp/Specs/archive/29_series/29.281/). Versions read: V8.0.0 (`29281-800.zip`), V9.0.0, V11.0.0, V14.1.0, V15.0.0, V15.7.0 (`29281-f70.zip`), V16.2.0 (`29281-g20.zip`), V17.5.0 (`29281-h50.zip`), V18.4.0 (`29281-i40.zip`), V19.2.0 (`29281-j20.zip`). No Rel-10 version was read.

## TS 29.281 7.2.1 (Echo Request), shall, primary

> "Even if there is no path in use, a GTP-U peer shall be prepared to receive an Echo Request at any time and it shall reply with an Echo Response."

- **Since:** Rel-8, V8.0.0 (`29281-800.zip`), worded "A GTP-U peer shall be prepared to receive an Echo Request at any time and it shall reply with an Echo Response."
- **Changes:** Rel-8 through Rel-14 (V8.0.0 and V14.1.0 read) have no "Even if there is no path in use," prefix. It is present from Rel-15 V15.0.0. The wording is unchanged Rel-15 through Rel-19.

## TS 29.281 7.2.2 (Echo Response), shall

> "The message shall be sent as a response to a received Echo Request."

- **Since:** Rel-8, V8.0.0 (`29281-800.zip`).
- **Changes:** none; the wording is unchanged Rel-8 through Rel-19.

## TS 29.281 4.3.1 (Handling of Sequence Numbers), shall

> "The Sequence Number in a signalling response message shall be copied from the signalling request message that the GTP-U entity is replying to."

- **Since:** Rel-8, V8.0.0 (`29281-800.zip`), worded "... that the GSN or RNC is replying to."
- **Changes:** Rel-8 and Rel-9 (V8.0.0, V9.0.0) say "GSN or RNC". By Rel-11 V11.0.0 it is "GTP-U entity"; no Rel-10 version was read, so the change is bounded only between V9.0.0 and V11.0.0. The wording is unchanged Rel-11 V11.0.0 through Rel-19.
- The "same sequence number" rule is in this clause, not in 7.2.1 or 7.2.2.

## Verdict

- **Class:** conformance, from the three shalls. The check owns the procedure "GTP-U Echo": a check that needs the UPF to answer a GTP-U Echo Request and gets no answer reports BLOCKED and names this check.
- **Requires:** Rel-15. The three requirements date from Rel-8, but the check addresses a UPF's N3 interface, which exists from Rel-15.
- **Stimulus:** up to three Echo Requests to `--upf-n3`, with sequence numbers 0x5A01, 0x5A02 and 0x5A03, each waited 2 s; the next is sent only when the one before went unanswered, so one lost datagram is not a missing response. No session or tunnel is needed (7.2.1: "Even if there is no path in use").
- **PASS** `echo-response` (7.2.1, 4.3.1): an Echo Response carrying a sequence number sent so far. A late answer to an earlier request counts: it copies that request's number.
- **FAIL** `sequence-not-copied` (4.3.1): `Echo Response with a sequence number that was not sent`.
- **FAIL** `no-echo-response` (7.2.1): `no Echo Response` to any of the three requests, the absence inferred within the 6 s bound of the check (three requests, 2 s each); TS 29.281 sets no timer for the response.
- Both FAILs end their detail with: "no GTP-U Echo Response / sequence number not copied. TS 29.281 7.2.1, 7.2.2, 4.3.1 (Rel-8 onward). Likely cause: UPF echo handling."
- **Not completed:** both FAILs report the GTP-U Echo as not completed: a response whose sequence number was not sent answers no request the sender can match.
- **ERROR:** `--upf-n3` or `--n3-bind` does not resolve, the N3 socket cannot be bound, a request cannot be sent, or the per-test timeout expired before the three waits ended.
- **SKIP:** no N3 params (`--upf-n3` / `--n3-bind`), or a target release older than Rel-15.
- **Limits:** silence cannot tell a UPF without Echo handling from one that `--upf-n3` does not reach, and with no session there is nothing else to probe the UPF with. `--upf-n3` must be the UPF's primary N3 address. An expected-failures entry for `no Echo Response` therefore also tolerates an unreachable UPF in this check.
- **Refinements the check does not apply:** clause 5.1 says "For the Echo Request, Echo Response, Error Indication and Supported Extension Headers Notification messages, the S flag shall be set to '1'." (present V15.0.0 onward; Rel-8 not confirmed). A response without the S flag carries no sequence number and is not counted as an Echo Response, so it ends as `no Echo Response`. Clause 7.2.2 makes the Recovery IE mandatory, with a Restart Counter that "shall be set to zero by the sender and shall be ignored by the receiver"; the check does not read it.
