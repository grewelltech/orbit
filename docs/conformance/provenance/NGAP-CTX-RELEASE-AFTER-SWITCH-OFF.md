# NGAP-CTX-RELEASE-AFTER-SWITCH-OFF

Source: the 3GPP archive only:
- [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/): V15.0.0 (`38413-f00.zip`), V15.13.0 (`38413-fd0.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`), V19.4.0 (`38413-j40.zip`), as recorded in issue #146.
- [https://www.3gpp.org/ftp/Specs/archive/23_series/23.502/](https://www.3gpp.org/ftp/Specs/archive/23_series/23.502/): V15.0.0 (`23502-f00.zip`), V15.17.0 (`23502-fh0.zip`), V16.19.0 (`23502-gj0.zip`), V17.15.0 (`23502-hf0.zip`), V18.14.0 (`23502-ie0.zip`), V19.9.0 (`23502-j90.zip`), read for step 0 of issue #146 (2026-11-10).
- [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/): V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`), read for step 0 (2026-11-10).

## Result of step 0: no shall requires the release

The issue's candidate for the obligation was TS 24.501 5.5.2.2.2 or the TS 23.502 deregistration procedure. Neither carries one:
- TS 24.501 5.5.2.2.2 says only that a switch-off deregistration "is completed when the AMF receives the DEREGISTRATION REQUEST message". 5.5.2.2.3 has the AMF release the PDU sessions and enter 5GMM-DEREGISTERED. No sentence in 5.5.2.2 or in 5.3.1.3 (release of the N1 NAS signalling connection) has the AMF release the connection after a deregistration, and a search of each version read for an AMF or network shall on releasing the signalling connection after a de-registration found none.
- TS 23.502 4.2.2.3.2 step 8 has the release, in the indicative mood ("the AMF sends").
- TS 38.413 8.3.3.2 gives the procedure and the message content, not its trigger.

A check may FAIL a core only for a missed shall. So the one requirement the check asserts is on the command's content, and a core that sends no UE CONTEXT RELEASE COMMAND is graded by the WARN criteria (see Verdict).

## TS 38.413 8.3.3.2 (UE Context Release (AMF initiated), Successful Operation), shall, primary

> "The AMF initiates the procedure by sending the UE CONTEXT RELEASE COMMAND message to the NG-RAN node. The UE CONTEXT RELEASE COMMAND message shall contain both the AMF UE NGAP ID IE and the RAN UE NGAP ID IE if available, otherwise the message shall contain the AMF UE NGAP ID IE."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Changes:** the quoted sentences are unchanged from Rel-15 through Rel-19. Rel-15: 8.3.3.1 is extended (V15.13.0 against V15.0.0) to mention releasing the old connection when the UE establishes a new one, and IE-handling sentences are added. Rel-16: further IE-handling sentences are added.
- **Strength:** "initiates" is descriptive. The content sentence is a shall. The NG-RAN side is a shall too: "Upon reception of the UE CONTEXT RELEASE COMMAND message, the NG-RAN node shall release all related signalling and user data transport resources and reply with the UE CONTEXT RELEASE COMPLETE message." (unchanged V15.0.0 to V19.4.0); Orbit, as the gNB, answers with the COMPLETE.
- **Use in the check:** the UE has registered, so the AMF holds both IDs of the connection (the RAN UE NGAP ID from the INITIAL UE MESSAGE) and "if available" is met.
- **Difference from issue #146:** the issue asked for FAIL on no release, citing "TS 24.501 5.5.2.2.2 (to be pinned)". Step 0 pinned no shall, so that outcome is not a FAIL and the FAIL moved to the ID content.

## TS 23.502 4.2.2.3.2 step 8 (UE-initiated Deregistration), shall (read as a requirement)

V15.17.0 through V19.9.0:

> "8. [Conditional] AMF to AN: N2 UE Context Release Request (Cause)
> If the target access for Deregistration procedure is 3GPP access or both 3GPP access and non-3GPP access and there is N2 signalling connection to NG-RAN, the AMF sends N2 UE Release command to NG-RAN with Cause set to Deregistration to release N2 signalling connection. The details of this step are covered by steps 2 to 4 in the AN Release procedure, as described in clause 4.2.6."

- **Since:** Rel-15, V15.0.0 (`23502-f00.zip`).
- **Changes:** Rel-15: V15.0.0 reads "... non-3GPP access, and there is N2 signalling connection to NG-RAN, the AMF sends N2 UE Release command to RAN with Cause set to Deregistration ..."; "to NG-RAN" by V15.17.0 (the versions between were not read). Rel-16: the following sentence, for non-3GPP access, names "N3IWF/TNGF/W-AGF" instead of "N3IWF". The NG-RAN sentence is the same in V15.17.0, V16.19.0, V17.15.0, V18.14.0 and V19.9.0.
- **Step 7, the same clause, every version read:** "[Conditional] The AMF sends NAS message Deregistration Accept to UE depending on the Deregistration type i.e. if Deregistration type is switch-off, AMF does not send Deregistration Accept message."
- **Use in the check:** graded as the requirement it states. The step has no "shall", but it is the procedure's only network action after a switch-off (step 7 sends no DEREGISTRATION ACCEPT), its one condition (an N2 signalling connection to NG-RAN) holds after the registration, and it gives no alternative: the AMF releases the N2 signalling connection with Cause Deregistration. TS 24.501 5.5.2.2.2 completes the switch-off deregistration at the request and says nothing of the connection, and TS 24.501 5.3.1.3 has the network initiate the release of the N1 NAS signalling connection; neither contradicts step 8. A missing release, or one for another UE NGAP ID pair only, is FAIL on this step.

## TS 24.501 5.5.2.2.2 (UE-initiated de-registration procedure completion), descriptive

> "When the DEREGISTRATION REQUEST message is received by the AMF, the AMF shall send a DEREGISTRATION ACCEPT message to the UE, if the De-registration type IE does not indicate "switch off". Otherwise, the procedure is completed when the AMF receives the DEREGISTRATION REQUEST message."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** the quoted sentences are the same in every version read, V15.0.0 through V19.8.0. Rel-15: V15.0.0 titles the clause "UE-initiated de-registration procedure completion - common procedure"; V15.7.0 onward "UE-initiated de-registration procedure completion".
- **Use in the check:** the second sentence is why the stimulus draws no DEREGISTRATION ACCEPT and the check waits for none. The shall of the first sentence is asserted by `NAS-DEREG-ACCEPT-NOT-SWITCH-OFF`, not here.
- **Stimulus:** the UE registers without a PDU session, then sends a DEREGISTRATION REQUEST under its NAS security context with the switch-off bit 1, and keeps the association open for the answer.

## Verdict

- **Class:** conformance: 8.3.3.2 on the command's content, and TS 23.502 4.2.2.3.2 step 8, read as a requirement, on the release itself. The check owns the procedure "release after switch-off": a check that needs it (`NAS-REGISTRATION-AFTER-RELEASE`) is BLOCKED naming this check when it does not complete.
- **Stimulus:** as above; the UE CONTEXT RELEASE COMMAND is awaited for 5 s and answered with UE CONTEXT RELEASE COMPLETE.
- **PASS** `both-ids`: a UE CONTEXT RELEASE COMMAND naming the connection's AMF UE NGAP ID and RAN UE NGAP ID.
- **FAIL** (8.3.3.2): `amf-id-only`, a command naming the UE by its AMF UE NGAP ID alone; `no-ue-ngap-id`, one carrying no UE NGAP ID. The detail carries `UE CONTEXT RELEASE COMMAND after switch-off deregistration names the UE by AMF UE NGAP ID <n> alone, with both UE NGAP IDs available. TS 38.413 8.3.3.2. Likely cause: AMF builds the release without the UE NGAP ID pair.`
- **FAIL** (TS 23.502 4.2.2.3.2 step 8) `no-release`: no command within 5 s, a bound of the check (neither TS 23.502 nor TS 38.413 sets a timer for the release); the absence is inferred within it. `release-for-other-ids`: a command for another UE NGAP ID or pair, and none for the connection. Both report the release after switch-off as not completed. The detail carries `no UE CONTEXT RELEASE COMMAND after switch-off deregistration. TS 23.502 4.2.2.3.2 step 8: with an N2 signalling connection to NG-RAN, the AMF sends N2 UE Release command with Cause set to Deregistration to release it. Likely cause: AMF leaves the N2 UE context.`
- **CRASH:** a fresh NG Setup after the switch-off that no longer completes.
- **BLOCKED** `blocked-registration`: the core did not complete the UE's registration (`NAS-INITIAL-REGISTRATION` owns whether it completes); `blocked-ng-setup`: nor the NG Setup before it.
- **ERROR:** the harness could not be set up; the request could not be sent (no 5G-GUTI assigned, which ORBIT needs to build it); a local fault during the registration; or the per-test timeout expired during the registration or the release wait.
- **SKIP:** no UE configured (`skip-no-ue`); a target release older than Rel-15.
- **Limits:** the release's cause is not asserted. The switch-off deregistration is the stimulus, not a procedure this check needs, so a core that ignores it is graded here (FAIL) and not BLOCKED.
