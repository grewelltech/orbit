# NAS-SMC-NO-NIA0-REPLAYED-CAPS

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/](https://www.3gpp.org/ftp/Specs/archive/33_series/33.501/). Versions read (orbit-architect, 2026-10-12 and 2026-10-18; issue #136): V15.0.0 (`33501-f00.zip`) to V15.5.0 (`33501-f50.zip`), including V15.1.0 (`33501-f10.zip`); V15.20.0 (`33501-fk0.zip`); V16.20.0 (`33501-gk0.zip`); V17.16.0 (`33501-hg0.zip`); V18.12.0 (`33501-ic0.zip`); and V19.7.0 (`33501-j70.zip`).

## TS 33.501 5.5.2 (Signalling data integrity), shall

> "All NAS signalling messages except those explicitly listed in TS 24.501 [35] as exceptions shall be integrity-protected with an algorithm different to NIA-0 except for emergency calls."

- **Since:** Rel-15, V15.1.0 (`33501-f10.zip`). V15.0.0 has only "...shall be integrity-protected."
- **Changes:** Rel-15, the NIA-0 exclusion is added in V15.1.0. The wording is unchanged from V15.1.0 through V19.7.0.
- **Use in the check:** for a normal (non-emergency) registration, a Security Mode Command that selects NIA0 is FAIL, and so is a Registration Accept with no Security Mode Command, which leaves NAS without integrity protection. This holds for all three UEs.

## TS 33.501 6.7.2 step 1b (NAS security mode command procedure), shall

> "The NAS Security Mode Command message shall contain: the replayed UE security capabilities, the selected NAS algorithms, and the ngKSI for identifying the KAMF."

- **Since:** Rel-15, V15.0.0 (`33501-f00.zip`). The replayed-capabilities requirement is present in every version read.
- **Changes:** Rel-15. In V15.0.0 the step is unnumbered, reads "selected NAS algorithm", and adds the N2-handover input container. It is step 1b from V15.1.0, and has the quoted wording from V15.2.0. It is unchanged from there through V19.7.0.
- **Use in the check:** "replayed" means the capabilities the UE sent. So replayed capabilities that differ byte for byte from those the UE sent are FAIL. Step 2a, where the UE checks the match, is context only.

## TS 33.501 6.7.1.1 (NAS algorithm selection), descriptive

> "The AMF shall select the NAS algorithm which have the highest priority according to the ordered lists."

- **Since:** Rel-15, V15.0.0.
- **Changes:** none. The sentence is unchanged from V15.0.0 through V19.7.0.
- **Use in the check:** the ordered lists are operator configuration, so the check cannot assert which algorithm wins. It records the selected 128-NIA and NEA.
- **Not scored:** before Rel-19 no shall says the selected algorithm is one the UE supports. V19.7.0 adds that only in 6.7.1.2, which covers AMF change. A selected algorithm the UE did not offer is therefore recorded, not scored. NIA0 is the exception, covered by 5.5.2.

## TS 33.501 5.2.3 (Requirements on the UE for integrity), descriptive

> "NIA0 is only allowed for unauthenticated emergency session as specified in clause 10.2.2."

- **Since:** Rel-15, V15.0.0.
- **Changes:** the quoted NIA0 sentence is unchanged from V15.0.0 through V19.7.0. Rel-16 adds a UE data-rate sentence to the clause, which is not cited.
- **Use in the check:** context for the second stimulus, a UE offering only NIA0 and NIA3, and the third below. That UE does not conform, since UEs shall implement 128-NIA1 and 128-NIA2. The requirement under test is on the AMF, which may select NIA3, reject or release, but never select NIA0 for a normal registration.
- **Third stimulus (issue #180):** a UE offering only NIA0 (with 5G-EA0/1/2). It is doubly non-conforming, since UEs shall implement 128-NIA1 and 128-NIA2. As for the NIA0/NIA3 UE, the requirement under test is on the AMF, not the UE. With no non-null integrity algorithm in common, the expected answer is a reject or a release. For any UE, a Security Mode Command selecting NIA0, or no Security Mode Command, is FAIL (TS 33.501 5.5.2, 6.7.2 step 1b). A non-null algorithm the UE did not offer is PASS, recorded, not scored: no shall requires the selected algorithm to be one the UE offered at initial registration. 5.11.2 b) says only 'dependent on', and 6.7.1.2 (from V19.4.0) covers AMF change. This UE runs last.
- **Not-offered algorithms:** 5.11.2 b) requires the serving network to select the algorithms 'dependent on' the UE security capabilities, but does not require the selected algorithm to be one the UE offered; only 6.7.1.2 (AMF change, added in V19.4.0 by CR 2172) does. An algorithm the UE did not offer is therefore recorded, not scored.

## Verdicts

Each UE is judged separately, and the check's verdict is the worst of the three (FAIL, then ERROR, then PASS). A UE that was not run is SKIP and does not affect the verdict. The second and third UEs run only when the normal UE's result is not ERROR. When the NIA0/NIA3 UE and the NIA0-only UE fail the same way, the detail says so.

- **A Registration Accept with no Security Mode Command:** FAIL for any UE. The detail says "no Security Mode Command; registration proceeded without NAS security" when the Accept is plain (security header type 0), and does not claim NIA0 was selected.
- **A Security Mode Command selecting NIA0:** FAIL for any UE.
- **Emergency exception:** the detail of every NIA0 or no-command FAIL says: "The check never requests an emergency registration, so the emergency exception in 5.5.2 does not apply." The UEs always send an initial registration.
- **A FAIL of the NIA0-only UE:** the detail adds: "Which of the two FAILs a core gives can depend on its configured NAS algorithm order (for example, a core may skip the Security Mode Command when only NEA0 is configured, and send one selecting NIA0 otherwise). Both are FAIL."
- **Cleanup after a plain Registration Accept:** the UE has no NAS security context, so it sends a switch-off Deregistration Request in the clear. TS 24.501 4.4.4.3 allows this, unchanged from V15.0.0 to V20.1.0: its list of messages processed before secure exchange is established includes "f) DEREGISTRATION REQUEST", and NOTE 2 reads "The DEREGISTRATION REQUEST message can be sent by the UE without integrity protection, e.g. if ... there is no valid 5G NAS security context available, ...". No Security Mode Command ran, so secure exchange was never established. The request names itself by the 5G-GUTI the Accept assigned, else by its SUCI. If no UE Context Release Command arrives within 2 s, the gNB sends a UE Context Release Request and waits 2 s again. A command is answered with UE Context Release Complete. The detail records each step, including a release that never came.
- The liveness probe after the three registrations is unchanged.
