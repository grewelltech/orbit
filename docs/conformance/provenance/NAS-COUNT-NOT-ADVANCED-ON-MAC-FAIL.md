# NAS-COUNT-NOT-ADVANCED-ON-MAC-FAIL

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). Versions read (issue #251, which records the read of every version from V15.0.0 to V20.1.0; the sentences below were read again on 2026-11-10): V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.5.1 (`24501-g51.zip`, issue #251 only), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`).

## TS 24.501 4.4.3.3 (Integrity protection and verification), shall, primary

> "After successful integrity protection validation, the receiver shall update its corresponding locally stored NAS COUNT with the value of the estimated NAS COUNT for this NAS message."

> "Integrity verification is not applicable when 5G-IA0 is used."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** both sentences are the same in every version read. Around them, Rel-16 splits the paragraph on the integrity algorithm's input into items a) and b) with a NOTE, and Rel-18 writes "clause" for "subclause".
- **Use in the check:** the stored COUNT is updated after a successful check, so a message that fails the check leaves it where it was. An AMF that stores the estimate first breaks this in every release, so every target release is graded.

## TS 24.501 4.4.3.2 (Replay protection), shall

> "Replay protection assures that one and the same NAS message is not accepted twice by the receiver. Specifically, for a given 5G NAS security context, a given NAS COUNT value shall be accepted at most one time and only if message integrity verifies correctly."

- **Since:** Rel-15, V15.0.0. Unchanged through V19.8.0.
- **Use in the check:** the FAIL row for an answer to the re-sent request. An AMF that checks the old sequence number at its original NAS COUNT finds a valid MAC; if it then answers, it accepted that COUNT twice.

## TS 24.501 4.4.3.1 (NAS COUNT: stored value and estimation), descriptive

> "The value of the uplink NAS COUNT stored in the AMF is the largest uplink NAS COUNT used in a successfully integrity checked NAS message."

> "The sequence number part of the estimated NAS COUNT value shall be equal to the sequence number in the received NAS message"

- **Since:** Rel-16, V16.5.1 (`24501-g51.zip`). V15.0.0 to V16.4.0 have neither sentence and give no estimation rules beyond "The receiving side shall estimate the NAS COUNT used by the sending side."
- **Changes:** the first sentence is the same from V16.15.0 through V19.8.0. Rel-18 writes "clause" for "subclause" in the surrounding text.
- **Use in the check:** supporting only. From V16.5.1, when the received sequence number is lower than the stored one, the receiver picks the overflow counter so that the estimate is higher than the stored COUNT, unless it can guarantee that the message was not previously accepted. So an old message sent again is checked at overflow + 1 and fails the MAC.
- **Before V16.5.1** an AMF may estimate the original COUNT, at which the old bytes verify. That AMF then either discards the message as a replay (PASS: the request after it is answered) or answers it (FAIL under 4.4.3.2).

## The check

After the security mode control procedure, on the same NAS signalling connection:

1. The UE sends request A, a UL NAS TRANSPORT carrying a PDU SESSION ESTABLISHMENT REQUEST with a fresh PDU session identity and procedure transaction identity. It must be answered.
2. It sends request B the same way. It must be answered. The downlink then settles for 1 s.
3. It sends A's exact bytes again. A is two uplinks back, so its sequence number is lower than the last accepted one (the equal case is NAS-REPLAY-DISCARD). The check watches 5 s for an answer naming A's identities, and for the AMF acting on the UE's state: a PDU SESSION RESOURCE RELEASE COMMAND or MODIFY REQUEST, a PDU SESSION RELEASE or MODIFICATION COMMAND, or a network-initiated DEREGISTRATION REQUEST. The downlink settled for 1 s after B and the UE sends nothing else in the window, so only the re-sent A can cause such an action; A names session A, which exists, so an AMF that processes it may release that session without answering. An action seen only after C was sent is recorded and not judged.
4. It sends request C, protected at its next uplink NAS COUNT.

## Verdict

- **Class:** conformance, two shalls (4.4.3.3, 4.4.3.2).
- **PASS** `count-kept` (4.4.3.3): C is answered (any 5GSM message naming its identities, the request returned by the AMF, or a PDU SESSION RESOURCE SETUP REQUEST for its session).
- **FAIL** `replay-accepted` (4.4.3.2): the re-sent A is answered. C is not judged.
- **FAIL** `acted-on` (4.4.3.2): the re-sent A is not answered, but within 5 s of it the AMF released or modified a PDU session of the UE or deregistered it. A's MAC verifies only at the NAS COUNT the AMF accepted it at, so acting on it accepts that COUNT a second time, as an answer would. C is not judged.
- **FAIL** `count-advanced` (4.4.3.3): C is not answered within 10 s while the association is up. The absence is inferred within that bound, the one A and B were answered within.
- **WARN (security)** `released`: the AMF released the UE, or the association went, after A was sent again and before C was answered. No requirement is violated; the release acts on a message whose MAC failed. As for NAS-INTEGRITY-FAIL-DISCARD, a UE release leaves the registration and the sessions in place and may have local causes, so it stays WARN, unlike `acted-on`.
- **BLOCKED** (PDU session establishment): A or B was not answered within 10 s, or the UE was released before A was sent again. **BLOCKED** (registration): the UE did not register.
- **CRASH:** the AMF no longer completes an NG Setup after the stimulus.
- **ERROR:** the harness could not build, send or read; the NAS probe could not be opened; a local socket failed.
- **SKIP:** no UE; the core selected 5G-IA0 (no NAS integrity protection; the UE harness refuses it); a target release older than Rel-15.

## Cleanup

A failure leaves the UE's security context unusable: the AMF checks every later uplink at the wrong COUNT. The cleanup sends a PDU SESSION RELEASE REQUEST for each session the core set up (best effort, unanswered), then a switch-off DEREGISTRATION REQUEST, waits up to 3 s for the UE CONTEXT RELEASE COMMAND, and drops the association whatever came back. On the FAIL path the AMF cannot verify either message, so it may keep a half-registered UE, and its sessions, until its own timers expire. The detail says so.
