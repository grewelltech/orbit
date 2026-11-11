# NGAP-RAN-CONFIG-UPDATE-KEEPS-TA

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: every Rel-15 version, V15.0.0 to V15.13.0 (`38413-f30.zip` is V15.3.0, `38413-fd0.zip` V15.13.0), plus V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`) and V19.4.0 (`38413-j40.zip`), the latest per release.

## TS 38.413 8.7.2.2 (RAN Configuration Update, Successful Operation), shall, primary

> "If an information element is not included in the RAN CONFIGURATION UPDATE message, the AMF shall interpret that the corresponding configuration data is not changed and shall continue to operate the NG-C interface with the existing related configuration data."

- **Since:** Rel-15, V15.3.0 (`38413-f30.zip`). The sentence is absent in V15.0.0 to V15.2.0.
- **Changes:** unchanged from V15.3.0 to V18.11.0. In V19.4.0 the same words are a bullet under "If the RAN Configuration Update procedure is executed between the NG-RAN node and the AMF".
- The response itself is descriptive in the same clause: "The AMF responds with a RAN CONFIGURATION UPDATE ACKNOWLEDGE message". A RAN CONFIGURATION UPDATE FAILURE is legal under 8.7.2.3 ("If the AMF cannot accept the update"). The state shall sits in the successful-operation clause, so the check judges state only after an ACKNOWLEDGE.

## TS 38.413 8.7.2.3 (RAN Configuration Update, Unsuccessful Operation), shall

> "If the AMF cannot accept the update, it shall respond with a RAN CONFIGURATION UPDATE FAILURE message and appropriate cause value."

- **Since:** Rel-15, V15.0.0 (`38413-f00.zip`).
- **Changes:** the sentence is the same in V15.0.0, V15.13.0, V16.18.0, V17.15.0, V18.11.0 and V19.4.0 (read in the extracted texts of all six, 2026-11-11).
- **Use in the check:** it permits the alternative to the ACKNOWLEDGE. A RAN CONFIGURATION UPDATE FAILURE is the answer this sentence asks of an AMF that cannot accept the update, and the state rule of 8.7.2.2 is for an acknowledged update, so after a FAILURE the check does not judge the state: a SKIP for a permitted alternative, resting on this sentence. No outcome is graded against it: whether an AMF that sent nothing could accept the update cannot be observed.

## 5GMM causes tied to a tracking area (cited here only, not in the check's references)

TS 24.501 Annex A, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). The versions these were read in are not listed, so they are not among the check's references.

- **#12 "Tracking area not allowed":** "sent to the UE if it requests service ... in a tracking area where the HPLMN or SNPN determines that the UE, by subscription, is not allowed to operate" (every version).
- **#13 "Roaming not allowed in this tracking area":** "in a tracking area of a PLMN or SNPN which by subscription offers roaming to that UE but not in that tracking area" (every version).
- **#15 "No suitable cells in tracking area":** "in a tracking area where the UE, by subscription, is not allowed to operate, but when it should find another allowed tracking area in the same PLMN ...".
- **#11 "PLMN not allowed"** applies at PLMN and subscription level ("in a PLMN where the UE, by subscription or due to operator determined barring, is not allowed to operate"). It is not tied to a tracking area, so a Reject with it does not show the tracking area was dropped; nor do #27 and #62.

## Verdict

- **Class:** conformance, one shall (8.7.2.2), with a SKIP for the alternative 8.7.2.3 permits.
- **Stimulus:** the UE's own association completes NG Setup with its Supported TA List; the gNB then sends a RAN CONFIGURATION UPDATE carrying only the RAN Node Name (the name NG Setup gave, so the update changes nothing); after the outcome the UE registers over the same association, in the tracking area NG Setup presented, with no PDU session. The update's outcome is awaited for 5 s, the registration for 10 s.
- **After a RAN CONFIGURATION UPDATE ACKNOWLEDGE:**
  - **PASS** `registered`: the registration succeeds. The UE is deregistered.
  - **FAIL** `tracking-area-reject` (8.7.2.2): a Registration Reject with 5GMM cause #12, #13 or #15; Observed names the cause and the tracking area. The detail ends with: "AMF stopped serving an NG Setup tracking area after a RAN CONFIGURATION UPDATE that did not include the Supported TA List. TS 38.413 8.7.2.2. Likely cause: the AMF treats an absent Supported TA List as an empty one."
  - **BLOCKED** `blocked-registration`: a Reject with any other cause (in Observed), a Reject that cannot be read, an Authentication Reject, a UE context release, no answer to the Registration Request within 10 s, or another failure of the registration. The core did not complete the registration the check judges the state by, and none of these shows the tracking area was dropped. `NAS-INITIAL-REGISTRATION` owns whether a registration completes.
- **SKIP** `update-failure` (8.7.2.3): a RAN CONFIGURATION UPDATE FAILURE. The UE still registers; Observed gives the FAILURE's cause and the registration result.
- **WARN (availability)** `update-unanswered`: neither ACKNOWLEDGE nor FAILURE within 5 s, a bound of the check (TS 38.413 sets no timer for the RAN Configuration Update outcome); the absence is inferred within it, and no registration is attempted. `update-association-lost`: the association ended before an outcome. No requirement covers either (8.7.2.3 binds only an AMF that cannot accept the update); the core does not complete the procedure for a valid update that changes nothing.
- **CRASH:** a fresh NG Setup with another gNB ID confirms the core after the stimulus; a core that no longer completes it is CRASH, `AMF unresponsive after the message (...)`.
- **BLOCKED** `blocked-ng-setup`: the UE's association did not complete NG Setup, so the update was never sent (no liveness probe follows).
- **ERROR:** the harness could not be set up (a missing gNB N3 address, for example), a local fault during the registration, or the per-test timeout expiring during the update's or the registration's wait.
- **SKIP:** no UE configured (`skip-no-ue`); a target release older than Rel-15.
- **Limit:** the check runs no control registration before the update. In a suite run, the preflight has registered the same subscriber with the same identity, so a subscriber the core rejects with #12, #13 or #15 anyway is unlikely to reach this check unnoticed; run alone, such a subscriber also FAILs.
