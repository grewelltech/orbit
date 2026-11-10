# NAS-REPLAY-DISCARD

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/](https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/) and [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). Versions read (2026-11-10; issue #143, step 0):

- TS 33.501: V15.0.0 (`33501-f00.zip`), V15.20.0 (`33501-fk0.zip`), V16.20.0 (`33501-gk0.zip`), V17.16.0 (`33501-hg0.zip`), V18.12.0 (`33501-ic0.zip`), V19.7.0 (`33501-j70.zip`).
- TS 24.501: V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`).

The issue named TS 33.501 6.4.3 and TS 24.501 4.4.3 as candidates. The read pinned them to 6.4.3.2 and 4.4.3.2.

## TS 33.501 6.4.3.2 (NAS integrity activation), shall

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
  1. The UE registers and sends a PDU SESSION ESTABLISHMENT REQUEST in a UL NAS TRANSPORT, with a fresh PDU session identity and procedure transaction identity. It must be answered, else ERROR. The check then lets the downlink settle for 1 s, so that anything more the core sends for this request is read before the replay.
  2. It sends the same NAS PDU again, byte for byte. No other uplink NAS message was sent in between, so the repeat carries exactly the NAS COUNT the AMF accepted last. This is the equal-COUNT case; a repeat of an older message, whose sequence number is lower than the last accepted one, is NAS-COUNT-NOT-ADVANCED-ON-MAC-FAIL (issue #251).
  3. It watches for 5 s. A further answer naming the request's identities (a 5GSM message, the request returned by the AMF, or a PDU SESSION RESOURCE SETUP REQUEST for the session) is FAIL: the COUNT was accepted twice. A 5GMM STATUS is FAIL too, since an answer is not a discard.
  4. It sends a second, valid request at the next NAS COUNT. Answered: PASS. Not answered, while the first one was: FAIL, since the repeat changed the AMF's state.
- **How the AMF discards is not asserted.** "No specific mechanism is required": an AMF may find the repeat by its COUNT, or estimate a higher overflow counter for it so that its MAC fails. Both leave the repeat unanswered, and both PASS.
- **A release is ERROR.** As for NAS-INTEGRITY-FAIL-DISCARD, a UE released after the repeat was not answered, but the second request cannot be judged.
- **5G-IA0:** replay protection is not applicable. The UE harness refuses 5G-IA0, so a core that selects it is SKIP.
- **Cleanup:** as NAS-INTEGRITY-FAIL-DISCARD: a PDU SESSION RELEASE REQUEST per session set up, a switch-off DEREGISTRATION REQUEST, up to 3 s for the UE CONTEXT RELEASE COMMAND, and then the association is dropped.
