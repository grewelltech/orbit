# NAS-PDU-RELEASE-UE-REQUESTED

Source: the 3GPP archive only:
- [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/): V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`), read for step 0 of issue #147 (2026-11-10). Versions between these were not read, so a change "by V15.7.0" or "in Rel-17" is not pinned to its first version.
- [https://www.3gpp.org/ftp/Specs/archive/23_series/23.502/](https://www.3gpp.org/ftp/Specs/archive/23_series/23.502/): V15.0.0 (`23502-f00.zip`), V15.4.1 (`23502-f41.zip`), V15.5.0 (`23502-f50.zip`), V15.17.0 (`23502-fh0.zip`), V16.19.0 (`23502-gj0.zip`), V17.15.0 (`23502-hf0.zip`), V18.14.0 (`23502-ie0.zip`), V19.9.0 (`23502-j90.zip`), read for step 0 (2026-11-10); V15.4.0 (`23502-f40.zip`) and V15.7.0 (`23502-f70.zip`) were searched for the step 3 sentence only.
- [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/): V15.0.0 (`38413-f00.zip`), V15.13.0 (`38413-fd0.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`), V19.4.0 (`38413-j40.zip`), as recorded in issue #147.

The issue's candidate, "TS 24.501 6.4.3", is pinned to 6.4.3.3 and 6.3.3.2 below. The requirement on the NGAP command and on the downlink is in TS 23.502 4.3.4.2, which the issue did not name.

## TS 24.501 6.4.3.3 (UE-requested PDU session release procedure accepted by the network), shall

> "Upon receipt of a PDU SESSION RELEASE REQUEST message and a PDU session ID, if the SMF accepts the request to release the PDU session, and shall perform the network-requested PDU session release procedure as specified in subclause 6.3.3."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`). The stray "and" is the specification's, in every version read.
- **Changes:** Rel-18: "subclause 6.3.3" becomes "clause 6.3.3". Otherwise the sentence is the same from V15.0.0 through V19.8.0.
- **The alternative, 6.4.3.4:** "Upon receipt of a PDU SESSION RELEASE REQUEST message, if the SMF does not accept the request to release the PDU session, the SMF shall create a PDU SESSION RELEASE REJECT message." (V15.0.0 reads "an PDU SESSION RELEASE REJECT"). The SMF answers one way or the other, so a request that draws neither is a failure, and a REJECT leaves no release to judge.

## TS 24.501 6.3.3.2 (Network-requested PDU session release procedure initiation), shall

> "In order to initiate the network-requested PDU session release procedure, the SMF shall create a PDU SESSION RELEASE COMMAND message."

> "If the network-requested PDU session release procedure is triggered by a UE-requested PDU session release procedure, the SMF shall set the PTI IE of the PDU SESSION RELEASE COMMAND message to the PTI of the PDU SESSION RELEASE REQUEST message received as part of the UE-requested PDU session release procedure."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** the first sentence is the same in every version read. Rel-15: the next sentence names the "SM cause IE" in V15.0.0 and the "5GSM cause IE" by V15.7.0, and the list of typical causes grows. Rel-17 (V17.18.1): the PTI sentence ends "... and shall not include the Access type IE in the PDU SESSION RELEASE COMMAND." Rel-18 adds Alternative S-NSSAI sentences.
- **Use in the check:** the PDU SESSION RELEASE COMMAND for the session is read through the UE's NAS security context, whether it is the NAS-PDU of the NGAP command or comes in a DOWNLINK NAS TRANSPORT. The UE answers it with a PDU SESSION RELEASE COMPLETE. The PTI (the request uses 1) and the 5GSM cause are recorded; a PTI that is not the request's is noted, not scored.

## TS 23.502 4.3.4.2 step 2a (UE or network requested PDU Session Release for Non-Roaming and Roaming with Local Breakout), shall

> "2a. The SMF sends an N4 Session Release Request (N4 Session ID) message to the UPF(s) of the PDU Session. The UPF(s) shall drop any remaining packets of the PDU Session and release all tunnel resource and contexts associated with the N4 Session."

- **Since:** Rel-15, V15.0.0 (`23502-f00.zip`).
- **Changes:** none: word-for-word the same in V15.0.0, V15.17.0, V16.19.0, V17.15.0, V18.14.0 and V19.9.0.
- **Use in the check:** step 2a precedes the command to the UE, so once the UE has both commands the UPF has dropped the session. The check allows downlink already in flight for 1 s after the later command, then watches the released downlink tunnel (the session's TEID at the gNB's N3 socket) for 3 s more. A G-PDU in that time is FAIL.
- **Probe:** with `--traffic`, the echo flow keeps sending uplink on the released tunnel, which a UPF that kept the session would answer. Without it the tunnel is only watched, and the detail says so. The detail gives the G-PDU counts before the request, in the grace and after it.

