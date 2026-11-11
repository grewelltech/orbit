# NAS-INITIAL-MSG-NONCLEARTEXT-ACCEPTED

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/) and [https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/](https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/). Versions read (2026-11-10): TS 24.501 V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`); TS 33.501 V15.0.0 (`33501-f00.zip`), V15.20.0 (`33501-fk0.zip`), V16.20.0 (`33501-gk0.zip`), V17.16.0 (`33501-hg0.zip`), V18.12.0 (`33501-ic0.zip`), V19.7.0 (`33501-j70.zip`).

## TS 24.501 4.4.6 a) (Protection of initial NAS signalling messages), descriptive

> "If the UE does not have a valid 5G NAS security context, the UE sends a REGISTRATION REQUEST message including cleartext IEs only."

- **Since:** Rel-15. V15.0.0 has no clause 4.4.6 and no cleartext IEs (it protects the initial NAS message with a hash); the sentence is in V15.7.0 and the same through V19.8.0. The list of cleartext IEs that follows is the same in V15.7.0 and V16.15.0 and grows by "NID" and "PLMN with disaster condition" in V17.18.1 (the latter "UE determined PLMN with disaster condition" in V18.14.0 and V19.8.0); the 5GMM capability and the Requested NSSAI are in it in no version read.
- **Strength:** descriptive for this check. The sentence is about the UE (TS 33.501 6.4.6 step 1, V15.20.0 onward: "If the UE has no NAS security context, the initial NAS message shall only contain the cleartext IEs"). The stimulus breaks it on purpose.

## TS 24.501 5.4.2.4 (NAS security mode control completion by the network), descriptive, primary

> "If the SECURITY MODE COMPLETE message contains a NAS message container IE with a REGISTRATION REQUEST message, the AMF shall complete the ongoing registration procedure by considering the REGISTRATION REQUEST message contained in the NAS message container IE as the message that triggered the procedure."

- **Since:** Rel-15. V15.0.0 names the IE "Replayed NAS message container"; "NAS message container" from V15.7.0 through V19.8.0.
- **Strength:** descriptive for this check. The shall is about which copy the AMF uses. Here the two copies are the same request, so the check cannot test it (`NAS-INITIAL-MSG-CONTAINER-WINS` does). The sentence is cited as the description of the registration going through on the container's request.

## Why there is nothing to grade

No version read says what an AMF does with an initial REGISTRATION REQUEST that has non-cleartext IEs in the clear: accepting it and refusing it are both open. The check is a characterisation, and its value is the comparison between cores.

## What the check does

One registration without a PDU session. The initial REGISTRATION REQUEST is the complete request, with the 5GMM capability and a Requested NSSAI the core serves (the run's own S-NSSAI) in the clear. The SECURITY MODE COMPLETE's NAS message container carries the same request, byte for byte.

## Verdict

- **Class:** characterisation: no clause the check cites requires anything of the AMF here, so no outcome is PASS or FAIL. Needs the registration, owned by `NAS-INITIAL-REGISTRATION`.
- **Stimulus:** the complete REGISTRATION REQUEST, its Requested NSSAI (an S-NSSAI the core serves) and 5GMM capability included, as the initial message, and the same request in the SECURITY MODE COMPLETE's NAS message container. The answer is awaited for 10 s.
- **INFO** `accepted`: `registration accepted`, on the request in the container, as 5.4.2.4 describes. The detail gives the allowed and rejected NSSAI of the Accept.
- **WARN (security)** `accepted-without-smc`: `registration accepted on the unprotected initial message, without a SECURITY MODE COMMAND`: the core registered the UE with no protected copy of a message whose non-cleartext IEs came in the clear.
- **INFO** `refused-before-smc`: `Registration Reject, 5GMM cause #n, before the SECURITY MODE COMMAND`: the core refused the initial message itself. TS 24.501 4.4.6 a) binds the UE, and refusing a message that breaks it weakens no protection and drops no valid request.
- **INFO** `rejected`: `Registration Reject, 5GMM cause #n, after the SECURITY MODE COMPLETE`, or one that cannot be read; as for the refusal.
- **INFO** `released`: `UE CONTEXT RELEASE COMMAND, no Registration Accept or Reject`.
- **INFO** `unanswered`: `no Registration Accept or Reject`, the absence inferred within 10 s, a bound of the check inside T3510 (TS 24.501 Table 10.2.1).
- **BLOCKED:** an Authentication Reject for the run's subscriber, or an attach that failed (`blocked-registration`, owned by `NAS-INITIAL-REGISTRATION`); the NG Setup not completed (`blocked-ng-setup`).
- **ERROR:** the harness could not be set up, a local socket failed, or the per-test timeout expired.
- **SKIP:** no UE configured, or a target release older than Rel-15.
- **CRASH:** a fresh NG Setup after the check does not complete: the core did not survive. Observed is `AMF unresponsive after the message (<what was observed>)`.
- **State left:** after an Accept the UE is deregistered (switch-off); otherwise the association is closed with the UE not registered. The detail says which.
