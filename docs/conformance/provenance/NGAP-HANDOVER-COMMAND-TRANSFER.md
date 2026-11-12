# NGAP-HANDOVER-COMMAND-TRANSFER

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: V15.0.0 (`38413-f00.zip`), V15.1.0 (`38413-f10.zip`), V15.5.0 (`38413-f50.zip`), V15.13.0 (`38413-fd0.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`), V19.4.0 (`38413-j40.zip`). The Rel-15 history below also names V15.3.0 and V15.4.0, from the same verification pass (#140).

## TS 38.413 8.4.1.2 (Handover Preparation, Successful Operation), shall, primary

> "When the preparation, including the reservation of resources at the target side is ready, the AMF responds with the HANDOVER COMMAND message to the source NG-RAN node. In case of intra-system handover, the AMF shall include the PDU Session Resource Handover List IE in the HANDOVER COMMAND message."

- **Since:** Rel-15, V15.5.0 (`38413-f50.zip`), for the second ("shall") sentence. The first sentence is present from V15.0.0.
- **Changes:** within Rel-15, V15.0.0 had a "PDU Session Resource Subject to Forwarding List" (0..1) with Handover Command Transfer O. V15.1.0 to V15.3.0 have "PDU Session Resource Handover List" with range 1 (mandatory). V15.4.0 makes it 0..1 with no obligation in the text. V15.5.0 adds the intra-system "shall include" sentence. Rel-16 through Rel-19 leave the quoted sentences unchanged.
- **Strength, sentence by sentence:** the first sentence is descriptive ("the AMF responds with"). The second is the shall, and it is about what the HANDOVER COMMAND carries, not about whether one is sent. 8.4.1.3 ("the AMF sends the HANDOVER PREPARATION FAILURE message") is descriptive too: no version read obliges the AMF to answer a valid HANDOVER REQUIRED with a HANDOVER COMMAND.
- **Role:** the second sentence is the obligation the check asserts. The check's handover is intra-system (handover type intra5gs, NG-RAN to NG-RAN).

## TS 38.413 9.2.3.2 (HANDOVER COMMAND), IE presence

> ">>Handover Command Transfer | M | | OCTET STRING | Containing the Handover Command Transfer IE specified in clause 9.3.4.10."

The row sits inside PDU Session Resource Handover List (0..1) > Item (1..<maxnoofPDUSessions>), next to ">>PDU Session ID", M.

- **Since:** Rel-15, V15.1.0 (`38413-f10.zip`), for the transfer being M per item. In V15.0.0 it was O.
- **Changes:** Rel-15 to Rel-17 say "subclause 9.3.4.10"; Rel-18 and Rel-19 say "clause 9.3.4.10" (editorial).
- **Role:** the per-item part of the check: each listed PDU session carries a transfer, which 9.1.1 makes a shall.

## TS 38.413 9.1.1 (Presence), shall

> "IEs marked as Mandatory (M) shall always be included in the message."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Versions read for this clause:** V15.0.0, V15.13.0, V16.18.0, V17.15.0, V18.11.0 and V19.4.0.
- **Changes:** none; the sentence is the same in all six.
- **Role:** the shall behind an IE that the message's table marks M: a Handover Command Transfer in each item of the PDU Session Resource Handover List (9.2.3.2, from V15.1.0).

## TS 38.413 9.3.4.10 (Handover Command Transfer), contents

> "This IE is transparent to the AMF."

- **Since:** Rel-15. 9.3.4.10 is the Handover Command Transfer clause in every version read.
- **Changes:** the contents grow over the releases. Rel-15 adds UL forwarding and additional tunnels, Rel-16 adds the Data Forwarding Response E-RAB List, and the QoS Flow Failed to Setup List is present by V19.4.0.
- **Role:** why nothing inside the transfer is asserted. Every IE of 9.3.4.10 is O: DL Forwarding UP TNL Information, QoS Flow to be Forwarded List (0..1), Data Forwarding Response DRB List and UL Forwarding UP TNL Information. The check asserts that a transfer is present per listed PDU session and that it decodes. It does not require forwarding tunnels.

Nothing in 38.413 says every admitted PDU session must appear in the list, so the check does not require the UE's session to be among the items; it records when it is not.

## The strict-3gpp profile

The target gNB answers the AMF's Handover Request with the conformant Handover Request Acknowledge Transfer (the `strict-3gpp` core profile, [ADR-0002](../../adr/0002-strict-codecs-with-core-profiles.md)). The check never selects a core profile. A core whose SMF cannot decode that transfer needs a core profile's workaround for ordinary runs; under this check the core is measured as it behaves without one: what its HANDOVER COMMAND then carries is the result.

## Verdict

- **Class:** conformance, from the shalls of 8.4.1.2 and 9.1.1. The check owns the procedure "N2 handover preparation": a check that needs a prepared handover reports BLOCKED and names this check when the core does not answer the HANDOVER REQUIRED with a HANDOVER COMMAND.
- **Stimulus:** a HANDOVER REQUIRED from gNB A for an intra-system handover of the UE to gNB B, after gNB B's own NG Setup. The handover, the target's NG Setup included, is given 8 s.
- **PASS** `command-with-transfers` (8.4.1.2, 9.1.1): the HANDOVER COMMAND carries the PDU Session Resource Handover List, and every item in it a Handover Command Transfer that decodes.
- **FAIL** `no-handover-list` (8.4.1.2): the list is absent, or present without items.
- **FAIL** `transfer-missing` (9.1.1): an item's transfer is empty or does not decode.
- **WARN (availability)** `refused`: a HANDOVER PREPARATION FAILURE, before or after gNB B acknowledged its HANDOVER REQUEST (the detail says which); Observed names the cause.
- **WARN (availability)** `unanswered`: neither HANDOVER COMMAND nor HANDOVER PREPARATION FAILURE within the 8 s, a bound of the check (TS 38.413 leaves the value of TNGRELOCprep to the implementation).
- **WARN (availability)** `no-command`: another message in place of the HANDOVER REQUEST or the HANDOVER COMMAND, or an association lost while one was awaited.
- The three WARNs rest on no requirement: 8.4.1.2 and 8.4.1.3 describe the AMF's answer without obliging it to prepare the handover. The criterion is availability: the core cannot complete the procedure.
- **Not completed:** the three WARNs report the N2 handover preparation as not completed, so the BLOCKED results of dependent checks in the same run are counted under this one.
- **BLOCKED:** the core did not complete the UE's registration, its PDU session establishment, or gNB B's NG Setup. The result names the procedure and its owner check.
- **CRASH:** after no HANDOVER COMMAND (no answer, another answer or a lost association, not a HANDOVER PREPARATION FAILURE), a fresh NG Setup from another gNB ID does not complete.
- **ERROR:** the harness could not run the check (a fixed-port `--bind`), a setup step failed for a reason on this host, a message could not be built or sent (`handover could not be started`), or the per-test timeout expired before the 8 s wait ended.
- **SKIP:** no UE configured, or a target release older than Rel-15. The "shall" dates from V15.5.0, and Rel-15 is treated as applicable.
- **Detail:** the Handover List item by item (transfer length, whether it decodes), and whether the UE's session is listed. It never includes key material. A handover that fails after the command keeps the command's verdict and says so in the detail.
- **Limits:** nothing inside the transfer is asserted, nor that every session is listed.

After the command the check completes the handover (Handover Notify from the target gNB) and deregisters the UE there.

Failure message: `HANDOVER COMMAND lacks the PDU Session Resource Handover List / per-item Handover Command Transfer. TS 38.413 8.4.1.2 (Rel-15 V15.5.0 onward), 9.2.3.2, 9.1.1.`
