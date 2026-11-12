# NAS-INITIAL-REGISTRATION

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). Versions read (2026-11-11): V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`).

## TS 24.501 5.5.1.2.4 (Initial registration accepted by the network), shall, primary

> "If the initial registration request is accepted by the network, the AMF shall send a REGISTRATION ACCEPT message to the UE."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** the sentence is the same in all six versions read.

## TS 24.501 5.5.1.2.5 (Initial registration not accepted by the network), shall

> "If the initial registration request cannot be accepted by the network, the AMF shall send a REGISTRATION REJECT message to the UE including an appropriate 5GMM cause value."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** the sentence is the same in all six versions read.

## How the two shalls are used

Each shall is conditional, and neither alone obliges an answer. Together they do: an initial registration request is either accepted by the network or one the network cannot accept, and each case ends with a message to the UE. A REGISTRATION ACCEPT meets the first; a REGISTRATION REJECT is what the second asks for, so it violates nothing. A core that sends neither has violated one of the two, whichever case applied. `NAS-REGISTRATION-AFTER-RELEASE` reads the pair the same way.

The network-side abnormal cases of 5.5.1.2.8 were read in all six versions for one that lets the AMF leave the request unanswered. Only e) 2) does: a second initial REGISTRATION REQUEST with the same information elements, received while the first has been answered by neither ACCEPT nor REJECT, is ignored. The check sends one request.

## Verdict

- **Class:** conformance, on two shalls. The check owns the procedure "registration": a check whose own registration the core does not complete reports BLOCKED and names this check.
- **Stimulus:** the run's own UE registers through the harness, as the run's preflight registers it: one association, an initial REGISTRATION REQUEST with the UE's SUCI, no PDU session. The answer is awaited for 10 s. An accepted UE is then deregistered.
- **PASS** `accepted`: REGISTRATION ACCEPT, and the UE registered.
- **FAIL** `unanswered`: neither REGISTRATION ACCEPT nor REGISTRATION REJECT within 10 s, inferred from their absence within that bound. The bound is the check's, the one the suite's other registrations use, and inside the 15 s of T3510, the UE's own wait for the answer (TS 24.501 Table 10.2.1). Messages of the procedure before the answer (AUTHENTICATION REQUEST, SECURITY MODE COMMAND) are not an answer: 5.5.1.2.4 and 5.5.1.2.5 ask for the ACCEPT or the REJECT.
- **FAIL** `released`: a UE CONTEXT RELEASE COMMAND with neither REGISTRATION ACCEPT nor REGISTRATION REJECT.
- **WARN (availability)** `rejected`: REGISTRATION REJECT, its cause in Observed. It follows the shall of 5.5.1.2.5, so no requirement is violated; the core does not complete the registration for the subscriber the run is configured with. A Reject the UE could not read is the same outcome.
- **WARN (availability)** `authentication-rejected`: AUTHENTICATION REJECT. It ends the registration within the authentication procedure, which the two shalls do not cover, so no requirement the check cites is violated; the core does not complete the registration for the run's subscriber.
- **WARN (availability)** `not-completed`: the registration ended another way (the association lost, an answer the UE could not use), with the reason in the detail. No clause the check cites says what that violates; the procedure did not complete.
- **Not completed:** every outcome but `accepted` reports the registration as not completed, so the BLOCKED results of dependent checks in the same run are counted under this one.
- **BLOCKED:** the check's own NG Setup did not complete (`blocked-ng-setup`, owned by `NGAP-NG-SETUP`).
- **ERROR:** the harness could not be set up, a local socket failed, or the per-test timeout expired before the 10 s wait.
- **SKIP:** no UE configured, or a target release older than Rel-15.
- **CRASH:** a fresh NG Setup is tried afterwards; a core that no longer completes it is CRASH.
- **Limits:** the run's preflight registers the same subscriber before any check runs, so this check reports a core that registered it once and does not register it now. The content of the REGISTRATION ACCEPT is not asserted, nor is the deregistration that follows; the detail says how it went. Budget: 20 s.
