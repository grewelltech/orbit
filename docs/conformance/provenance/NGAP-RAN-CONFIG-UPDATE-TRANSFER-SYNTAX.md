# NGAP-RAN-CONFIG-UPDATE-TRANSFER-SYNTAX

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: V15.0.0 (`38413-f00.zip`), V15.13.0 (`38413-fd0.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`), V19.0.0, V19.4.0 (`38413-j40.zip`).

## TS 38.413 10.2 (Transfer Syntax Error), should, primary

> "Missing mandatory elements in ASN.1 SEQUENCE definitions"

is one of the cases 10.2 lists as a Transfer Syntax Error, and for one the receiver

> "should initiate Error Indication procedure"

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Changes:** none; the wording of 10.2 is unchanged V15.0.0 to V19.4.0.
- Only these two fragments were quoted from the archive; the sentences around them are not reproduced here.

## Why a transfer syntax error (cited here only, not in the check's references)

- **TS 38.413 9.2.6.4 (RAN CONFIGURATION UPDATE), descriptive:** "Message Type | M | ... | YES | reject"; every other top-level IE is O or 0..1 (RAN Node Name, Supported TA List 0..1, Default Paging DRX, Global RAN Node ID, NG-RAN TNL Association to Remove List, NB-IoT Default Paging DRX, Extended RAN Node Name, Additional ULI, A-IoT Support). The ASN.1 `RANConfigurationUpdateIEs` list every IE as "PRESENCE optional". Since Rel-15, V15.0.0; the list of optional IEs grows each release and none has ever been mandatory. So a "missing mandatory IE" case under 10.3.5 cannot be built for this message. Leaving out a mandatory field inside an included group, such as the Broadcast PLMN List of a Supported TA Item, is a Transfer Syntax Error under 10.2.
- **TS 38.413 8.7.2.3 (RAN Configuration Update, Unsuccessful Operation), shall:** "If the AMF cannot accept the update, it shall respond with a RAN CONFIGURATION UPDATE FAILURE message and appropriate cause value." Since Rel-15, V15.0.0; the sentence is unchanged Rel-15 through Rel-19 (Rel-15 adds a Time to Wait sentence; Rel-19 V19.0.0 adds AIOTF applicability text). 8.7.2.3 does not define "cannot accept", and for a transfer syntax error 10.2 names the Error Indication procedure, so RAN CONFIGURATION UPDATE FAILURE is not the specified outcome and the check does not assert it.

## Verdict

- **Class:** conformance, one should (10.2): the check cites no shall, so its worst verdict is DEVIATION.
- **Stimulus:** on an association that has completed NG Setup, a RAN CONFIGURATION UPDATE (stream 0) whose only IE is a Supported TA List with one Supported TA Item cut off after its TAC, the IE's and the message's lengths stating the shortened sizes. For TAC 1 the PDU is `00 23 00 0c 00 00 01 00 66 00 05 00 00 00 00 01`. Everything before the cut is what the encoder produces for the well-formed message. The answer is awaited for 3 s.
- **PASS** `error-indication`: an ERROR INDICATION. The cause (10.2 points at Protocol "Transfer Syntax Error") is recorded, not asserted.
- **DEVIATION** (10.2), each its own outcome: `silent-drop` (`silently dropped; association survived`: nothing within 3 s, a bound of the check, as TS 38.413 sets no timer for the ERROR INDICATION; the absence is inferred within it), `other-pdu` (the PDU that came instead: `RAN CONFIGURATION UPDATE FAILURE`, `RAN CONFIGURATION UPDATE ACKNOWLEDGE (accepted!)`, or another), `association-lost` (`association reset`: the AMF dropped the association and a fresh NG Setup still completes). The detail ends with: "no ERROR INDICATION for a transfer-syntax error in RAN CONFIGURATION UPDATE. TS 38.413 10.2 (should). Likely cause: AMF silently drops malformed PDUs."
- **CRASH:** after every outcome, a lost association included, a fresh NG Setup with another gNB ID is tried; a core that no longer completes it did not survive the stimulus (`AMF unresponsive after the message (...)`).
- **BLOCKED:** the check's NG Setup did not complete (`blocked-ng-setup`; `NGAP-NG-SETUP` owns whether it completes).
- **ERROR:** the stimulus could not be built or sent, the per-test timeout expired before the 3 s wait, or the liveness probe could not run.
- **SKIP:** a target release older than Rel-15.
