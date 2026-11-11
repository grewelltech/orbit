# NAS-REGISTRATION-DURING-OTHER-RELEASE

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). The references are those of `NAS-REGISTRATION-AFTER-RELEASE`, read there in V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`) and V19.8.0 (`24501-j80.zip`); the quotes and the version notes below are taken from that provenance, not read again. Only TS 24.501 is cited; no clause of TS 38.413 or TS 23.502 is relied on.

## TS 24.501 5.5.1.2.4 (Initial registration accepted by the network), shall, primary

> "If the initial registration request is accepted by the network, the AMF shall send a REGISTRATION ACCEPT message to the UE."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** the sentence is the same in all six versions read.

## TS 24.501 5.5.1.2.5 (Initial registration not accepted by the network), shall

> "If the initial registration request cannot be accepted by the network, the AMF shall send a REGISTRATION REJECT message to the UE including an appropriate 5GMM cause value."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** the sentence is the same in all six versions read.

## How the two shalls are used

As in `NAS-REGISTRATION-AFTER-RELEASE`: each shall is conditional, and together they give every initial registration request the AMF receives an answer, an Accept when it is accepted and a Reject when it cannot be. That reading is the check's one inference. 5.5.1.2.3 lets the procedure start with common procedures ("The network may initiate 5GMM common procedures, e.g. the identification, authentication and security procedures during the registration procedure, depending on the information received in the REGISTRATION REQUEST message."), so any NAS message for the registration, such as the AUTHENTICATION REQUEST, counts as the answer having begun.

The request here comes from a UE other than the one being released, on another NG association, so nothing in the text ties it to the released UE at all: the AMF receives an INITIAL UE MESSAGE on a UE-associated logical connection of its own. The abnormal cases of 5.5.1.2.8, which `NAS-REGISTRATION-AFTER-RELEASE`'s provenance lists for all six versions, were checked against this stimulus: only e) 2) lets the network ignore a REGISTRATION REQUEST, a second initial request with the same IEs while the first is unanswered. The other UE's request is the only one it has outstanding (its previous registration, if any, completed and ended with a switch-off deregistration), so no case covers it and the FAIL stands.

## TS 24.501 5.5.2.2.2 (UE-initiated de-registration procedure completion), descriptive

> "When the DEREGISTRATION REQUEST message is received by the AMF, the AMF shall send a DEREGISTRATION ACCEPT message to the UE, if the De-registration type IE does not indicate "switch off". Otherwise, the procedure is completed when the AMF receives the DEREGISTRATION REQUEST message."

- **Since:** Rel-15, V15.0.0. **Changes:** as in `NAS-REGISTRATION-AFTER-RELEASE` (the clause title changes by V15.7.0; the sentences do not).
- **Use in the check:** the released UE's deregistration was complete at the AMF before the release the stimulus is timed against, and when the other UE is the subscriber the previous attempt registered, its request is an ordinary initial registration too.

## TS 24.501 Table 10.2.1 T3510 (Timers of 5GS mobility management, UE side), descriptive

T3510 is the UE's 15 s wait for the REGISTRATION ACCEPT or REJECT (85 s in WB-N1/CE mode from Rel-16, 27 s via a satellite NG-RAN cell from Rel-17). The check waits 10 s for the first NAS message, the bound the suite's other registration checks use, inside the UE's own wait. The table is not asserted.

## The stimulus

Two subscribers, A and B, consecutive SUPIs from the check's block of the pool, and two gNBs: the check's own and a second one with its own gNB ID (the run's handover-target ID for the check), each with its own NG association from the run's N2 source address.

1. A registers on the first gNB, without a PDU session.
2. The second gNB opens its association and completes NG Setup, before anything else happens, so the only message timed is the INITIAL UE MESSAGE.
3. A sends a switch-off DEREGISTRATION REQUEST. When the core's UE CONTEXT RELEASE COMMAND for A arrives, the first gNB sends the UE CONTEXT RELEASE COMPLETE and, after the attempt's gap, the second gNB sends an INITIAL UE MESSAGE with B's initial REGISTRATION REQUEST (a SUCI). The first gNB keeps its association until B's registration has ended, as a gNB keeps its NG association when one of its UEs leaves.
4. Both associations send each PDU as it is written (SCTP_NODELAY), so the complete is not held behind an unacknowledged write and cannot reach the AMF after the request; the detail gives the gap as measured, from the complete written to the request written.

The next attempt swaps the roles: B, registered on the second gNB, is deregistered, and A registers on the first gNB (a new association with the first gNB's ID, whose old one is closed by then). A registration that does not complete has its association closed; the check waits 1 s and registers that subscriber on the first gNB, and the next attempt starts from there.

### The gaps

0, 10, 20, 30, 40 and 50 ms: six attempts, evenly spaced from 0 to 50 ms.

- **Why a sweep, and why evenly spaced.** The requirement is that the AMF answers an initial registration whenever it arrives. Another UE's registration arrives at a time that has nothing to do with a release in progress, so no gap is more legitimate than another, and the check does not pick gaps to land in any one core's window: it samples the interval at a fixed, even step and holds the core to the same requirement at each.
- **Why from 0.** A UE CONTEXT RELEASE COMPLETE and another UE's INITIAL UE MESSAGE can reach the AMF together; 0 ms is the request written as soon as the complete is.
- **Why up to 50 ms.** The interval of interest is the one in which the AMF may still be acting on the release it has just been told is complete. No 3GPP text bounds how long that takes, so the bound is the check's: a test-design choice of an interval expected to cover a core's own release work on a lab deployment, where the core's release round trip (the time from the DEREGISTRATION REQUEST to the UE CONTEXT RELEASE COMMAND) is a few milliseconds. The detail records that round trip for every attempt, so a reader can see whether the sweep reached well past it. The check samples; it does not claim that a core answering at every sampled gap answers at every gap. The step, 10 ms, gives six attempts within the budget; a finer step would add attempts without a requirement-based reason to sample one part of the interval more densely.

## Verdict

- **Class:** conformance, on the two shalls. Needs the NG Setup, owned by `NGAP-NG-SETUP`; the registration, owned by `NAS-INITIAL-REGISTRATION`; and the release after switch-off, owned by `NGAP-CTX-RELEASE-AFTER-SWITCH-OFF`.
- **PASS** `answered`: every registration drew a NAS message within 10 s. Observed: `another UE's REGISTRATION REQUEST sent right after a UE CONTEXT RELEASE COMPLETE answered every time`. A REGISTRATION REJECT is an answer; so is an AUTHENTICATION REQUEST after which the registration stalls.
- **FAIL** `unanswered`: a registration drew no NAS message within 10 s, inferred from the absence within that bound (a bound of the check, inside the 15 s of T3510). Observed: `no NAS answer to another UE's REGISTRATION REQUEST sent right after a UE CONTEXT RELEASE COMPLETE`. A UE CONTEXT RELEASE COMMAND or an ERROR INDICATION with no NAS message is not an answer; the detail names what came instead. One unanswered registration is enough, whatever the others drew. Message: "the AMF did not answer an initial REGISTRATION REQUEST that one UE sent, on its own NG association, right after the UE CONTEXT RELEASE COMPLETE for another UE. TS 24.501 5.5.1.2.4, 5.5.1.2.5. Likely cause: the AMF takes an INITIAL UE MESSAGE that arrives while it clears another UE's context as part of that context, and drops it."
- **BLOCKED** `blocked-release-after-switch-off`: the core sent no UE CONTEXT RELEASE COMMAND within 5 s of a switch-off deregistration (`core sent no UE CONTEXT RELEASE COMMAND after the switch-off deregistration; the other UE's registration could not be sent`). The remaining attempts are not tried.
- **BLOCKED** `blocked-ng-setup`: the second gNB's NG Setup did not complete (`the NG Setup of the other UE's gNB did not complete`), or the first gNB's at the start. The remaining attempts are not tried. A local socket fault is ERROR instead.
- **BLOCKED** `blocked-registration`: the first registration did not complete, or a registration between attempts did not.
- **ERROR:** the harness could not be set up, a local socket failed, an attempt could not be sent for another reason, or the check's time ran out.
- **SKIP:** no UE configured, or a target release older than Rel-15.
- **CRASH:** a fresh NG Setup is tried afterwards; a core that no longer completes it is CRASH.
- **Detail:** per registration, the gap asked for and measured, the first NAS message and how long it took, how the registration ended, and how long the core took to release the deregistered UE. With a subscriber pool of one SUPI the other UE is that same subscriber on its own association, and the detail says so. It never includes key material.
- **State left:** the registered UE is deregistered (switch-off) at the end. After an unanswered last registration the check registers that subscriber once more after 1 s and deregisters it; the detail says whether that registration completed.
- **Subscribers:** two, from the check's own block of the pool. **gNB IDs:** the check's own, its second (handover-target) ID and the liveness probe's.
- **Budget:** up to 200 s: 10 s for the first registration; per attempt 4 s for the second gNB's NG Setup, 5 s for the release, the gap, 10 s for the registration and, when it does not complete, 1 s and up to 10 s for the fresh one; 5 s for the final deregistration and 5 s for the liveness probe. A core that answers takes a few seconds.
