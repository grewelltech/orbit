# NGAP-UNKNOWN-UE-SURVIVES

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: V15.0.0 (`38413-f00.zip`), V15.1.0 (`38413-f10.zip`), V15.3.0, V15.4.0 (`38413-f40.zip`), V15.13.0 (`38413-fd0.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`), V19.2.0, V19.3.0, V19.4.0 (`38413-j40.zip`).

## TS 38.413 10.6 (Handling of AP ID), shall

> "if this message is not the last message for this UE-associated logical connection, the node shall initiate an Error Indication procedure with inclusion of the received AP ID(s) from the peer node and an appropriate cause value. Both nodes shall initiate a local release of any established UE-associated logical connection (for the same NG interface) having the erroneous AP ID as either the local or remote identifier."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`). The first sentence is word-for-word the same in V15.0.0. The second sentence reaches this wording in V15.1.0 (`38413-f10.zip`).
- **Changes:** Rel-15: V15.1.0 rewords the lead-in ("an erroneous AP ID that is either an unknown local AP ID, or an inconsistent remote AP ID") and the release sentence ("having the erroneous AP ID(s) as local or remote identifier" becomes "having the erroneous AP ID as either the local or remote identifier"). V15.3.0 adds a new paragraph for an unknown local AP ID in a "first returned message". Wording unchanged from Rel-16 through Rel-19.
- **Scope of the check:** the rule applies to messages other than the first or first-returned message. If the erroneous message is the "last message" (for example UE CONTEXT RELEASE COMPLETE), the clause requires only a local release, with no Error Indication. This check's stimulus is an UPLINK NAS TRANSPORT for UE NGAP IDs the AMF never allocated, which is neither the first nor the last message on a UE-associated connection, so the local-release-only exception does not apply and an ERROR INDICATION is required.
- **Verdicts:** ERROR INDICATION, with the NG association still able to complete a fresh NG Setup: PASS. The association survives but no ERROR INDICATION arrives (a silent drop, or some other PDU): DEVIATION, the detail naming what was observed. The association is reset, or a fresh NG Setup then fails: FAIL. The check does not assert the local release itself, which is not observable from the gNB side. Earlier citations of 8.7.1 were wrong: 8.7.1 is NG Setup, Error Indication is 8.7.5.

## TS 38.413 8.7.5.2 (Error Indication, Successful Operation), shall

> "In case the Error Indication procedure is triggered by utilising UE-associated signalling the AMF UE NGAP ID IE and the RAN UE NGAP ID IE shall be included in the ERROR INDICATION message. ... If one or both of the AMF UE NGAP ID IE and the RAN UE NGAP ID IE are not correct, the cause shall be set to an appropriate value, e.g., "Unknown local UE NGAP ID" or "Inconsistent remote UE NGAP ID"."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`) for the obligation. The example cause names shown date from V15.4.0 (`38413-f40.zip`).
- **Changes:** Rel-15: in V15.0.0 to V15.3.0 the example causes were "Unknown or already allocated AMF UE NGAP ID", "Unknown or already allocated RAN UE NGAP ID" and "Unknown or inconsistent pair of UE NGAP ID". Rel-16, Rel-17, Rel-18: unchanged. Rel-19: V19.2.0 adds A-IoT/AIOTF figures and text; V19.3.0 inserts an NB-IoT CP-CIoT 5G-S-TMSI sentence between the two quoted sentences (shown as "..."). The quoted sentences are otherwise unchanged.
- **Scope:** the cause names are examples ("e.g."), not a mandated value; the check accepts any appropriate cause. Per 8.7.5.1, the ERROR INDICATION uses UE-associated signalling when the error came from a UE-associated message.
