# NGAP-PAGING-ON-DL-DATA

Source: the 3GPP archive only, [24_series/24.501](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/), [23_series/23.502](https://www.3gpp.org/ftp/Specs/archive/23_series/23.502/) and [38_series/38.413](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: TS 24.501 V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`); TS 23.502 V15.0.0 (`23502-f00.zip`), V15.17.0 (`23502-fh0.zip`), V16.19.0 (`23502-gj0.zip`), V17.15.0 (`23502-hf0.zip`), V18.14.0 (`23502-ie0.zip`), V19.9.0 (`23502-j90.zip`); TS 38.413 V15.0.0 (`38413-f00.zip`), V15.1.0 (`38413-f10.zip`), V15.13.0 (`38413-fd0.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`), V19.4.0 (`38413-j40.zip`).

The issue for this check named TS 23.502 4.2.3.3 as an unverified candidate for the trigger. Read in the archive, that step is descriptive ("the AMF sends a Paging message"), so it cannot carry a FAIL. The requirement is in TS 24.501 5.6.2.2.1, which the same reading found.

## TS 24.501 5.6.2.2.1 (Paging for 5GS services through 3GPP access, General), shall, primary

> "The network shall initiate the paging procedure for 5GS services when NAS signalling messages or user data is pending to be sent to the UE in 5GMM-IDLE mode over 3GPP access (see example in figure 5.6.2.2.1.1) and there is no paging restriction applied in the network for that paging."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`), as "The network shall initiate the paging procedure for 5GS services when NAS signalling messages or user data is pending to be sent to the UE in 5GMM-IDLE mode (see example in figure 5.6.2.2.1)."
- **Changes:** Rel-15: "over 3GPP access" is added and the figure renumbered by V15.7.0. Rel-16: unchanged from V15.7.0. Rel-17: "and there is no paging restriction applied in the network for that paging" is added by V17.18.1. Rel-18, Rel-19: unchanged from Rel-17.
- **Scope:** the UE requests no paging restriction (a Rel-17 multi-USIM feature) and no MICO mode, so the shall binds. A MICO indication in the Registration Accept that the UE did not ask for is recorded in the detail.
- **How the paging starts:** the next sentence of the clause, "To initiate the procedure the 5GMM entity in the AMF requests the lower layer to start paging and shall start timer T3513", makes the NGAP PAGING the observable. T3513 and its retransmissions are not asserted.
- **Verdicts:** see "Verdict" below.

## TS 23.502 4.2.3.3 step 4b (Network Triggered Service Request), descriptive

> "If the UE is in RM-REGISTERED state and CM-IDLE and reachable in 3GPP access, the AMF sends a Paging message (NAS ID for paging, Registration Area list, Paging DRX length, Paging Priority, access associated to the PDU Session, Enhanced Coverage Restricted information, WUS Assistance Information) to (R)AN node(s) belonging to the Registration Area(s) in which the UE is registered, then the NG-RAN node pages the UE, including the access associated to the PDU Session in the paging message if received from the AMF, see TS 38.331 [12]."

- **Since:** Rel-15, V15.0.0 (`23502-f00.zip`), as "If the UE is in RM-REGISTERED state and CM-IDLE and reachable, the AMF sends a Paging message (NAS ID for paging, Registration Area list, Paging DRX length, Paging Priority indication, access associated to the PDU Session) to (R)AN node(s) belonging to the Registration Area(s) in which the UE is registered, then the RAN node pages the UE, ...".
- **Changes:** Rel-15: "reachable in 3GPP access", "Paging Priority" and "NG-RAN node" by V15.17.0. Rel-16: V16.19.0 adds "Enhanced Coverage Restricted information, WUS Assistance Information" to the message's content. Rel-17 to Rel-19: this sentence is unchanged from Rel-16. Later sentences of the step (extended DRX, paging strategies) were not compared.
- **Strength:** descriptive. The verb is "sends" in every version read. The step's only "shall" is about a second paging with a Paging Priority, which this check does not exercise.
- **Use:** it ties the paging to downlink data for a PDU session (the clause's steps 1 to 3: the UPF's Data Notification and the SMF's Namf_Communication_N1N2MessageTransfer) and says where the AMF pages: the RAN nodes of the UE's registration area, which include the gNB the UE registered through. That is why the PAGING is expected on the check's own association. Whether the TAI List for Paging holds the serving TAI is recorded, not asserted.

## TS 38.413 8.5.1.2 (Paging, Successful Operation), descriptive

> "The AMF initiates the Paging procedure by sending the PAGING message to the NG-RAN node. At the reception of the PAGING message, the NG-RAN node shall perform paging of the UE in cells which belong to tracking areas as indicated in the TAI List for Paging IE."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Changes:** Rel-15: V15.0.0 reads "paging procedure" and "List of TAIs"; "Paging procedure" and "TAI List for Paging" from V15.1.0 (`38413-f10.zip`). Rel-16 to Rel-19: only optional-IE sentences are added (NB-IoT DRX, CE, NPN and others); the quoted text is unchanged in V19.4.0.
- **Strength:** descriptive for the AMF. The "shall" binds the NG-RAN node.

## TS 38.413 9.2.4.1 (PAGING), descriptive

> "UE Paging Identity | M | ... 9.3.3.18" and "TAI List for Paging | | 1 | ..." with ">>TAI | M | 9.3.3.11"

- **Since:** Rel-15, V15.0.0.
- **Changes:** Rel-15: "List of TAIs" is renamed "TAI List for Paging" in V15.1.0. The V15.0.0 message also carried "UE Identity Index Value", which is no longer in the table by V19.4.0; the version that removed it was not traced.
- **Use:** the two IEs the check reads. TS 38.413 does not say which TAIs the list holds.

## TS 38.413 9.3.3.18 (UE Paging Identity), descriptive

> "CHOICE UE Paging Identity | M" with the one alternative ">5G-S-TMSI / >>5G-S-TMSI | M | 9.3.3.20"

- **Since:** Rel-15, V15.0.0. Unchanged through V19.4.0.
- **Use:** the identity compared with the UE's 5G-S-TMSI, taken from the 5G-GUTI of its Registration Accept.

## Tolerances

- A UPF need not buffer: the packet that triggers paging may be lost, and its delivery after the Service Request is not asserted.
- Downlink that reaches the core before it has switched the session to buffering goes to the released tunnel and raises no notification, and how long that switch takes is the core's own business. The check therefore does not guess a settle time: it sends the first trigger as soon as the UE CONTEXT RELEASE COMPLETE is written and sends it again every 2 s until a PAGING naming the UE arrives or the 10 s window, counted from the first trigger, ends (five triggers at most). The interval is the check's own choice, not a timer of the specification: the requirement is that the network pages when downlink data is pending, whenever that data arrives, so every trigger is the stimulus. 2 s leaves the core time to act on one trigger before the next. The detail records how many triggers were sent and when, and the PAGING's delay from the first trigger and from the last one before it.
- A UPF may de-duplicate its downlink data notifications per session for a while; one has been seen to hold off for 20 s. The re-sent triggers then raise nothing new, which changes no verdict; leave at least 20 s before a re-run on the same subscriber.
- The AN release uses cause Radio Network "release due to NG-RAN generated reason", not "user inactivity".

## Verdict

- **Class:** conformance, one shall (TS 24.501 5.6.2.2.1); the TS 23.502 and TS 38.413 clauses are descriptive.
- **Stimulus:** a UE with a PDU session, released to CM-IDLE by an AN release; a responder on N6 then sends downlink data to its address, again every 2 s, and the gNB's association is watched for NGAP PAGING for 10 s from the first.
- **PASS** `paged` (5.6.2.2.1): an NGAP PAGING whose UE Paging Identity is the UE's 5G-S-TMSI.
- **FAIL** `no-paging` (5.6.2.2.1): no PAGING within 10 s, a bound of the check.
- **FAIL** `other-identity` (5.6.2.2.1): only PAGING naming other identities within 10 s; none paged the UE.
- **FAIL** `association-lost` (5.6.2.2.1): the gNB association ended during the wait without a PAGING for the UE: the core dropped the UE's path instead of paging it. A fresh NG Setup then decides between this and CRASH.
- **CRASH:** after any judged outcome, a fresh NG Setup from another gNB ID does not complete.
- **BLOCKED** (registration, PDU session establishment): the UE or its session was not set up. **BLOCKED** (AN release): the core did not release the UE to CM-IDLE (no UE CONTEXT RELEASE COMMAND, or no 5G-GUTI to page it by).
- **ERROR:** the trigger failing, or the per-test timeout ending a wait. A lost association is ERROR only when the per-test timeout had already ended the check.
- **SKIP:** no UE or no N6 downlink trigger configured, or a target release older than Rel-15.
