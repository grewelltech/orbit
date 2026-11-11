# NGAP-PATH-SWITCH-ACK-TRANSFER

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: V15.0.0 (`38413-f00.zip`), V15.1.0 (`38413-f10.zip`), V15.13.0 (`38413-fd0.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`), V19.4.0 (`38413-j40.zip`).

## TS 38.413 8.4.4.2 (Path Switch Request, Successful Operation), shall, primary

> "After all necessary updates including the UP path switch have been successfully completed in the 5GC for at least one of the PDU session resources included in the PATH SWITCH REQUEST, the AMF shall send the PATH SWITCH REQUEST ACKNOWLEDGE message to the NG-RAN node and the procedure ends."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Changes:** the quoted sentence is word-for-word the same from Rel-15 through Rel-19. In Rel-17 and Rel-18 sentences were added around it (IAB-MT, MT communication handling, RedCap).
- **Role:** the obligation the check asserts. The UE has one PDU session, so "at least one of the PDU session resources" is that session.

The same clause says what an absent uplink endpoint means:

> "If the UL NG-U UP TNL Information IE is included within the Path Switch Request Acknowledge Transfer IE of the PATH SWITCH REQUEST ACKNOWLEDGE message, the NG-RAN node shall store this information and use it as the uplink termination point for the user plane data for this PDU session."

This sentence is present from V15.1.0. It binds the NG-RAN node, not the core. Without the IE the uplink termination point stays as it was.

## TS 38.413 8.4.4.3 (Path Switch Request, Unsuccessful Operation), shall

