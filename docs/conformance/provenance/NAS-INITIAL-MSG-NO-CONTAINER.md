# NAS-INITIAL-MSG-NO-CONTAINER

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/) and [https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/](https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/). Versions read (2026-11-10): TS 24.501 V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`); TS 33.501 V15.0.0 (`33501-f00.zip`), V15.20.0 (`33501-fk0.zip`), V16.20.0 (`33501-gk0.zip`), V17.16.0 (`33501-hg0.zip`), V18.12.0 (`33501-ic0.zip`), V19.7.0 (`33501-j70.zip`).

## TS 24.501 4.4.6 a) (Protection of initial NAS signalling messages), descriptive

> "If the UE does not have a valid 5G NAS security context, the UE sends a REGISTRATION REQUEST message including cleartext IEs only. After activating a 5G NAS security context resulting from a security mode control procedure: 1) if the UE needs to send non-cleartext IEs, the UE shall include the entire REGISTRATION REQUEST message (i.e. containing both cleartext IEs and non-cleartext IEs) in the NAS message container IE and shall include the NAS message container IE in the SECURITY MODE COMPLETE message; or 2) if the UE does not need to send non-cleartext IEs, the UE shall include the entire REGISTRATION REQUEST message (i.e. containing cleartext IEs only) in the NAS message container IE and shall include the NAS message container IE in the SECURITY MODE COMPLETE message."

- **Since:** Rel-15. V15.0.0 has no clause 4.4.6: it protects the initial NAS message with a hash the AMF returns in the SECURITY MODE COMMAND. The text quoted is in V15.7.0 and the same in V16.15.0, V17.18.1, V18.14.0 and V19.8.0.
- **Strength:** descriptive for this check. Both shalls bind the UE; the stimulus breaks them on purpose.

## TS 24.501 5.4.2.2 RINMR (NAS security mode control initiation by the network), descriptive, primary

V19.8.0:

> "If, during an ongoing registration procedure, the AMF is initiating a SECURITY MODE COMMAND (i.e. after receiving the REGISTRATION REQUEST message, but before sending a response to that message) and: a) the REGISTRATION REQUEST message does not successfully pass the integrity check at the AMF; or b) the AMF can not decipher the value part of the NAS message container IE in the REGISTRATION REQUEST message; the AMF shall include the Additional 5G security information IE with the RINMR bit set to "Retransmission of the initial NAS message requested" in the SECURITY MODE COMMAND message requesting the UE to send the entire REGISTRATION REQUEST message in the SECURITY MODE COMPLETE message as described in 3GPP TS 33.501 [24]."

- **Since:** Rel-15. V15.0.0 has the AMF "calculate the HASHAMF of the entire plain REGISTRATION REQUEST message" instead and has no RINMR bit. V15.7.0 has the paragraph with condition a) only; condition b) is in V16.15.0 and later.
- **Strength:** descriptive for this check. The shall covers a REGISTRATION REQUEST that failed the integrity check or whose container could not be deciphered. The stimulus's REGISTRATION REQUEST is unprotected and has no container, so neither condition holds. The paragraph is cited as the description of how an AMF asks for the complete initial NAS message.

## Also read, not cited

- **TS 24.501 5.4.2.4:** "If the SECURITY MODE COMPLETE message contains a NAS message container IE with a REGISTRATION REQUEST message, the AMF shall complete the ongoing registration procedure by considering [it] as the message that triggered the procedure." It is conditional on the container being there and says nothing of a SECURITY MODE COMPLETE without one.
- **TS 33.501 6.4.6 step 3 and step 4** (V15.20.0 to V19.7.0): the AMF "shall include in the Security Mode Command message a flag requesting the UE to send the complete initial NAS message" when the protected initial message failed its integrity check or could not be decrypted; "the NAS Security Mode Complete message shall include the complete initial NAS message in a NAS Container if either requested by the AMF or the UE sent the initial NAS message unprotected" (the UE's obligation).

## Why there is nothing to grade

No version read has a shall or a should for an AMF that receives a SECURITY MODE COMPLETE without the NAS message container from a UE that sent its initial message unprotected. The check is a characterisation.

## What the check does

One registration without a PDU session. The initial REGISTRATION REQUEST is the complete request, with a 5GMM capability and a Requested NSSAI the core does not serve (the run's SST with SD EEEEEE, or DDDDDD) in the clear. The SECURITY MODE COMPLETE has no NAS message container. The UE does not answer a second SECURITY MODE COMMAND: the registration is stopped there.

## Verdict

- **Class:** characterisation: no clause the check cites requires anything of the AMF here, so no outcome is PASS or FAIL. Needs the registration, owned by `NAS-INITIAL-REGISTRATION`.
- **Stimulus:** the initial message is the complete REGISTRATION REQUEST, its Requested NSSAI (an S-NSSAI the core does not serve) and 5GMM capability in the clear; the SECURITY MODE COMPLETE carries no NAS message container. The answer is awaited for 10 s.
- **INFO** `asked-again-rinmr`: `second SECURITY MODE COMMAND with RINMR set (the initial NAS message requested again)`, the request 5.4.2.2 describes for getting the complete message.
- **INFO** `second-smc`: `second SECURITY MODE COMMAND without RINMR`.
- **WARN (security)** `accepted`: `registration accepted without the NAS message container`, or `registration accepted on the unprotected initial message, without a SECURITY MODE COMMAND`. The core registered the UE with only the unprotected initial message to go on: it acted on unprotected input. The detail gives the allowed and rejected NSSAI.
- **WARN (security)** `rejected-on-unprotected-nssai`: `REGISTRATION REJECT, 5GMM cause #62, after the SECURITY MODE COMPLETE without the NAS message container`. Cause #62 answers a Requested NSSAI, and only the initial message carried one, in the clear: the core went on with the unprotected values.
- **INFO** `rejected`: a Reject with another cause, or one that cannot be read, after the SECURITY MODE COMPLETE. It acts on no unprotected value the check can name, and the UE broke 4.4.6.
- **INFO** `refused-before-smc`: `initial message refused before the SECURITY MODE COMMAND (Registration Reject, 5GMM cause #n)`; the missing container was not exercised. Refusing a message that breaks 4.4.6 weakens no protection and drops no valid request.
- **INFO** `dropped-before-smc`: `initial message released or left unanswered before a SECURITY MODE COMMAND`, as for the refusal; the absence is inferred within 10 s.
- **INFO** `released`: `UE CONTEXT RELEASE COMMAND after the SECURITY MODE COMPLETE without the NAS message container`.
- **INFO** `unanswered`: `no answer to the SECURITY MODE COMPLETE without the NAS message container`, the absence inferred within 10 s, a bound of the check inside T3510 (TS 24.501 Table 10.2.1). Neither a release nor silence drops a valid request: the UE left out the container 4.4.6 requires of it.
- **BLOCKED:** an Authentication Reject for the run's subscriber, or an attach that failed (`blocked-registration`, owned by `NAS-INITIAL-REGISTRATION`); the NG Setup not completed (`blocked-ng-setup`).
- **ERROR:** the harness could not be set up, a local socket failed, or the per-test timeout expired.
- **SKIP:** no UE configured, or a target release older than Rel-15.
- **CRASH:** a fresh NG Setup after the check does not complete: the core did not survive. Observed is `AMF unresponsive after the message (<what was observed>)`.
- **State left:** after an Accept the UE is deregistered (switch-off). In every other case the association is closed with the UE not registered; a core that had sent a second SECURITY MODE COMMAND is left to its own T3560. The detail says which.
