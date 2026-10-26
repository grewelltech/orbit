# NAS-PDU-UNKNOWN-DNN

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/).

Versions read:
- orbit-architect, 2026-10-12 and 2026-10-18 (issue #137): V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`) and V19.8.0 (`24501-j80.zip`), with V15.1.0 and V15.2.0 for the step 0 reading.
- orbit-conformance-test-writer, 2026-10-26, for these clauses:
  - Rel-15: every version from V15.0.0 to V15.7.0 (`f00` to `f70`), including V15.2.1 (`f21`);
  - Rel-16: `g00` to `g40`, `g51`, and `g60` to `gf0` (not `g01`, `g02`, `g41` or `g50`);
  - Rel-17: `h00` to `h21` and `h30` to `hi1` (not `h31`, `h41`, `h61`, `h71`, `ha1` or `hb1`);
  - then only V18.14.0 (`ie0`), V19.8.0 (`j80`) and V20.1.0 (`k10`). The Rel-18 versions `i00` to `id0` were not bisected.

Unread versions sit between read versions with identical wording, except Rel-18 before V18.14.0, where only V18.14.0 was read.

Change points for the wording below: V15.0.0 (`f00`), V15.1.0 (`f10`), V15.2.0 (`f20`), V15.3.0 (`f30`), V16.0.0 (`g00`), V16.1.0 (`g10`), V16.3.0 (`g30`), V16.15.0 (`gf0`), V17.3.0 (`h30`) and V17.5.0 (`h50`).

Latest of each release read: V15.7.0 (`f70`), V16.15.0 (`gf0`), V17.18.1 (`hi1`), V18.14.0 (`ie0`), V19.8.0 (`j80`), and V20.1.0 (`k10`).

The network can catch an unknown DNN in either of two places, and either is conformant:
- If no SMF is selected, the **AMF** returns the request (5.4.5.2.5, with the DL NAS TRANSPORT built per 5.4.5.3.1 and 5.4.5.3.2).
- If an SMF is reached, the **SMF** rejects the request (6.4.1.4.1).

## TS 24.501 5.4.5.2.5 a) 1) (UL NAS transport, abnormal cases on the network side), shall

From V15.3.0:

> "... the Request type IE is set to "initial request", and the SMF selection fails, then the AMF shall send back to the UE the 5GSM message which was not forwarded as specified in subclause 5.4.5.3.1 case e) ..."

V15.0.0:

> "... and the SMF selection fails then the AMF may send back to the UE the 5GSM message which was not forwarded as specified in subclause 5.4.5.3.1 case e)"

- **Since:** Rel-15. It is a shall from V15.1.0 (`24501-f10.zip`). V15.0.0 has only the "may" above, as item a) with no sub-items.
- **Changes:**
  - Rel-15:
    - V15.1.0 to V15.2.1: a) 1) is a shall only when "the SMF selection fails due to the DNN is not supported in the slice identified by the S-NSSAI used by the AMF". Any other reason falls under a) 2), which is a may. In V15.2.0 and V15.2.1 the items are lettered a) 1) i) and ii), under a new item a) for "Payload container type IE is set to "N1 SM information"".
    - From V15.3.0: a shall on any SMF selection failure.
  - Rel-16:
    - V16.3.0 adds "or case f)" to "as specified in subclause 5.4.5.3.1 case e)", so a congestion send-back is also conformant.
    - From V16.15.0 the request type is "initial request" or "MA PDU request".
  - Rel-18: "subclause" becomes "clause".
  - Otherwise unchanged through V19.8.0 and V20.1.0.
- **Required from V15.1.0:** the stimulus is a DNN the core does not support, so the shall applies from V15.1.0 under both readings. Only a core built on V15.0.0 could legally drop the request. Since cannot express a version, so the check's Expected says so.
- **Use in the check:**
  - The AMF returning the request is path (B), judged under 5.4.5.3.2.
  - A congestion send-back is SKIP.

## TS 24.501 5.4.5.3.2 (case e) of 5.4.5.3.1) (network-initiated NAS transport), shall

5.4.5.3.1 (General) lists cases a) to g). Case e) is:

> "a single uplink 5GSM message which was not forwarded due to routing failure"

5.4.5.3.2 then says:

> "In case e) in subclause 5.4.5.3.1 ... the AMF shall ... set the 5GMM cause IE to ..."

