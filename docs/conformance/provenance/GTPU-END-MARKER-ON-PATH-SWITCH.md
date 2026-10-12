# GTPU-END-MARKER-ON-PATH-SWITCH

Source: the 3GPP archive only:

- [https://www.3gpp.org/ftp/Specs/archive/23_series/23.501/](https://www.3gpp.org/ftp/Specs/archive/23_series/23.501/)
- [https://www.3gpp.org/ftp/Specs/archive/29_series/29.281/](https://www.3gpp.org/ftp/Specs/archive/29_series/29.281/)
- [https://www.3gpp.org/ftp/Specs/archive/29_series/29.244/](https://www.3gpp.org/ftp/Specs/archive/29_series/29.244/)

Versions read:

- **TS 23.501:** V15.0.0 (`23501-f00.zip`), V15.13.0 (`23501-fd0.zip`), V16.20.0 (`23501-gk0.zip`), V17.15.0 (`23501-hf0.zip`), V18.12.0 (`23501-ic0.zip`), V19.9.0 (`23501-j90.zip`).
- **TS 29.281:** V8.0.0 (`29281-800.zip`), V15.2.0 (`29281-f20.zip`), V15.5.0 (`29281-f50.zip`), V15.7.0 (`29281-f70.zip`), V16.2.0 (`29281-g20.zip`), V17.5.0 (`29281-h50.zip`), V18.4.0 (`29281-i40.zip`), V19.2.0 (`29281-j20.zip`).
- **TS 29.244:** V14.0.0 (`29244-e00.zip`), V15.0.0 (`29244-f00.zip`), V15.3.0 (`29244-f30.zip`), V15.10.0 (`29244-fa0.zip`), V16.13.0 (`29244-gd0.zip`), V17.0.0 (`29244-h00.zip`), V17.10.0 (`29244-ha0.zip`), V18.10.0 (`29244-ia0.zip`), V19.6.0 (`29244-j60.zip`). Rel-20 (`29244-k00`, `k10`) exists on the archive and was not read.

## TS 23.501 5.8.2.9.1 (UPF Constructing the "End marker" Packets), shall

> "On receiving this indication, the UPF shall construct end marker packet(s) and send it for each N3 GTP-U tunnel towards the source NG RAN after sending the last PDU on the old path."

- **Since:** Rel-15, V15.0.0 (`23501-f00.zip`).
- **Changes:** the quoted sentence is unchanged Rel-15 through Rel-19. Surrounding text: "In case of" in V15.0.0 reads "In the case of" by V15.13.0. Within Rel-16 (after V16.0.0, by V16.20.0) the two preamble paragraphs are removed, and the with-UPF-change case is reworded from "PSA UPF" to "the UPF with N9 reference point to the UPF terminating N3". The clause number 5.8.2.9.1 is unchanged.
- **Role:** the primary, release-independent obligation the check asserts. TS 23.501 5.8.2.9.2 lets the SMF construct the markers instead, with the UPF only forwarding them. The source gNB sees the same thing either way, so the check asserts only what arrives on the old tunnel, never who built it.

## TS 29.281 7.3.2 (End Marker), shall

> "The End Marker message(s) shall also be sent after sending the last G-PDU that needs to be sent on a GTP-U tunnel or after receiving an End Marker Indication as specified in 3GPP TS 23.501 [28] and 3GPP TS 23.502 [29]. End marker packets sent over data forward tunnels in 5GS, for data forwarding between 5GS and EPS, shall be sent with the PDU Session Container extension header including the Qos Flow Identifier of one of the QoS flows mapped to the same E-RAB." (7.3.2.3 from V16.1.0)

- **Since:** the 5GS sentence is from Rel-15, V15.2.0 (`29281-f20.zip`). The End Marker message, type 254 in clause 6.1, Table 6.1-1, is from Rel-8, V8.0.0 (`29281-800.zip`). The PDU Session Container and QFI sentence, which covers 5GS-EPS forwarding only, is from Rel-15, V15.5.0 (`29281-f50.zip`).
- **Changes:** in Rel-15 the text is in clause 7.3.2, with IE table 7.3.2-1. V16.0.0 adds "End Marker messages shall not be sent over the N19 interface." V16.1.0 splits the clause into 7.3.2.1 (General), 7.3.2.2 (EPS) and 7.3.2.3 (5GS). V16.2.0 adds "For QoS Flows transferred to and from Secondary RAN Node in Dual Connectivity ..., end marker packets shall be sent over old tunnel for the transferred QoS flows without the Qos Flow Identifier included as specified in 3GPP TS 37.340 [38]." Wording is unchanged from V16.2.0 through V19.2.0.
- **QFI:** TS 29.281 has no requirement for a PDU Session Container or QFI on an End Marker sent on N3 after an intra-5GS Xn path switch. The container "shall" applies only to 5GS-EPS data-forwarding tunnels, and a Dual Connectivity transfer requires the marker *without* a QFI. The End Marker IE table holds only the optional Private Extension. The check therefore records the container's presence and never asserts it.
- **Related shall text the check does not assert:** per 7.3.2.1, a G-PDU arriving after the End Marker may be silently discarded, and an End Marker with an unknown TEID shall be ignored.

## TS 29.244 8.2.31 (PFCPSMReq-Flags, SNDEM), shall when SNDEM is set

> "Bit 2 – SNDEM (Send End Marker Packets): if this bit is set to "1", it indicates that the UP function shall construct and send End Marker packets towards the old F-TEID of the downstream node when switching to the new F-TEID."

- **Since:** Rel-14, V14.0.0 (`29244-e00.zip`), as clause 8.2.31 "SxSMReq-Flags" (Sx naming). For 5GC, clause 5.3.2 carried "Editor's Note: Sending of End Marker Packets in 5GC is FFS" in V15.0.0 to V15.2.0. The 5GC reference (TS 23.501 5.8.2.9) appears from V15.3.0 (`29244-f30.zip`).
- **Changes:** Rel-14 names the IE SxSMReq-Flags. Rel-15 V15.0.0 renames it PFCPSMReq-Flags, in the same clause 8.2.31. By V15.10.0 "set to 1" reads "set to "1"" (editorial). The normative wording is otherwise unchanged Rel-14 through Rel-19.
- **Role:** cited in the absent-failure likely cause. The UPF's duty is conditional on the SMF setting SNDEM. TS 29.244 5.3.2 makes UP support optional before Rel-17, with the CP requesting it only if the UP advertised support. From Rel-17 (pinned to V17.0.0, `29244-h00.zip`) the UP "shall support" it and indicates EMPU, while the CP only "should request" it. TS 29.244 7.5.4.3, Table 7.5.4.3-2, requires the SNDEM flag to be present when the CP changes the downstream F-TEID and requests End Markers. So 23.501 5.8.2.9.1 is the primary reference, and `MinRelease()` is Rel-15.

## Sequencing and the window

TS 23.502 4.9.1.2.2 (Xn based inter NG-RAN handover without UPF re-allocation) orders the procedure as follows:

1. Path Switch Request to the AMF.
2. The SMF modifies the N4 session (steps 3 and 4).
3. The UPF sends the end marker(s) on the old path "immediately after switching the path" (step 5).
4. The SMF answers the AMF (step 6).
5. The AMF sends the Path Switch Request Acknowledge to the target gNB (step 7).

A conforming core's markers therefore usually reach the source gNB *before* the target gNB sees the Acknowledge. The check counts End Markers on the old F-TEID at the source gNB's N3 socket from the start of the handover, which precedes the Path Switch Request, until `datapath.EndMarkerGraceTTL` (5 s) after the Acknowledge. That is as long as the engine keeps the source tunnel. A marker before the handover starts is recorded in the detail and not counted. TS 23.502 is the sequencing rationale only. It is not in the check's references, because it has not been archive-verified.

## Verdicts

- **PASS:** one or more End Markers on the old F-TEID within the window, and no G-PDU on that tunnel after the first of them.
- **FAIL (absent):** no End Marker on the old F-TEID within the window. On SD-Core the likely cause is the UPF's `enable_end_marker` being off (`upf.jsonc`; the chart, the OnRamp fork and Ref 2 leave it unset), or the SMF not setting SNDEM.
- **FAIL (ordering):** a G-PDU on the old F-TEID after the End Marker, which TS 23.501 5.8.2.9.1 rules out ("after sending the last PDU on the old path").
- **ERROR:** the harness could not run the check. Causes are a fixed-port `--bind`, a registration, PDU session or path switch that did not complete, or a per-test timeout shorter than the window.
- **SKIP:** no UE configured, or a target release older than Rel-15.
- **Detail:** always records the old TEID, the source N3 socket, the End Marker counts (counted, before the handover, after the window, on other tunnels), whether a PDU Session Container with QFI was present, the downlink G-PDUs on the old path before the Acknowledge, and whether traffic was flowing. It never includes key material.