## TS 23.502 4.3.4.2 step 3 (the same procedure), shall

From V15.5.0:

> "If the User Plane connection of the PDU Session is activated, the message sent by the SMF to the AMF shall include N2 SM Resource Release request. If the User Plane connection of the PDU Session is not activated, the message sent by the SMF to the AMF shall not include N2 SM Resource Release request."

- **Since:** Rel-15. The shall first appears in V15.5.0 (`23502-f50.zip`); V15.4.1 (`23502-f41.zip`) and earlier only list the N2 SM Resource Release request in step 3a's message. Located by bisection: absent from V15.0.0, V15.4.0 and V15.4.1, present in V15.5.0, V15.7.0 and V15.17.0; the other Rel-15 versions were not searched.
- **Changes:** the two sentences are the same in V15.5.0, V15.17.0, V16.19.0, V17.15.0, V18.14.0 and V19.9.0. Rel-16 adds, in step 4, that a Control Plane CIoT 5GS Optimisation session carries no N2 release.
- **Use in the check:** the session was established with its N3 tunnel moments before, and the UE is in CM-CONNECTED, so the user plane connection is activated and the N2 release is due. It reaches the gNB as a PDU SESSION RESOURCE RELEASE COMMAND listing the session, which Orbit answers with a PDU SESSION RESOURCE RELEASE RESPONSE.
- **Limit:** the shall binds the SMF. Step 4 has the AMF pass the request on in the indicative ("the AMF transfers the SM information"). A missing NGAP command FAILs either way; the check cannot tell which function dropped it.

## TS 38.413 8.2.2.2 (PDU Session Resource Release, Successful Operation), descriptive

> "The AMF initiates the procedure by sending a PDU SESSION RESOURCE RELEASE COMMAND message. The PDU SESSION RESOURCE RELEASE COMMAND message shall contain the information required by the NG-RAN node to release at least one PDU session resource, and include each PDU session resource to release in the PDU Session Resource to Release List IE."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`) for the first sentence.
- **Changes:** Rel-15: in V15.0.0 the second sentence ends "... to release at least one PDU session in the PDU Session Resource to Release List IE."; it reaches the wording above during Rel-15. The NAS-PDU sentence becomes a paragraph of its own in V15.1.0, and "over Uu and over NG, if any" arrives in V15.4.0. Rel-16 and Rel-17: unchanged. Rel-18: a Downlink TL Container sentence is added. The quoted sentences are unchanged from Rel-16 through Rel-19.
- **Strength:** descriptive for the AMF ("initiates"); the shall is on the message content. The check reads the PDU Session Resource to Release List IE to find the session.
- **The NAS-PDU is optional** in 9.2.1.3 in every version; 8.2.2.2 says only "If a NAS-PDU IE is contained in the PDU SESSION RESOURCE RELEASE COMMAND message, the NG-RAN node shall pass it to the UE." (V15.1.0 on). So the check does not require the NAS command to ride in the NGAP command.

## TS 24.501 Table 10.3.1 T3582 (Timers of 5GS session management, UE side), descriptive

> T3582: 16s. Cause of start: "Transmission of PDU SESSION RELEASE REQUEST message". Normal stop: "PDU SESSION RELEASE COMMAND message with the same PTI is received or PDU SESSION RELEASE REJECT message received". On expiry: "Retransmission of PDU SESSION RELEASE REQUEST message".

- **Since:** Rel-15. The value is "TBD" in V15.0.0 (`24501-f00.zip`) and 16s by V15.7.0.
- **Changes:** Rel-16 adds "In WB-N1/CE mode, 24s". Rel-17 onward adds "For access via a satellite NG-RAN cell, 21s". The normal value is 16s from V15.7.0 through V19.8.0.
- **Use in the check:** descriptive only. It is the UE's timer, not a bound on the network. The check waits one T3582 at its normal value for the first answer, then 2 s for the other command, and sends no retransmission.

## Verdicts

- Both commands, and no G-PDU on the released tunnel after the 1 s grace: PASS.
- No answer within 16 s; the NAS command without the NGAP command within 2 s of it, or the reverse; or a G-PDU on the released tunnel after the grace: FAIL (`PDU session not released on UE request. TS 24.501 6.4.3.3, 6.3.3.2; TS 23.502 4.3.4.2; TS 38.413 8.2.2.2. Likely cause: SMF release path.`, followed by what was seen).
- A PDU SESSION RELEASE REJECT: SKIP (6.4.3.4).
- No UE configured: SKIP. UE setup that fails, a downlink NAS PDU that does not decode with no command seen, or the check's time running out: ERROR.
- A core that then stops completing NG Setup: the crash FAIL.

The UE is deregistered (switch-off) at the end on every path that registered it.
