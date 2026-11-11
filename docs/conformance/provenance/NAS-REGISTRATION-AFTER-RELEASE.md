# NAS-REGISTRATION-AFTER-RELEASE

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). Versions read (2026-11-10): V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`). Only TS 24.501 is cited; no clause of TS 38.413 or TS 23.502 is relied on.

## TS 24.501 5.5.1.2.4 (Initial registration accepted by the network), shall, primary

> "If the initial registration request is accepted by the network, the AMF shall send a REGISTRATION ACCEPT message to the UE."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** the sentence is the same in all six versions read.

## TS 24.501 5.5.1.2.5 (Initial registration not accepted by the network), shall

> "If the initial registration request cannot be accepted by the network, the AMF shall send a REGISTRATION REJECT message to the UE including an appropriate 5GMM cause value."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** the sentence is the same in all six versions read.

## How the two shalls are used

Each shall is conditional, and neither alone obliges an answer. Together they do: an initial registration request the AMF receives is either accepted or one the network cannot accept, and each case ends with a message to the UE. The check reads them that way, and that reading is its one inference: no single sentence read says "the AMF shall answer every REGISTRATION REQUEST".

5.5.1.2.3, in every version read, lets the procedure start with other messages first: "The network may initiate 5GMM common procedures, e.g. the identification, authentication and security procedures during the registration procedure, depending on the information received in the REGISTRATION REQUEST message." So the check does not wait for the Accept or the Reject itself. It asks for less: any NAS message for the new registration, such as the AUTHENTICATION REQUEST. A core that sends nothing at all has sent neither the Accept nor the Reject, whichever case applied.

The network-side abnormal cases of 5.5.1.2.8 were read in full, 2026-11-11, in all six versions (V15.0.0, V15.7.0, V16.15.0, V17.18.1, V18.14.0, V19.8.0) for one that lets the AMF stay silent here. None does. Its cases are a) a lower layer failure, b) a protocol error (answered with a REGISTRATION REJECT), c) T3550 expiry, d) a REGISTRATION REQUEST received after the REGISTRATION ACCEPT and before the REGISTRATION COMPLETE (aborted and progressed, or the ACCEPT resent), e) more than one initial REGISTRATION REQUEST before any ACCEPT or REJECT, f) an initial REGISTRATION REQUEST in state 5GMM-REGISTERED (the old context deleted and the new request progressed; from V18.14.0 the context kept unchanged when the request is found not to come from a genuine UE), g) a mobility or periodic REGISTRATION REQUEST before the REGISTRATION COMPLETE, h) a DEREGISTRATION REQUEST before the REGISTRATION COMPLETE, and, from V15.7.0, i) invalid UE security capabilities (answered with a REGISTRATION REJECT) and, from V16.15.0 in the versions read, j) CAG restrictions (a reject). Only e) 2) lets the network ignore a REGISTRATION REQUEST: a second initial request with the same IEs while the first has been answered by neither ACCEPT nor REJECT. The check's request is the only one outstanding, the registration before it having completed and ended with a switch-off deregistration, so no case covers it and the FAIL stands.

## TS 24.501 5.5.2.2.2 (UE-initiated de-registration procedure completion), descriptive

> "When the DEREGISTRATION REQUEST message is received by the AMF, the AMF shall send a DEREGISTRATION ACCEPT message to the UE, if the De-registration type IE does not indicate "switch off". Otherwise, the procedure is completed when the AMF receives the DEREGISTRATION REQUEST message."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** the sentences are the same in all six versions read. V15.0.0 titles the clause "UE-initiated de-registration procedure completion - common procedure"; V15.7.0 onward "UE-initiated de-registration procedure completion".
- **Use in the check:** the second sentence is why the request that follows is an ordinary initial registration. The deregistration was complete when the AMF received it, before the release the check waits for. 5.5.2.2.3, in all six versions, adds "The AMF shall enter the state 5GMM-DEREGISTERED for 3GPP access."

## TS 24.501 Table 10.2.1 T3510 (Timers of 5GS mobility management, UE side), descriptive

T3510 is the UE's timer for the answer: started at "Transmission of REGISTRATION REQUEST message", stopped at "REGISTRATION ACCEPT message received or REGISTRATION REJECT message received", value 15s.

- **Since:** Rel-15, V15.0.0.
- **Changes:** Rel-16 (V16.15.0) adds "In WB-N1/CE mode, 85s". Rel-17 onward (V17.18.1, V18.14.0, V19.8.0) adds "For access via a satellite NG-RAN cell, 27s". The normal value is 15s in all six versions.
- **Use in the check:** the check waits 10 s for the first NAS message, the bound the suite's other registration checks use, which is inside the 15 s a UE itself waits. The table is not asserted.

## The stimulus

The UE registers, without a PDU session, and sends a switch-off DEREGISTRATION REQUEST. When the core's UE CONTEXT RELEASE COMMAND arrives, the gNB sends the UE CONTEXT RELEASE COMPLETE and then an INITIAL UE MESSAGE with a new initial REGISTRATION REQUEST for the same SUPI (a SUCI, as at the first registration), on the same association under the next RAN UE NGAP ID. Both messages are well formed and in the order a gNB sends them.

The check does this four times, with a pause of 0, 5, 10 and 20 ms between the two messages: a core can answer a request that arrives together with the complete and still miss one that arrives while it clears the UE context, so the pauses step through the first tens of milliseconds. The gNB's association is set to send each PDU as it is written (SCTP_NODELAY); without that the kernel would hold the request until the complete is acknowledged, about 200 ms. With no pause the request is written as soon as the engine has written the complete, some tens of microseconds later; the detail gives the gap as measured for each registration.

A registration that is answered is completed, and it is the registered UE of the next attempt. One that does not complete has its association closed; the check then waits 1 s and registers the same SUPI on a new association for the next attempt. The same SUPI is used throughout, since the check is about one subscriber's release and next registration.

## Verdict

- **Class:** conformance, on the two shalls. Needs the registration, owned by `NAS-INITIAL-REGISTRATION`.
- **PASS** `answered`: every registration drew a NAS message within 10 s. Observed: `REGISTRATION REQUEST sent right after the same UE's UE CONTEXT RELEASE COMPLETE answered every time`. A REGISTRATION REJECT is an answer; so is an AUTHENTICATION REQUEST after which the registration stalls. The detail says how each registration went on.
- **FAIL** `unanswered`: a registration drew no NAS message within 10 s, inferred from the absence within that bound (a bound of the check, inside the 15 s of T3510). Observed: `no NAS answer to a REGISTRATION REQUEST sent right after the same UE's UE CONTEXT RELEASE COMPLETE`. A UE CONTEXT RELEASE COMMAND or an ERROR INDICATION with no NAS message is not an answer; the detail names what came instead. One unanswered registration is enough, whatever the others drew. Message: "the AMF did not answer an initial REGISTRATION REQUEST that arrived right after the UE CONTEXT RELEASE COMPLETE for the same subscriber's previous UE context. TS 24.501 5.5.1.2.4, 5.5.1.2.5. Likely cause: the AMF drops a registration that arrives while it is still clearing the released UE context."
- **INFO** `no-release`: the core sent no UE CONTEXT RELEASE COMMAND within 5 s of a switch-off deregistration, so there was no release to send the registration after (`core sent no UE CONTEXT RELEASE COMMAND after the switch-off deregistration; the registration after the release could not be sent`). The requirement was not exercised. No clause the check cites asks for the release (5.5.2.2.2 completes a switch-off deregistration when the AMF receives the request), the release is not one of the procedures a BLOCKED can name, and `NGAP-CTX-RELEASE-AFTER-SWITCH-OFF` reports on it.
- **BLOCKED:** the first registration did not complete, or a registration of the same SUPI between attempts did not (`blocked-registration`, owned by `NAS-INITIAL-REGISTRATION`); the NG Setup not completed (`blocked-ng-setup`).
- **ERROR:** the harness could not be set up, a local socket failed, an attempt could not be sent for another reason than a missing release, or the check's time ran out.
- **SKIP:** no UE configured, or a target release older than Rel-15.
- **CRASH:** a fresh NG Setup is tried afterwards; a core that no longer completes it is CRASH (`AMF unresponsive after the message (...)`).
- **Detail:** per registration, the pause asked for and measured, the first NAS message and how long it took, and how the registration ended. It never includes key material.
- **State left:** the UE is deregistered (switch-off) at the end. After an unanswered registration the UE's previous context had been released and the new request drew nothing, so the core holds at most a REGISTRATION REQUEST it did not answer, and the association is closed. When the last registration is the unanswered one, the check registers the SUPI once more after 1 s and deregisters it; the detail says whether that registration completed. If it did not, the detail says so and the SUPI is left as the core holds it.
- **Subscribers:** one, from the check's own block of the pool.
- **Budget:** up to 124 s: 10 s for the first registration; per attempt 5 s for the release, 10 s for the new registration and, when it does not complete, 1 s and up to 10 s for the fresh one; 5 s for the final deregistration and 5 s for the liveness probe. A core that answers takes a few seconds.
