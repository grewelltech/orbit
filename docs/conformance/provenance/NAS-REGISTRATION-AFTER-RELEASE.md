# NAS-REGISTRATION-AFTER-RELEASE

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). Versions read (2026-11-10): V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`). Only TS 24.501 is cited; no clause of TS 38.413 or TS 23.502 is relied on.

## TS 24.501 5.5.1.2.4 (Initial registration accepted by the network), shall

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

The network-side abnormal cases of 5.5.1.2.8 were read in V15.0.0 and V19.8.0 for one that lets the AMF stay silent here. None does. Its cases are a lower layer failure, a protocol error (answered with a REGISTRATION REJECT), T3550 expiry, a REGISTRATION REQUEST received while an earlier registration of the UE is still in progress or in state 5GMM-REGISTERED, a DEREGISTRATION REQUEST before the REGISTRATION COMPLETE and, in V19.8.0, invalid UE security capabilities and CAG restrictions. Case e) has the network ignore a second, identical REGISTRATION REQUEST while the first is unanswered; the check's request is the only one outstanding, the registration before it having completed and ended.

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

## Verdicts

- **PASS:** every registration drew a NAS message within 10 s. Observed: `REGISTRATION REQUEST sent right after the same UE's UE CONTEXT RELEASE COMPLETE answered every time`. A REGISTRATION REJECT is an answer; so is an AUTHENTICATION REQUEST after which the registration stalls. The detail says how each registration went on.
- **FAIL:** a registration drew no NAS message within 10 s. Observed: `no NAS answer to a REGISTRATION REQUEST sent right after the same UE's UE CONTEXT RELEASE COMPLETE`. A UE CONTEXT RELEASE COMMAND or an ERROR INDICATION with no NAS message is not an answer; the detail names what came instead. One unanswered registration is enough, whatever the others drew. Message: "the AMF did not answer an initial REGISTRATION REQUEST that arrived right after the UE CONTEXT RELEASE COMPLETE for the same subscriber's previous UE context. TS 24.501 5.5.1.2.4, 5.5.1.2.5. Likely cause: the AMF drops a registration that arrives while it is still clearing the released UE context."
- **SKIP:** the core sent no UE CONTEXT RELEASE COMMAND after a switch-off deregistration, so there was no release to send the registration after (`core sent no UE CONTEXT RELEASE COMMAND after the switch-off deregistration; the registration after the release could not be sent`); `NGAP-CTX-RELEASE-AFTER-SWITCH-OFF` reports on that release. Also no UE configured, or a target release older than Rel-15.
- **ERROR:** the first registration failed, or a later attempt could not be made for a reason other than a missing release and no registration went unanswered.
- **Crash:** a fresh NG Setup is tried afterwards; a core that no longer completes it is FAIL (`AMF unresponsive after the message (...)`).
- **Detail:** per registration, the pause asked for and measured, the first NAS message and how long it took, and how the registration ended. It never includes key material.
- **State left:** the UE is deregistered (switch-off) at the end. After an unanswered registration the UE's previous context had been released and the new request drew nothing, so the core holds at most a REGISTRATION REQUEST it did not answer, and the association is closed. When the last registration is the unanswered one, the check registers the SUPI once more after 1 s and deregisters it; the detail says whether that registration completed. If it did not, the detail says so and the SUPI is left as the core holds it.
- **Subscribers:** one, from the check's own block of the pool.
- **Budget:** up to 124 s: 10 s for the first registration; per attempt 5 s for the release, 10 s for the new registration and, when it does not complete, 1 s and up to 10 s for the fresh one; 5 s for the final deregistration and 5 s for the liveness probe. A core that answers takes a few seconds.
