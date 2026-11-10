# NGAP-RAN-CONFIG-UPDATE-KEEPS-TA

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: every Rel-15 version, V15.0.0 to V15.13.0 (`38413-f30.zip` is V15.3.0, `38413-fd0.zip` V15.13.0), plus V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`) and V19.4.0 (`38413-j40.zip`), the latest per release.

## TS 38.413 8.7.2.2 (RAN Configuration Update, Successful Operation), shall

> "If an information element is not included in the RAN CONFIGURATION UPDATE message, the AMF shall interpret that the corresponding configuration data is not changed and shall continue to operate the NG-C interface with the existing related configuration data."

- **Since:** Rel-15, V15.3.0 (`38413-f30.zip`). The sentence is absent in V15.0.0 to V15.2.0.
- **Changes:** unchanged from V15.3.0 to V18.11.0. In V19.4.0 the same words are a bullet under "If the RAN Configuration Update procedure is executed between the NG-RAN node and the AMF".
- The response itself is descriptive in the same clause: "The AMF responds with a RAN CONFIGURATION UPDATE ACKNOWLEDGE message". A RAN CONFIGURATION UPDATE FAILURE is legal under 8.7.2.3 ("If the AMF cannot accept the update"). The state shall sits in the successful-operation clause, so the check judges state only after an ACKNOWLEDGE.

## 5GMM causes tied to a tracking area (cited here only, not in the check's references)

TS 24.501 Annex A, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). The versions these were read in are not listed, so they are not among the check's references.

- **#12 "Tracking area not allowed":** "sent to the UE if it requests service ... in a tracking area where the HPLMN or SNPN determines that the UE, by subscription, is not allowed to operate" (every version).
- **#13 "Roaming not allowed in this tracking area":** "in a tracking area of a PLMN or SNPN which by subscription offers roaming to that UE but not in that tracking area" (every version).
- **#15 "No suitable cells in tracking area":** "in a tracking area where the UE, by subscription, is not allowed to operate, but when it should find another allowed tracking area in the same PLMN ...".
- **#11 "PLMN not allowed"** applies at PLMN and subscription level ("in a PLMN where the UE, by subscription or due to operator determined barring, is not allowed to operate"). It is not tied to a tracking area, so it is ERROR, not FAIL; so are #27 and #62.

## Verdict

- **Verdict class:** FAIL, derived from the shall.
- **Stimulus:** the UE's own association completes NG Setup with its Supported TA List; the gNB then sends a RAN CONFIGURATION UPDATE carrying only the RAN Node Name (the name NG Setup gave, so the update changes nothing); after the outcome the UE registers over the same association, in the tracking area NG Setup presented, with no PDU session.
- **After a RAN CONFIGURATION UPDATE ACKNOWLEDGE:**
  - **PASS:** the registration succeeds. The UE is deregistered.
  - **FAIL:** a Registration Reject with 5GMM cause #12, #13 or #15; Observed names the cause and the tracking area. The detail ends with: "AMF stopped serving an NG Setup tracking area after a RAN CONFIGURATION UPDATE that did not include the Supported TA List. TS 38.413 8.7.2.2. Likely cause: the AMF treats an absent Supported TA List as an empty one."
  - **ERROR:** a Reject with any other cause (in Observed), a Reject that cannot be read, an Authentication Reject, a UE context release, or no answer to the Registration Request within 10 s.
- **After a RAN CONFIGURATION UPDATE FAILURE:** SKIP. The UE still registers; Observed gives the FAILURE's cause and the registration result.
- **No outcome within 5 s:** ERROR, `no response to the RAN CONFIGURATION UPDATE`, and no registration is attempted. Whether a class 1 NGAP procedure may go unanswered is not settled, so the missing response is not scored.
- **SKIP:** category not selected; no UE configured.
- **Crash safety:** a fresh NG Setup confirms the core afterwards; a core that no longer completes it is FAIL, `AMF unresponsive after the message (...)`.
- **Limit:** the check runs no control registration before the update, so a subscriber the core would reject with #12, #13 or #15 in this tracking area anyway also FAILs. The suite's other UE checks register the same subscriber in the same tracking area without an update.
