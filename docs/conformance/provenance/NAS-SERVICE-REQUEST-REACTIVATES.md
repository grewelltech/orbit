# NAS-SERVICE-REQUEST-REACTIVATES

Source: the 3GPP archive only, [24_series/24.501](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/) and [23_series/23.502](https://www.3gpp.org/ftp/Specs/archive/23_series/23.502/). Versions read: TS 24.501 V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`); TS 23.502 V15.0.0 (`23502-f00.zip`), V15.17.0 (`23502-fh0.zip`), V16.19.0 (`23502-gj0.zip`), V17.15.0 (`23502-hf0.zip`), V18.14.0 (`23502-ie0.zip`), V19.9.0 (`23502-j90.zip`). They were read as text extracted from those archive files, not in the documents' own layout.

The issue for this check named TS 24.501 5.6.1 as an unverified candidate. Read in the archive, the requirement on the network is in 5.6.1.4 (Rel-15), which Rel-16 splits into 5.6.1.4.1 and 5.6.1.4.2; the check's UE does not use control plane CIoT 5GS optimization, so 5.6.1.4.1 applies. The check's FAIL message names that clause in place of the issue's "5.6.1 (to be pinned)".

Not verified: TS 38.413 was not read for this check, so the NGAP messages that carry the tunnel setup (INITIAL CONTEXT SETUP REQUEST, PDU SESSION RESOURCE SETUP REQUEST) are not cited. The intermediate versions of each release were not read: a change is dated "by" the first version read that has it.

## TS 24.501 5.6.1.4.1 (5.6.1.4 in Rel-15) Uplink data status a), shall, primary

> "If the Uplink data status IE is included in the SERVICE REQUEST message and the UE is: a) not in NB-N1 mode; or b) in NB-N1 mode and the UE does not indicate a request to have user-plane resources established for a number of PDU sessions that exceeds the UE's maximum number of supported user-plane resources; the AMF shall: a) indicate the SMF to re-establish the user-plane resources for the corresponding PDU sessions; b) include the PDU session reactivation result IE in the SERVICE ACCEPT message to indicate the user-plane resources re-establishment result of the PDU sessions for which the UE requested to re-establish the user-plane resources; and c) ..."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`), in 5.6.1.4 ("Service request procedure accepted by the network") as "If the Uplink data status IE is included in the SERVICE REQUEST message, the AMF shall: a) indicate the SMF to re-establish the user-plane resources for the corresponding PDU sessions; b) include the PDU session reactivation result IE in the SERVICE ACCEPT message to indicate the user-plane resources reactivation result of the PDU sessions the UE requested to re-establish ; ...".
- **Changes:** Rel-15: item b) reads "re-establishment result of the PDU sessions for which the UE requested to re-establish the user-plane resources" by V15.7.0; item a) is unchanged. Rel-16: by V16.15.0 the clause is split, the text is in 5.6.1.4.1 ("UE is not using 5GS services with control plane CIoT 5GS optimization"), and the NB-N1 condition is added before "the AMF shall:". Rel-17: unchanged from V16.15.0. Rel-18, Rel-19: items a) and b) unchanged; item c) gains "(see clause 6.2.6)".
- **Scope:** the UE is not in NB-N1 mode, so the shall binds in every release. The request names one PDU session, the one the UE established, in its Uplink data status.
- **What is asserted:** item a). The AMF's indication to the SMF is not visible to the check; its result is: the session's N3 tunnel set up again on N2, and downlink data arriving on the gNB's new tunnel. Item b) is also a shall, and is recorded only: the detail says whether the Service Accept carried the PDU session reactivation result IE and what it reported. The issue's verdicts do not grade it.
- **Verdicts:** see "Verdict" below.

## TS 24.501 5.6.1.5 first sentence (Service request procedure not accepted by the network), shall

> "If the service request cannot be accepted, the network shall return a SERVICE REJECT message to the UE including an appropriate 5GMM cause value."

- **Since:** Rel-15, V15.0.0, as "... including an appropriate 5GMM cause value and stop timer T3517."
- **Changes:** Rel-15: V15.7.0 as V15.0.0. Rel-16: "and stop timer T3517" is absent by V16.15.0. Rel-17 to Rel-19: unchanged from V16.15.0.
- **Use:** with 5.6.1.4.1 it leaves the network two answers, so a Service Request that gets neither within 15 s is a FAIL, not an inconclusive run. The UE's own wait is T3517; its value was not read for this check and the 15 s is the check's.
- **A Service Reject is WARN, not FAIL.** The clause lets a network reject a request it cannot accept, and the reject is the answer it requires then, so no requirement is violated. The check gives the network no reason to reject (the UE is registered, its security context is the one the registration set up, the request is integrity protected, and the session was active until the release), so a reject means the core cannot complete the procedure: WARN for availability. The detail records the 5GMM cause.

## TS 23.502 4.2.3.2 steps 12 and 18a (UE Triggered Service Request), descriptive

> "12. AMF to (R)AN: N2 Request (N2 SM information received from SMF, security context, Mobility Restriction List, UE-AMBR, ..., MM NAS Service Accept, ...)."

> "18a. [Conditional] SMF to UPF (PSA): N4 Session Modification Request (AN Tunnel Info, List of rejected QoS Flows). If a User Plane is to be setup or modified and after the modification there is no I-UPF, the SMF initiates a N4 Session Modification procedure to UPF (PSA) and provides AN Tunnel Info. The Downlink Data from the UPF (PSA) can now be forwarded to NG-RAN and UE."

- **Since:** Rel-15, V15.0.0 (`23502-f00.zip`), step 18a ending "can now be forwarded to RAN and UE" and step 12 as "N2 Request (N2 SM information received from SMF, security context, AMF Signalling Connection ID, Handover Restriction List, Subscribed UE-AMBR, MM NAS Service Accept, list of recommended cells / TAs / NG-RAN node identifiers)".
- **Changes:** Rel-15: "NG-RAN and UE" in step 18a by V15.17.0; that text is the same in V16.19.0, V17.15.0, V18.14.0 and V19.9.0. Step 12's list of the N2 Request's content grows in every release read; "N2 SM information received from SMF" and "MM NAS Service Accept" are in all of them.
- **Strength:** descriptive. No "shall" in either step.
- **Use:** step 12 is why the tunnel's setup is expected on N2 with or after the Service Accept, and step 18a why downlink on the gNB's new tunnel is the observable: it arrives only once the SMF has given the UPF the AN Tunnel Info the gNB answered with. Uplink is not that evidence; the check does not assert it.

## Tolerances

- The downlink is a responder's flow to the UE's address, sent once while the UE is connected and once after the Service Accept. The first is a control on the trigger: without it on the UE's tunnel the run is an ERROR.
- The release follows once no downlink has arrived for 500 ms, so the first flow does not reach the core while the UE is idle and the UE is not paged.
- The Service Request follows the UE CONTEXT RELEASE COMPLETE by one second, so the core has finished its own release.
- The tunnel's setup may come in the INITIAL CONTEXT SETUP REQUEST that carries the Service Accept or in a PDU SESSION RESOURCE SETUP REQUEST up to 5 s after it; the detail records which.
- The AN release uses cause Radio Network "release due to NG-RAN generated reason", not "user inactivity".

## Verdict

- **Class:** conformance, two shalls (5.6.1.4.1 item a), 5.6.1.5). The check owns the procedure "Service Request": a check whose Service Request the core does not accept reports BLOCKED and names this check.
- **Stimulus:** a UE with a PDU session, released to CM-IDLE by an AN release, sends a SERVICE REQUEST with the session in its Uplink data status; the answer and the tunnel's setup are awaited for 15 s, and downlink on the new tunnel for 5 s.
- **PASS** `reactivated` (5.6.1.4.1): Service Accept, the session's N3 tunnel set up again and a downlink G-PDU on it.
- **FAIL** `unanswered` (5.6.1.4.1, 5.6.1.5): neither Accept nor Reject within 15 s, a bound of the check. The two clauses leave the network two answers, so neither violates one of them.
- **FAIL** `association-lost` (5.6.1.4.1, 5.6.1.5): the association went before either answer.
- **FAIL** `released-after-accept` (5.6.1.4.1): an Accept followed by a release of the UE context, with the user plane not re-established.
- **FAIL** `no-user-plane` (5.6.1.4.1): an Accept without the session's tunnel within 5 s of it.
- **FAIL** `no-downlink` (5.6.1.4.1): a tunnel with no downlink on it within 5 s, where downlink reached the UE within that bound before the release.
- **WARN (availability)** `rejected`: a Service Reject; see 5.6.1.5 above.
- **Not completed:** `unanswered`, `association-lost`, `rejected` and `released-after-accept` report the Service Request as not completed, so the BLOCKED results of dependent checks in the same run are counted under this one.
- **BLOCKED** (registration, PDU session establishment): the UE or its session was not set up. **BLOCKED** (AN release): the core did not release the UE to CM-IDLE (no UE CONTEXT RELEASE COMMAND, or no 5G-GUTI to page it by).
- **CRASH:** after no answer or a lost association, a fresh NG Setup from another gNB ID does not complete.
- **ERROR:** no downlink reaching the UE before the release, that downlink not stopping, the trigger failing, the Service Request not sent, or the per-test timeout ending a wait.
- **SKIP:** no UE or no N6 downlink trigger configured, or a target release older than Rel-15.
