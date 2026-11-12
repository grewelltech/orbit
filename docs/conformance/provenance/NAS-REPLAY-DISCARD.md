# NAS-REPLAY-DISCARD

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/](https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/) and [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). Versions read (2026-11-10; issue #143, step 0):

- TS 33.501: V15.0.0 (`33501-f00.zip`), V15.20.0 (`33501-fk0.zip`), V16.20.0 (`33501-gk0.zip`), V17.16.0 (`33501-hg0.zip`), V18.12.0 (`33501-ic0.zip`), V19.7.0 (`33501-j70.zip`).
- TS 24.501: V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`).

The issue named TS 33.501 6.4.3 and TS 24.501 4.4.3 as candidates. The read pinned them to 6.4.3.2 and 4.4.3.2.

## TS 33.501 6.4.3.2 (NAS integrity activation), shall, primary

> "Replay protection shall be activated when integrity protection is activated, except when the NULL integrity protection algorithm is selected. Replay protection shall ensure that the receiver only accepts each incoming NAS COUNT value once using the same NAS security context."

- **Since:** Rel-15, V15.0.0 (`33501-f00.zip`).
- **Changes:** none. The two sentences are the same in all six versions read, V15.0.0 through V19.7.0.
- **Use in the check:** the AMF is the receiver of uplink NAS. A NAS PDU sent twice carries one NAS COUNT, so the AMF may accept it once.

## TS 24.501 4.4.3.2 (Replay protection), shall

> "Replay protection shall be supported for received NAS messages both in the AMF and the UE. However, since the realization of replay protection does not affect the interoperability between nodes, no specific mechanism is required for implementation."
>
> "Replay protection assures that one and the same NAS message is not accepted twice by the receiver. Specifically, for a given 5G NAS security context, a given NAS COUNT value shall be accepted at most one time and only if message integrity verifies correctly."
>
> "Replay protection is not applicable when 5G-IA0 is used."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** none. The clause is the same in all six versions read, V15.0.0 through V19.8.0.
- **Use in the check:**
  1. The UE registers and sends a PDU SESSION ESTABLISHMENT REQUEST in a UL NAS TRANSPORT, with a fresh PDU session identity and procedure transaction identity. It must be answered, else BLOCKED on the PDU session establishment. The check then lets the downlink settle for 1 s, so that anything more the core sends for this request is read before the replay.
  2. It sends the same NAS PDU again, byte for byte. No other uplink NAS message was sent in between, so the repeat carries exactly the NAS COUNT the AMF accepted last. This is the equal-COUNT case; a repeat of an older message, whose sequence number is lower than the last accepted one, is NAS-COUNT-NOT-ADVANCED-ON-MAC-FAIL (issue #251).
  3. It watches for 5 s. A further answer naming the request's identities (a 5GSM message, the request returned by the AMF, or a PDU SESSION RESOURCE SETUP REQUEST for the session) is FAIL: the COUNT was accepted twice. A 5GMM STATUS is FAIL too, since an answer is not a discard.
  4. It sends a second, valid request at the next NAS COUNT. Answered: PASS. Not answered, while the first one was: FAIL, since the repeat changed the AMF's state.
- **An action on the UE's state is FAIL.** Within the same 5 s the check also watches for the AMF acting on what the UE already has: a PDU SESSION RESOURCE RELEASE COMMAND or PDU SESSION RESOURCE MODIFY REQUEST, a PDU SESSION RELEASE COMMAND or PDU SESSION MODIFICATION COMMAND, or a network-initiated DEREGISTRATION REQUEST. Before the stimulus the downlink settled for 1 s after the first request was answered, and the UE sends nothing else in the window, so only the repeat can have caused such an action. A discarded message changes nothing, so an action is FAIL on the same shalls as an answer, and Observed names each message and how long after the stimulus it came. The window is the check's existing quiet bound: the one the discard itself is inferred within, not tuned to any core. An action seen only after the next valid request was sent is recorded in the detail and not judged, since that request could have caused it.
- **The repeat names an existing session.** The repeat carries the PDU session identity of the session the first request set up. A core that processes it as a new request finds that identity in use, and may handle the collision by releasing the existing session without answering the request. The repeat then draws no NAS answer, yet the existing session is torn down: `acted-on`, not `discarded`.
- **How the AMF discards is not asserted.** "No specific mechanism is required": an AMF may find the repeat by its COUNT, or estimate a higher overflow counter for it so that its MAC fails. Both leave the repeat unanswered, and both PASS.
- **A release is WARN (security).** As for NAS-INTEGRITY-FAIL-DISCARD, a UE released after the repeat was not answered, but the second request cannot be judged, and the release acts on a replayed message. It stays WARN for the reason given there: a UE release leaves the registration and the sessions in place and may have local causes, so it does not show the repeat was processed.
- **5G-IA0:** replay protection is not applicable. The UE harness refuses 5G-IA0, so a core that selects it is SKIP for the missing input "NAS integrity protection".
- **Cleanup:** as NAS-INTEGRITY-FAIL-DISCARD: a PDU SESSION RELEASE REQUEST per session set up, a switch-off DEREGISTRATION REQUEST, up to 3 s for the UE CONTEXT RELEASE COMMAND, and then the association is dropped.

## Verdict

- **Class:** conformance, two shalls (TS 33.501 6.4.3.2, TS 24.501 4.4.3.2).
- **Stimulus:** after a registration and an answered PDU SESSION ESTABLISHMENT REQUEST, the same NAS PDU again, watched for 5 s, then a second, valid request awaited for 10 s.
- **PASS** `discarded`: nothing answered the repeat within 5 s and the second request was answered. The discard is inferred from that absence, within the check's bound.
- **FAIL** `answered`, `5gmm-status` (6.4.3.2, 4.4.3.2): a further answer naming the request's identities, or a 5GMM STATUS after the repeat.
- **FAIL** `acted-on` (6.4.3.2, 4.4.3.2): no answer, but within 5 s of the repeat the AMF released or modified a PDU session of the UE, typically the one the repeated request names (PDU SESSION RESOURCE RELEASE COMMAND or MODIFY REQUEST, PDU SESSION RELEASE or MODIFICATION COMMAND), or deregistered it: the repeated COUNT was accepted and the message processed. Judged within the check's 5 s bound, before the second request is sent. The action is attributed to the stimulus by timing alone: it comes inside the 5 s quiet window after it, the UE sends nothing else in that window, and the core's messages for the earlier requests have settled before the stimulus is sent.
- **FAIL** `state-changed` (6.4.3.2, 4.4.3.2): no answer to the second, valid request within 10 s, while the first was answered. The absence is inferred within that bound. The FAIL is an inference: the stimulus itself draws no answer, and the state change is read from the valid follow-up request, which the same kind of request answered before the stimulus, going unanswered after it.
- **WARN (security)** `released`: the AMF released the UE, or the association went, after the repeat. No requirement is violated; the criterion is security, as the core acts on a replayed message.
- **BLOCKED** (PDU session establishment): the first request got no answer within 10 s, or the UE was released while it was being answered. **BLOCKED** (registration): the UE did not register.
- **CRASH:** the AMF no longer completes an NG Setup after the stimulus.
- **ERROR:** the harness could not build, send or read; the NAS probe could not be opened; a local socket failed.
- **SKIP:** no UE configured; the core selected 5G-IA0 (no NAS integrity protection); a target release older than Rel-15.
- **Limits:** how the AMF discards is not asserted, and the discard is inferred from the absence of an answer within 5 s.
