# NGAP-PATH-SWITCH-ACK-TRANSFER

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: V15.0.0 (`38413-f00.zip`), V15.1.0 (`38413-f10.zip`), V15.13.0 (`38413-fd0.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`), V19.4.0 (`38413-j40.zip`).

## TS 38.413 8.4.4.2 (Path Switch Request, Successful Operation), shall

> "After all necessary updates including the UP path switch have been successfully completed in the 5GC for at least one of the PDU session resources included in the PATH SWITCH REQUEST, the AMF shall send the PATH SWITCH REQUEST ACKNOWLEDGE message to the NG-RAN node and the procedure ends."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Changes:** the quoted sentence is word-for-word the same from Rel-15 through Rel-19. In Rel-17 and Rel-18 sentences were added around it (IAB-MT, MT communication handling, RedCap).
- **Role:** the obligation the check asserts. The UE has one PDU session, so "at least one of the PDU session resources" is that session.

The same clause says what an absent uplink endpoint means:

> "If the UL NG-U UP TNL Information IE is included within the Path Switch Request Acknowledge Transfer IE of the PATH SWITCH REQUEST ACKNOWLEDGE message, the NG-RAN node shall store this information and use it as the uplink termination point for the user plane data for this PDU session."

This sentence is present from V15.1.0. It binds the NG-RAN node, not the core. Without the IE the uplink termination point stays as it was.

## TS 38.413 9.3.4.9 (Path Switch Request Acknowledge Transfer), IE presence

> "UL NG-U UP TNL Information | O | | UP Transport Layer Information 9.3.2.2 | UPF endpoint of the NG-U transport bearer corresponding to the DL NG-U UP TNL Information IE received in the Path Switch Request Transfer IE."

- **Since:** Rel-15. V15.0.0 has the IE as M ("UPF endpoint of the NG-U transport bearer, for delivery of UL PDUs."). It is O from V15.1.0 (`38413-f10.zip`). The semantics text quoted above dates from V15.3.0.
- **Changes:** Rel-16 to Rel-19 leave the row unchanged and add other optional IEs (redundant TNL, QoS Flow Parameters List and others).
- **Role:** why the check never requires the IE. A check that expected UL NG-U UP TNL Information for each switched session would fail a conforming core in every version from V15.1.0 to V19.4.0.

## TS 38.413 9.2.3.9 (PATH SWITCH REQUEST ACKNOWLEDGE), IE presence

- "PDU Session Resource Switched List" (range 1) with ">>Path Switch Request Acknowledge Transfer", presence M, is present from V15.1.0. In V15.0.0 the list was "PDU Session Resource to be Switched in Uplink List", 0..1.
- **Role:** the per-session part of the check. 38.413 mandates a Path Switch Request Acknowledge Transfer in each item of the Switched List, and nothing inside it.

## What the check does

It registers a UE with one PDU session on gNB A and hands it over by Xn to gNB B, which sends the Path Switch Request on a second association. The handover, the target's NG Setup included, is given 5 s.

With `--traffic` (a UDP echo peer on the data network) the UE sends to the peer from before the handover until the check ends, and the check watches the downlink for echoes. The uplink is probed only when the peer answered before the handover. An echo counts as an answer to uplink sent after the switch when it arrives between 1 s and 2 s after the Acknowledge, on either downlink path.

## Verdicts

- **PASS:** an Acknowledge whose Switched List carries the UE's session with a transfer that decodes, every other listed item likewise, and no sign of a broken uplink. The observed line says whether UL NG-U UP TNL Information was present and whether the uplink was answered or not probed.
- **FAIL (no Acknowledge):** a PATH SWITCH REQUEST FAILURE, any other answer, or none within the 5 s.
- **FAIL (transfer):** no Switched List, an item whose transfer is empty or does not decode, or the UE's session missing from the list.
- **FAIL (uplink):** the transfer carries no UL NG-U UP TNL Information, the echo peer answered before the handover, and no echo arrived in the window after it. The spec meaning of the absent IE is an unchanged uplink endpoint, so uplink to it has to keep working. Seen from the UE the same silence can come from a downlink that was never switched; either way the user plane the Acknowledge reports as switched is not carrying the flow.
- **ERROR:** the harness could not run the check (a fixed-port `--bind`, a registration or PDU session that did not complete, a target NG Setup that failed, a per-test timeout inside the window). Also ERROR: no echo after a switch whose transfer named a new uplink endpoint. The check asserts nothing about a new endpoint, so that silence is recorded as inconclusive rather than scored.
- **SKIP:** no UE configured, or a target release older than Rel-15.
- **Detail:** the Switched List item by item (transfer length, whether it decodes, whether UL NG-U UP TNL Information is present) and the echo counts. It never includes key material.

Failure message: `PATH SWITCH REQUEST ACKNOWLEDGE missing / per-session transfer missing. TS 38.413 8.4.4.2, 9.3.4.9 (UL NG-U UP TNL optional from V15.1.0). Likely cause: AMF/SMF path-switch handling.`
