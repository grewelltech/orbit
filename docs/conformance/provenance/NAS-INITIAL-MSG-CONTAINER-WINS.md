# NAS-INITIAL-MSG-CONTAINER-WINS

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/) and [https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/](https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/). Versions read (2026-11-10): TS 24.501 V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`); TS 33.501 V15.0.0 (`33501-f00.zip`), V15.20.0 (`33501-fk0.zip`), V16.20.0 (`33501-gk0.zip`), V17.16.0 (`33501-hg0.zip`), V18.12.0 (`33501-ic0.zip`), V19.7.0 (`33501-j70.zip`).

## TS 24.501 5.4.2.4 (NAS security mode control completion by the network), shall, primary

> "If the SECURITY MODE COMPLETE message contains a NAS message container IE with a REGISTRATION REQUEST message, the AMF shall complete the ongoing registration procedure by considering the REGISTRATION REQUEST message contained in the NAS message container IE as the message that triggered the procedure."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** V15.0.0 reads "contains a Replayed NAS container message IE with an REGISTRATION REQUEST message ... contained in the Replayed NAS message container IE". From V15.7.0 through V19.8.0 the sentence is as quoted, word for word. Later versions add sentences of the same form for the SERVICE REQUEST (V15.7.0), the CONTROL PLANE SERVICE REQUEST (V16.15.0) and the DEREGISTRATION REQUEST (V18.14.0), which this check does not use.

## TS 33.501 6.4.6 step 4 (Protection of initial NAS message), shall

> "The AMF shall use the complete initial NAS message that is in the NAS container as the message to respond to."

- **Since:** Rel-15, V15.0.0 (`33501-f00.zip`).
- **Changes:** V15.0.0 protects the initial NAS message with a hash: the UE resends the complete message in the NAS Security Mode Complete "if the check of the hash failed", and "In this case, the AMF shall treat this as the initial NAS message to respond to." From V15.20.0 through V19.7.0 step 4 has the UE include "the complete initial NAS message in a NAS Container if either requested by the AMF or the UE sent the initial NAS message unprotected", followed by the sentence quoted, unchanged.

## Also read, not cited

- **TS 24.501 4.4.6 a)** (V15.7.0 onward; V15.0.0 has no clause 4.4.6): a UE without a valid 5G NAS security context "sends a REGISTRATION REQUEST message including cleartext IEs only" and "shall include the entire REGISTRATION REQUEST message ... in the NAS message container IE" of the SECURITY MODE COMPLETE. The Requested NSSAI is not in the list of cleartext IEs. These sentences bind the UE. The last paragraphs of 4.4.6 on the AMF ("shall consider the NAS message that is obtained from the NAS message container IE as the initial NAS message that triggered the procedure") are about an integrity protected initial NAS message that carries the container itself, which is not the case here: the SECURITY MODE COMPLETE carries it, and 5.4.2.4 is the clause for that.
- No clause read says what the AMF does with an initial REGISTRATION REQUEST that has non-cleartext IEs in the clear. A core that rejects such a message before the security mode control procedure is therefore not judged.

## Why the stimulus smuggles a non-cleartext IE

The two copies must differ in something the answer shows. Of the cleartext IEs, the UE security capability decides the SECURITY MODE COMMAND, which is sent before the container arrives, and the 5GS registration type and the mobile identity leave nothing in the answer to read. The Requested NSSAI does: the Allowed NSSAI and Rejected NSSAI, or the reject cause, follow from it. So the initial message carries a Requested NSSAI in the clear, which a conformant UE never does.

## What the check does

Three registrations of the same UE, each without a PDU session, each over its own association:

1. **Stimulus.** The initial REGISTRATION REQUEST is the complete request with its Requested NSSAI replaced by an S-NSSAI the core does not serve (the run's SST with SD EEEEEE, or DDDDDD when the run's SD is EEEEEE). The SECURITY MODE COMPLETE's NAS message container carries the complete request with the run's own S-NSSAI.
2. **Reference, served.** A conformant registration (cleartext IEs only in the initial message) requesting the run's own S-NSSAI.
3. **Reference, unserved.** A conformant registration requesting the unserved S-NSSAI.

Each answer is reduced to one line: "Registration Accept, allowed NSSAI ..., rejected NSSAI ..." or "Registration Reject, 5GMM cause #n, rejected NSSAI ...". The references say how this core answers each value when it is sent protected only, so the check does not assume what an unserved S-NSSAI gets.

## Verdict

- **Class:** conformance, on the two shalls. Needs the registration, owned by `NAS-INITIAL-REGISTRATION`.
- **Stimulus:** the three registrations above, without a PDU session, each awaited for 10 s.
- **PASS** `container-answered`: the stimulus is answered as reference 2 is, and reference 3 is answered differently. Observed: `registration answered on the request in the NAS message container`.
- **FAIL** `unprotected-answered`: the stimulus is answered as reference 3 is. Observed: `registration answered on the unprotected Requested NSSAI, not the one in the NAS message container`.
- **FAIL** `other-answer`: the stimulus is answered otherwise than reference 2, a release after the SECURITY MODE COMPLETE included. Observed: `registration not answered as the request in the NAS message container is: <answer>`.
- **FAIL** `unanswered`: the stimulus drew neither a Registration Accept nor a Reject within 10 s after the SECURITY MODE COMPLETE, where reference 2 was accepted. The bound is the check's, inside T3510, the UE's own 15 s wait for the answer (TS 24.501 Table 10.2.1). Observed: `registration left without an answer after the SECURITY MODE COMPLETE`.
- Each FAIL carries the message "the AMF did not complete the registration on the REGISTRATION REQUEST in the SECURITY MODE COMPLETE's NAS message container. TS 24.501 5.4.2.4; TS 33.501 6.4.6 step 4. Likely cause: the AMF keeps IEs of the unprotected initial NAS message."
- **INFO** `references-alike`: references 2 and 3 got the same answer (a core that allows a default S-NSSAI whatever is requested), and the stimulus got it too: the two copies cannot be told apart, so the requirement is not exercised.
- **INFO** `refused-before-smc`: a Registration Reject before any SECURITY MODE COMMAND (`core refuses non-cleartext IEs in the initial message (Registration Reject, 5GMM cause #n) before the container is consulted`). No SECURITY MODE COMPLETE was sent, so the condition of 5.4.2.4 never arose. TS 24.501 4.4.6 binds the UE, and refusing a message that breaks it weakens no protection and drops no valid request. The references are not run.
- **INFO** `dropped-before-smc`: a release, or nothing within 10 s, before any SECURITY MODE COMMAND; as for the refusal.
- **WARN (security)** `accepted-without-smc`: the core registered the UE on the unprotected initial message with no SECURITY MODE COMMAND at all. No clause the check cites covers it; the core acted on non-cleartext IEs that had no protected copy.
- **BLOCKED:** an Authentication Reject for the run's subscriber, a stimulus attach that failed, reference 2 not registered, or reference 3 not answered (`blocked-registration`, owned by `NAS-INITIAL-REGISTRATION`); the NG Setup not completed (`blocked-ng-setup`).
- **ERROR:** the harness could not be set up or switch subscribers, a local socket failed, reference 3's Reject could not be read, or the per-test timeout expired.
- **SKIP:** no UE configured, or a target release older than Rel-15.
- **CRASH:** a fresh NG Setup is tried afterwards; a core that no longer completes it is CRASH.
- **State left:** after each accepted registration the UE is deregistered (switch-off); after a reject or an unanswered registration the association is closed with the UE not registered. The detail says which, per registration. It never includes key material.
- **Subscribers:** three, one per registration, taken from the check's own block of the subscriber pool, so no registration meets the context the core still holds of the one before. A run without a pool (a single SUPI), or with a block cut short at the pool's end, registers a subscriber again; the check then pauses 1 s before that registration, in which the core finishes releasing the UE context.
- **Limits:** budget up to 52 s: three registrations of up to 10 s, each with up to 5 s for the deregistration, and the two 1 s pauses of a run without a subscriber for each.