Payload container type is "N1 SM information", and the Payload container carries the 5GSM message that was not forwarded. The 5.4.5.2.5 cross-reference names 5.4.5.3.1 case e) in every version. The requirement itself is in 5.4.5.3.2, so the check cites it as "5.4.5.3.2 (case e) of 5.4.5.3.1)".

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`), with #90 only.
- **Changes:**
  - Rel-15:
    - V15.1.0 adds #91 as "DNN not supported with no slice or DNN not supported in specific slice".
    - V15.2.0 renames it "DNN not supported or not subscribed in the slice" and adds "Otherwise ... #90".
  - Rel-17: V17.3.0 adds "e) optionally include the Back-off timer value IE if the 5GMM cause IE is set to 5GMM cause #91 ... due to the DNN is not supported in the slice". It is still present in V20.1.0.
- **Case f) of 5.4.5.3.1:** congestion, with #22, #67 or #69 and a Back-off timer value IE. From V16.3.0 it is an allowed send-back form under 5.4.5.2.5 a) 1).
- **Use in the check:**
  - #90 or #91 with a payload container byte-equal to the request the UE sent is PASS (B). Which of the two causes was used is recorded, not asserted.
  - #90 or #91 with any other payload is FAIL, including an AMF-built PDU SESSION ESTABLISHMENT REJECT.
  - The request returned byte-equal with #22, #67 or #69 is SKIP, for every target release. The congestion path is conformant, and the unknown-DNN path was not exercised. The detail records the cause and whether a Back-off timer value IE was present.
  - The request returned with any other 5GMM cause is ERROR, not judged.

## TS 24.501 6.4.1.4.1 (6.4.1.4 in V15.1.0 to V15.7.0) (UE-requested PDU session establishment not accepted by the network), shall

> "If the connectivity with the requested DN is rejected by the network, the SMF shall create a PDU SESSION ESTABLISHMENT REJECT message."

> "The SMF shall set the 5GSM cause IE ... to indicate the reason"

> "The 5GSM cause IE typically indicates one of the following SM cause values:" (the list includes #27 "missing or unknown DNN" and #70 "missing or unknown DNN in a slice" in every version)

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`), which has 6.4.1.4.1 "General".
- **Changes:**
  - Rel-15:
    - V15.1.0 to V15.7.0 drop the sub-clause, so the text sits directly in 6.4.1.4.
    - V15.x reads "a SM PDU SESSION ESTABLISHMENT REJECT".
    - Typical causes: V15.1.0 adds #46 and #82; V15.2.0 adds #8, #32, #33, #38 and #95 to #111.
  - Rel-16:
    - It is 6.4.1.4.1 again from V16.0.0, which drops "SM".
    - Typical causes: V16.1.0 adds #39; V16.3.0 adds #57, #58 and #61.
  - Rel-17: V17.5.0 adds #86 to the typical causes.
  - The two shalls, and #27 in the list, are unchanged in every version through V20.1.0.
- **Use in the check:**
  - A PDU SESSION ESTABLISHMENT REJECT with any 5GSM cause is PASS (A), and the cause is recorded. "Typically" is descriptive, so #27 is not asserted, and the detail says so when the cause is something else.
  - A Reject that also carries a 5GMM cause is still PASS (A), with that cause recorded.

## TS 24.501 Annex B.1 #27 (Causes related to nature of request), descriptive

> "Cause #27 – Missing or unknown DNN: This 5GSM cause is used by the network to indicate that the requested service was rejected by the external DN because the DNN was not included although required or if the DNN could not be resolved."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`). Annex B is informative.
- **Changes:** none. The wording is the same in every version from V15.0.0 to V20.1.0.

## Verdicts

- **PASS (A):** a PDU SESSION ESTABLISHMENT REJECT, any 5GSM cause.
  - **Known limit:** an AMF-built Reject with no 5GMM cause looks the same on the wire as an SMF's Reject, so it also scores PASS (A). The detail says the two cannot be told apart.
- **PASS (B):** a DL NAS TRANSPORT with #90/#91 whose payload is byte-equal to the request the UE sent.
- **FAIL:**
  - #90/#91 with another payload. The likely cause depends on the payload. If it is a PDU SESSION ESTABLISHMENT REJECT: "AMF returns its own Reject in the payload container instead of the un-forwarded message". Otherwise: "AMF returns a payload other than the un-forwarded 5GSM message".
  - The request returned byte-equal with no 5GMM cause: 5.4.5.3.2 requires one. The likely cause is "AMF returned the request without the 5GMM cause 5.4.5.3.2 requires".
  - Nothing back within 10 s. Either the SMF shall reject or the AMF shall return the message, so this FAIL cites both.
  - Any other decoded answer, for example a 5GSM STATUS. Its 5GSM cause is recorded. #43 or a protocol-error cause (#95 to #101, #111) adds "check the stimulus".
- **SKIP:**
  - The core accepted the stimulus DNN, either by establishing the session or with an Accept in a plain DL NAS TRANSPORT. No shall forbids accepting it.
  - A congestion send-back (#22/#67/#69, case f)).
  - No UE configured.
- **ERROR:**
  - A registration or harness failure.
  - A downlink after the request that fails the integrity check ("downlink NAS after the request failed integrity check", with its message type when the header is readable).
  - A downlink after the request that does not decode, or a Reject or 5GSM STATUS payload that does not decode. Orbit's codec covers about Rel-15/16, so this may be Orbit's gap; the raw NAS is in the detail.
  - The request returned with a 5GMM cause other than #90/#91/#22/#67/#69.
- A core that then stops completing NG Setup is the crash FAIL.

The answer is decoded through the UE's NAS security context, which verifies the MAC and deciphers. Whenever registration completed, the UE is deregistered at the end, on every verdict, including both decode ERRORs: the UE is RM-REGISTERED by then, and switch-off deregistration uses the uplink context, which a downlink failure does not touch. The association is dropped without a deregistration only when the deregistration itself fails or times out, and the detail records it.

Stimulus: DNN `orbit-unknown-dnn`, on the run's slice.