> "If the 5GC fails to switch the downlink termination point of the NG-U transport bearer towards a new termination point for all PDU session resources, the AMF shall send the PATH SWITCH REQUEST FAILURE message to the NG-RAN node."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`), worded "If the 5GC fails to switch the downlink GTP tunnel endpoint towards a new GTP tunnel endpoint for all PDU session resources, the AMF shall send the PATH SWITCH REQUEST FAILURE message to the NG-RAN node with an appropriate cause value."
- **Versions read for this clause:** V15.0.0, V15.13.0, V16.18.0, V17.15.0, V18.11.0 and V19.4.0.
- **Changes:** the current wording is present by V15.13.0 and unchanged through V19.4.0. From V15.13.0 two sentences on the PDU Session Resource Released List follow it.
- **Role:** with 8.4.4.2 it covers every outcome of the 5GC's switch: switched for at least one session, the AMF shall send the Acknowledge; failed for all, it shall send the Failure. An AMF that sends neither violates one of the two. Neither clause obliges the 5GC to switch, so a Failure violates nothing.

## TS 38.413 9.1.1 (Presence), shall

> "IEs marked as Mandatory (M) shall always be included in the message."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Versions read for this clause:** V15.0.0, V15.13.0, V16.18.0, V17.15.0, V18.11.0 and V19.4.0.
- **Changes:** none; the sentence is the same in all six.
- **Role:** the shall behind an IE that the message's table marks M: a PDU Session Resource Switched List in the Acknowledge (9.2.3.9, from V15.1.0) and a Path Switch Request Acknowledge Transfer in each of its items.

## TS 38.413 9.3.4.9 (Path Switch Request Acknowledge Transfer), IE presence

> "UL NG-U UP TNL Information | O | | UP Transport Layer Information 9.3.2.2 | UPF endpoint of the NG-U transport bearer corresponding to the DL NG-U UP TNL Information IE received in the Path Switch Request Transfer IE."

- **Since:** Rel-15. V15.0.0 has the IE as M ("UPF endpoint of the NG-U transport bearer, for delivery of UL PDUs."). It is O from V15.1.0 (`38413-f10.zip`). The semantics text quoted above dates from V15.3.0.
- **Changes:** Rel-16 to Rel-19 leave the row unchanged and add other optional IEs (redundant TNL, QoS Flow Parameters List and others).
- **Role:** why the check never requires the IE. A check that expected UL NG-U UP TNL Information for each switched session would fail a conforming core in every version from V15.1.0 to V19.4.0.

## TS 38.413 9.2.3.9 (PATH SWITCH REQUEST ACKNOWLEDGE), IE presence

- "PDU Session Resource Switched List" (range 1) with ">>Path Switch Request Acknowledge Transfer", presence M, is present from V15.1.0. In V15.0.0 the list was "PDU Session Resource to be Switched in Uplink List", 0..1.
- **Role:** the per-session part of the check. 38.413 marks the list and a Path Switch Request Acknowledge Transfer in each of its items M, which 9.1.1 makes a shall, and mandates nothing inside the transfer.

## What the check does

It registers a UE with one PDU session on gNB A and hands it over by Xn to gNB B, which sends the Path Switch Request on a second association. The handover, the target's NG Setup included, is given 5 s.

With `--traffic` (a UDP echo peer on the data network) the UE sends to the peer from before the handover until the check ends, and the check watches the downlink for echoes. The uplink is probed only when the peer answered before the handover. An echo counts as an answer to uplink sent after the switch when it arrives between 1 s and 2 s after the Acknowledge, on either downlink path.

## Verdict

- **Class:** conformance, from the shalls of 8.4.4.2, 8.4.4.3 and 9.1.1. The check owns the procedure "Xn path switch": a check that needs one reports BLOCKED and names this check when the core does not acknowledge the PATH SWITCH REQUEST.
- **Stimulus:** a PATH SWITCH REQUEST from gNB B for the UE's PDU session, after gNB B's own NG Setup. The handover, the target's NG Setup included, is given 5 s.
- **PASS** `acknowledged` (8.4.4.2, 9.1.1): an Acknowledge whose Switched List carries the UE's session with a transfer that decodes, every other listed item likewise, and no sign of a broken uplink. The observed line says whether UL NG-U UP TNL Information was present and whether the uplink was answered or not probed.
- **FAIL** `no-switched-list` (9.1.1): an Acknowledge without the Switched List.
- **FAIL** `transfer-missing` (9.1.1): an item whose transfer is empty or does not decode.
- **FAIL** `unanswered` (8.4.4.2, 8.4.4.3): neither Acknowledge nor Failure within the 5 s, a bound of the check (TS 38.413 sets no timer for the answer).
- **FAIL** `other-answer` (8.4.4.2, 8.4.4.3): another message in place of either, or the association lost while waiting.
- **WARN (availability)** `refused`: a PATH SWITCH REQUEST FAILURE. 8.4.4.3 requires it when the 5GC fails to switch, and nothing obliges the 5GC to switch, so no requirement is violated; the core cannot complete the path switch.
- **WARN (availability)** `session-not-listed`: an Acknowledge whose Switched List leaves out the UE's only session. No shall covers which sessions the list carries; the gNB cannot tell that the session's user plane survived.
- **WARN (availability)** `uplink-not-answered`: the echo peer answered before the handover and no echo arrived between 1 s and 2 s after the Acknowledge. Without UL NG-U UP TNL Information the uplink endpoint is unchanged; with it the uplink goes to the endpoint it names. Either way the user plane the Acknowledge reports as switched is not carrying the flow, and no shall obliges it to: the user data is lost.
- **Not completed:** `refused`, `unanswered` and `other-answer` report the Xn path switch as not completed, so the BLOCKED results of dependent checks in the same run are counted under this one.
- **BLOCKED:** the core did not complete the UE's registration, its PDU session establishment, or gNB B's NG Setup. The result names the procedure and its owner check.
- **ERROR:** the harness could not run the check (a fixed-port `--bind`), a setup step failed for a reason on this host, the PATH SWITCH REQUEST could not be built or sent, or the per-test timeout expired before the 5 s wait or inside the uplink window.
- **SKIP:** no UE configured, or a target release older than Rel-15.
- **Detail:** the Switched List item by item (transfer length, whether it decodes, whether UL NG-U UP TNL Information is present) and the echo counts. It never includes key material.
- **Limits:** seen from the UE, a silent uplink after the switch cannot be told from a downlink that was never switched. The content of the transfer beyond UL NG-U UP TNL Information is not asserted.

Failure message: `PATH SWITCH REQUEST ACKNOWLEDGE missing / per-session transfer missing. TS 38.413 8.4.4.2, 9.3.4.9 (UL NG-U UP TNL optional from V15.1.0). Likely cause: AMF/SMF path-switch handling.`
