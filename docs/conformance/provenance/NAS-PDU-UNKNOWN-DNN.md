# NAS-PDU-UNKNOWN-DNN

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). Versions read (orbit-architect, 2026-10-12 and 2026-10-18; issue #137): V15.0.0 (`24501-f00.zip`), V15.1.0 (`24501-f10.zip`), V15.2.0 (`24501-f20.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`).

An unknown DNN can be caught in two places. If no SMF is selected, the **AMF** catches it and returns the request (5.4.5.2.5, 5.4.5.3.2). If an SMF is reached, the **SMF** rejects it (6.4.1.4). Either is conformant.

## TS 24.501 5.4.5.2.5 a) 1) (UL NAS transport, abnormal cases on the network side), shall

> "... and the SMF selection fails, then the AMF shall send back to the UE the 5GSM message which was not forwarded as specified in clause 5.4.5.3.1 case e) or case f)"

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** V15.0.0 letters the item a) rather than a) 1). The wording is unchanged from V15.0.0 through V19.8.0.
- **Use in the check:** the AMF returning the request is path (B), judged under 5.4.5.3.2 case e).

## TS 24.501 5.4.5.3.2 case e) (network-initiated NAS transport, routing failure), shall

> "set the 5GMM cause IE to the 5GMM cause #90 "payload was not forwarded" or 5GMM cause #91 "DNN not supported or not subscribed in the slice""

The DL NAS TRANSPORT carries Payload container type "N1 SM information" and a Payload container holding the 5GSM message that was not forwarded. The AMF sets #91 when SMF selection fails because the DNN is not supported in the slice or is not subscribed, and #90 otherwise.

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:**
  - Rel-15: #91 first appears in V15.2.0 (`24501-f20.zip`).
  - Rel-17: adds an optional back-off timer value.
  - From V15.7.0 to V19.8.0 the case e) wording is unchanged.
- **Not cited:** case f) (congestion: #22, #67 and #69). Neither are the Rel-15 UE-side cases e) and f) under 5.4.5.3.3.
- **Use in the check:**
  - A DL NAS TRANSPORT with #90 or #91 whose payload container is byte-equal to the request the UE sent is PASS (B). Which of the two causes was used is recorded, not asserted.
  - #90 or #91 with any other payload is FAIL, including an AMF-built PDU SESSION ESTABLISHMENT REJECT.
  - The request returned byte-equal with another 5GMM cause is ERROR, not judged. For #22, #67 or #69 the detail names case f) (congestion, transient: re-run); for any other cause it says the cause is not one 5.4.5.3.2 assigns to a routing failure.

## TS 24.501 6.4.1.4.1 (UE-requested PDU session establishment not accepted by the network), shall

> "If the connectivity with the requested DN is rejected by the network, the SMF shall create a PDU SESSION ESTABLISHMENT REJECT message."

> "The SMF shall set the 5GSM cause IE ... to indicate the reason"

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`), as 6.4.1.4.
- **Changes:** Rel-16 renumbers the clause 6.4.1.4.1.
- **Use in the check:** a PDU SESSION ESTABLISHMENT REJECT with any 5GSM cause is PASS (A), and the cause is recorded. A Reject that also carries a 5GMM cause other than #90/#91 is still PASS (A), with that cause recorded.

## TS 24.501 Annex B.1 #27 (with the 6.4.1.4.1 cause list), descriptive

> "The 5GSM cause IE typically indicates one of the following SM cause values: ... #27 missing or unknown DNN"

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`). Annex B.1 is informative.
- **Use in the check:** "typically" is descriptive, so #27 is not asserted. When the cause is not #27 the detail says so.

## Verdicts

- **PASS (A):** a PDU SESSION ESTABLISHMENT REJECT, any 5GSM cause.
  - **Known limit:** an AMF-built Reject with no 5GMM cause looks the same on the wire as an SMF's Reject, so it also scores PASS (A). The detail says the two cannot be told apart.
- **PASS (B):** a DL NAS TRANSPORT with #90/#91 whose payload is byte-equal to the request the UE sent.
- **FAIL:**
  - #90/#91 with another payload. The likely cause depends on the payload: if it is a PDU SESSION ESTABLISHMENT REJECT, "AMF returns its own Reject in the payload container instead of the un-forwarded message"; otherwise, "AMF returns a payload other than the un-forwarded 5GSM message".
  - Nothing back within 10 s. Either the SMF shall reject or the AMF shall return the message, so this FAIL cites both.
  - Any other decoded answer, for example a 5GSM STATUS. Its 5GSM cause is recorded. #43 or a protocol-error cause (#95 to #101, #111) adds "check the stimulus".
- **SKIP:**
  - The core accepted the stimulus DNN, by establishing the session or with an Accept in a plain DL NAS TRANSPORT. Observed is "core accepted the stimulus DNN; no rejection to check". No shall forbids accepting it.
  - No UE configured.
- **ERROR:**
  - Registration or harness failure.
  - An answer that fails the integrity check ("DL NAS Transport failed integrity check").
  - An answer that does not decode. Orbit's codec covers about Rel-15/16, so this may be Orbit's gap. The raw NAS is in the detail.
  - The request returned with another 5GMM cause (see 5.4.5.3.2).
- A core that then stops completing NG Setup is the crash FAIL.

The answer is decoded through the UE's NAS security context, which verifies the MAC and deciphers. Whenever registration completed, the UE is deregistered at the end. Only when the NAS state is unusable is the association dropped without a deregistration, and the detail records it.

Stimulus: DNN `orbit-unknown-dnn`, on the run's slice.
