# NGAP-NGSETUP-MISSING-TALIST

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: V15.0.0 (`38413-f00.zip`), V15.13.0 (`38413-fd0.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`), V19.4.0 (`38413-j40.zip`).

## TS 38.413 9.2.6.1 Supported TA List (NG SETUP REQUEST), descriptive (mandatory IE)

> "Supported TA List | | 1 | | Supported TAs in the NG-RAN node. | YES | reject" (table row: range 1, i.e. mandatory, criticality reject). The ASN.1 has "{ ID id-SupportedTAList CRITICALITY reject TYPE SupportedTAList PRESENCE mandatory }".

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Changes:** Rel-15: in V15.0.0 the criticality column reads "GLOBAL reject"; by V15.13.0 it reads "YES reject". Rel-16 to Rel-18: unchanged. Rel-19: by V19.4.0 (absent in V18.11.0; the exact Rel-19 version was not traced) the semantics text adds "This IE is ignored if the A-IoT Support IE is present and set to "A-IoT only"". The IE is still mandatory with criticality reject.
- **Scope:** valid provided the AMF is not A-IoT and the test leaves out the whole Supported TA List protocol IE. Leaving out a field inside a Supported TA Item (TAC, Broadcast PLMN List) is an ASN.1 SEQUENCE violation, a Transfer Syntax Error under 10.2, for which the receiver "should" send an ERROR INDICATION, not NG SETUP FAILURE.

## TS 38.413 10.3.5 Reject IE bullet 1 (Missing IE or IE group), shall, primary

> "if a received message initiating a procedure is missing one or more IEs/IE groups with specified criticality "Reject IE"; none of the functional requests of the message shall be executed. The receiving node shall reject the procedure and report the missing IEs/IE groups using the message normally used to report unsuccessful outcome of the procedure."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Changes:** wording unchanged Rel-15 through Rel-19. In 10.3.1 (which classifies a missing IE as abstract syntax error case 3), Rel-17 adds case 6 and Rel-18 changes "subclause" to "clause"; this does not touch 10.3.5.
- **Criticality Diagnostics:** per the last paragraph of 10.3.5, the NG SETUP FAILURE's Criticality Diagnostics IE shall carry the Information Element Criticality Diagnostics for the missing IE; the check records whether the IE is present but does not assert it.
- **Error Indication instead:** the same bullet continues: "In case the information received in the initiating message was insufficient to determine a value for all IEs that are required to be present in the message used to report the unsuccessful outcome of the procedure, the receiving node shall instead terminate the procedure and initiate the Error Indication procedure." The NG SETUP FAILURE (9.2.6.3) has two mandatory IEs, Message Type and Cause (read in V15.0.0 and V19.4.0), and neither depends on the missing Supported TA List, so this exception does not apply and an ERROR INDICATION is not the answer 10.3.5 requires. Clause 10.3.5 applies to any "message initiating a procedure", UE-associated or not.

## Verdict

- **Class:** conformance, one shall (10.3.5).
- **Stimulus:** one association from the suite's N2 source address and one NG SETUP REQUEST with the run's identity and the check's own gNB ID, with the whole Supported TA List protocol IE left out. The answer is awaited for 4 s.
- **PASS** `setup-failure`: NG SETUP FAILURE. Whether it carries Criticality Diagnostics is in the detail.
- **FAIL** (10.3.5), each its own outcome: `accepted` (NG SETUP RESPONSE), `other-pdu` (an ERROR INDICATION or another PDU; the detail names it and when it came), `silent-drop` (neither within 4 s, a bound of the check: TS 38.413 sets no timer for the NG Setup outcome; the absence is inferred within it), `association-reset`.
- **CRASH:** after any outcome but a reset association, a fresh NG Setup with another gNB ID is tried; a core that no longer completes it did not survive the stimulus.
- **ERROR:** the association could not be opened, the stimulus could not be built or sent, the per-test timeout expired before the 4 s wait, or the liveness probe could not run (a probe identity the AMF rejects is the run's --mcc/--mnc, not the core).
- **BLOCKED:** none: the NG Setup is the stimulus, so the check needs no procedure first.
- **SKIP:** a target release older than Rel-15.
- **Limits:** the Criticality Diagnostics content and the cause value are not asserted.
