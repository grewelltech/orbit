# NAS-INTEGRITY-FAIL-DISCARD

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/) and [https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/](https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/). Versions read (2026-11-10; issue #142, step 0):

- TS 24.501: V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`).
- TS 33.501: V15.0.0 (`33501-f00.zip`), V15.20.0 (`33501-fk0.zip`), V16.20.0 (`33501-gk0.zip`), V17.16.0 (`33501-hg0.zip`), V18.12.0 (`33501-ic0.zip`), V19.7.0 (`33501-j70.zip`).

The issue named TS 24.501 4.4.4.3 as a candidate. The read confirmed it and added TS 33.501 6.4.3.3, which states the same requirement from the security side.

## TS 24.501 4.4.4.3 (Integrity checking of NAS signalling messages in the AMF), shall, primary

> "Once the secure exchange of NAS messages has been established for the NAS signalling connection, the receiving 5GMM entity in the AMF shall not process any NAS signalling messages unless they have been successfully integrity checked by the NAS. If any NAS signalling message, having not successfully passed the integrity check, is received, then the NAS in the AMF shall discard that message."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** the quoted paragraph is word for word the same in all six versions read. Other parts of the clause change: V16.3.0 splits the REGISTRATION REQUEST rule into initial and mobility registration, and V18.0.0 adds a sentence on an unsuccessful authentication (both recorded for NAS-BADMAC-REGISTRATION-AFTER-SMC, issue #253). Neither touches a message the check sends.
- **Scope of the check:** the clause lists messages the AMF processes even when the MAC fails (REGISTRATION REQUEST, DEREGISTRATION REQUEST, SERVICE REQUEST and others), but only "until the secure exchange of NAS messages has been established". The stimulus is a UL NAS TRANSPORT, which is on no such list, sent after the security mode control procedure completed on the same connection. So the discard paragraph applies without the clause's ambiguity about REGISTRATION REQUEST.
- **Use in the check:**
  1. The UE registers and sends a PDU SESSION ESTABLISHMENT REQUEST in a UL NAS TRANSPORT, with a fresh PDU session identity and procedure transaction identity. It must be answered (any 5GSM message naming them, the request returned by the AMF, or a PDU SESSION RESOURCE SETUP REQUEST for the session), else BLOCKED on the PDU session establishment: the core does not answer the message type the check relies on.
  2. It sends a second such request, protected at its next uplink NAS COUNT, with the four MAC octets inverted.
  3. It watches for 5 s. An answer naming the second request's identities is FAIL (the message was processed), and so is a 5GMM STATUS (an answer is not a discard).
  4. It sends a third, valid request. If that one is answered, PASS. If it is not, while the first one was, the discarded message changed the AMF's state: FAIL.
- **An action on the UE's state is FAIL.** Within the same 5 s the check also watches for the AMF acting on what the UE already has: a PDU SESSION RESOURCE RELEASE COMMAND or PDU SESSION RESOURCE MODIFY REQUEST, a PDU SESSION RELEASE COMMAND or PDU SESSION MODIFICATION COMMAND, or a network-initiated DEREGISTRATION REQUEST. Before the stimulus the downlink settled for 1 s after the first request was answered, and the UE sends nothing else in the window, so only the bad-MAC message can have caused such an action. A discarded message changes nothing, so an action is FAIL on the same shalls as an answer, and Observed names each message and how long after the stimulus it came. The window is the check's existing quiet bound: the one the discard itself is inferred within, not tuned to any core. An action seen only after the next valid request was sent is recorded in the detail and not judged, since that request could have caused it.
- **A release is WARN, not PASS and not FAIL.** If the AMF releases the UE (UE CONTEXT RELEASE COMMAND, or the association goes) after the bad MAC, the message was not answered, but "discard" does not describe a release either, and the third request cannot be sent. No shall is observably violated; the release acts on a message that failed the integrity check (security).
- **Why a UE release stays WARN.** A UE CONTEXT RELEASE COMMAND moves the UE to 5GMM-IDLE and leaves its registration, its NAS security context and its PDU sessions in place, and an AMF may release a UE's N2 connection for local reasons of its own. It does not show that the message's content was processed, so no shall is observably violated. A PDU session released or modified, or the UE deregistered, changes the state the discard protects, and only the message can have caused it.
- **5G-IA0:** integrity verification is not applicable under 5G-IA0 (4.4.3.3). The UE harness refuses 5G-IA0, so a core that selects it is SKIP for the missing input "NAS integrity protection".
- **Cleanup:** the UE sends a PDU SESSION RELEASE REQUEST for each session the core set up (best effort, the answer is not awaited), then a switch-off DEREGISTRATION REQUEST, waits up to 3 s for the UE CONTEXT RELEASE COMMAND, and drops the association whatever came back. The detail says which happened. If the AMF's state was changed by the bad MAC it may not accept the deregistration, and then keeps the UE until its own timers expire.

## TS 33.501 6.4.3.3 (NAS integrity failure handling), shall

> "The supervision of failed NAS integrity checks shall be performed both in the ME and the AMF. In case of failed integrity check (i.e. faulty or missing NAS-MAC) is detected after the start of NAS integrity protection, the concerned message shall be discarded except for some NAS messages specified in TS 24.501 [35]. For those exceptions the AMF shall take the actions specified in TS 24.501 [35] when receiving a NAS message with faulty or missing NAS-MAC. Discarding NAS messages can happen on the AMF side or on the ME side."

- **Since:** Rel-15, V15.0.0 (`33501-f00.zip`).
- **Changes:** none. The paragraph is the same in all six versions read, V15.0.0 through V19.7.0.
- **Use in the check:** the same discard, stated by the security specification. The exceptions it points to are the lists in TS 24.501 4.4.4.3, which do not include UL NAS TRANSPORT.

## Verdict

- **Class:** conformance, two shalls (TS 24.501 4.4.4.3, TS 33.501 6.4.3.3).
- **Stimulus:** after a registration and an answered PDU SESSION ESTABLISHMENT REQUEST, a second request with the four MAC octets inverted, watched for 5 s, then a third, valid request awaited for 10 s.
- **PASS** `discarded`: nothing answered the bad-MAC request within 5 s and the third request was answered. The discard is inferred from that absence, within the check's bound.
- **FAIL** `answered`, `5gmm-status` (4.4.4.3, 6.4.3.3): an answer naming the bad-MAC request's identities, or a 5GMM STATUS after it.
- **FAIL** `acted-on` (4.4.4.3, 6.4.3.3): no answer, but within 5 s of the bad MAC the AMF released or modified a PDU session of the UE (PDU SESSION RESOURCE RELEASE COMMAND or MODIFY REQUEST, PDU SESSION RELEASE or MODIFICATION COMMAND) or deregistered it. Judged within the check's 5 s bound, before the third request is sent; a release of the UE after the action does not change the verdict.
- **FAIL** `state-changed` (4.4.4.3, 6.4.3.3): no answer to the third, valid request within 10 s, while the first was answered. The absence is inferred within that bound.
- **WARN (security)** `released`: the AMF released the UE, or the association went, after the bad MAC. No requirement is violated, since the message was not answered; the criterion is security, as the core acts on a message it had the means to reject.
- **BLOCKED** (PDU session establishment): the first, valid request got no answer within 10 s, or the UE was released while it was being answered. The check's needs include a PDU session, so the run's preflight has shown that this configuration gets one. **BLOCKED** (registration): the UE did not register.
- **CRASH:** the AMF no longer completes an NG Setup after the stimulus.
- **ERROR:** the harness could not build, send or read; the NAS probe could not be opened; a local socket failed.
- **SKIP:** no UE configured; the core selected 5G-IA0 (no NAS integrity protection); a target release older than Rel-15.
- **Limits:** a discard is inferred from the absence of an answer within 5 s. A release is not told from a lost association in the verdict; the detail says which.
