# NAS-T3550-ACCEPT-RETRANSMIT

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). Versions read (orbit-architect, 2026-10-12 and 2026-10-18; issue #138): V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`).

## TS 24.501 5.5.1.2.8 c) (Abnormal cases on the network side, "T3550 time-out"), shall

> "On the first expiry of the timer, the AMF shall retransmit the REGISTRATION ACCEPT message and shall reset and restart timer T3550. This retransmission is repeated four times, i.e. on the fifth expiry of timer T3550, the registration procedure for initial registration shall be aborted and the AMF enters state 5GMM-REGISTERED."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** Rel-15: V15.0.0 has the AMF enter "5GMM-DEREGISTERED" on the abort; from V15.7.0 it is "5GMM-REGISTERED". The retransmission wording is unchanged from V15.0.0 through V19.8.0. The same clause adds the old/new 5G-GUTI rule, which the check does not assert.
- **Scope of the check:** initial registration only. The mobility and periodic registration update case (5.5.1.3.8 c)) is not cited. The UE withholds its REGISTRATION COMPLETE after a REGISTRATION ACCEPT that assigns a 5G-GUTI. The check counts the REGISTRATION ACCEPTs after the first: the first comes in INITIAL CONTEXT SETUP REQUEST, and the retransmissions come in DOWNLINK NAS TRANSPORT.
- **Timing:** the timer is not assumed to be the default.
  - The first retransmission is awaited for 1.5 × 18 s, the largest Table 10.2.2 value for T3550 (WB-N1/CE, Rel-16).
  - After that, T_est = max(6 s, the mean gap so far), and each next retransmission is due 1.5 × T_est after the last.
  - After the fourth, the check waits another 1.5 × T_est for a fifth.
- **No release is required on the abort**, unlike T3560 (5.4.1.3.7 b)). So the check asserts only the retransmission count, and records any UE CONTEXT RELEASE COMMAND without judging it.
- **Verdicts:**
  - Four retransmissions: PASS.
  - None, fewer, or a fifth: FAIL, since 5.5.1.2.8 c) is a shall. A fifth means the AMF did not abort on the fifth expiry.
  - A REGISTRATION ACCEPT that assigns no 5G-GUTI: SKIP, because the 5G-GUTI trigger that starts T3550 (5.5.1.2.4) is absent; other T3550 triggers are not evaluated.
  - A reject, or no REGISTRATION ACCEPT within 10 s: ERROR.
  - A core that then stops completing NG Setup: the crash FAIL.

## TS 24.501 5.5.1.2.4 T3550 start (Initial registration accepted by the network), shall

V15.0.0 (`24501-f00.zip`):

> "When the REGISTRATION REQUEST message includes the SUCI or PEI, or the AMF considers the 5G-GUTI provided by the UE is invalid, or the 5G-GUTI provided by the UE was assigned by another AMF, the AMF shall allocate a new 5G-GUTI to the UE. The AMF shall include in the REGISTRATION ACCEPT message the new assigned 5G-GUTI together with the assigned TAI list. In this case the AMF shall start timer T3550 and enter state 5GMM-COMMON-PROCEDURE-INITIATED as described in subclause 5.1.3.2.3.3."

V15.7.0 (`24501-f70.zip`) onward (the typo is the spec's, fixed in Rel-17):

> "If a 5G-GUTI or the SOR transparent container IE is included in the REGISTRATION ACCCEPT message, the AMF shall start timer T3550 and enter state 5GMM-COMMON-PROCEDURE-INITIATED as described in subclause 5.1.3.2.3.3."

- **Since:** Rel-15, V15.0.0. "The AMF shall start timer T3550" holds in every release read.
- **Changes:**
  - Rel-15: V15.0.0 triggers on a newly allocated 5G-GUTI only. The check's UE registers with a SUCI, so a new 5G-GUTI is allocated. From V15.7.0 the trigger is a 5G-GUTI or the SOR transparent container being included, with further triggers: operator-defined access category definitions, extended emergency number list, a new configured NSSAI, and network slicing subscription changed.
  - Rel-16 (V16.15.0): the 5G-GUTI sentence is unchanged. It adds CAG information list, UE radio capability ID or its deletion indication, and truncated 5G-S-TMSI configuration.
  - Rel-17 (V17.18.1): the same sentence with the typo fixed. It adds extended CAG information list, negotiated PEIPS assistance information, UUAA-MM pending and NSSRG. The configured-NSSAI trigger applies only when roaming.
  - Rel-18 (V18.14.0): "subclause" becomes "clause". It adds on-demand NSSAI, S-NSSAI time and location validity, NSAG and alternative NSSAI. The configured-NSSAI wording is no longer in 5.5.1.2.4.
  - Rel-19 (V19.8.0): as Rel-18, plus negotiated LP-WUSPS assistance information and LP-WUS status.
- **Use in the check:** this is the precondition of the 5.5.1.2.8 c) retransmission rule, so a missing retransmission after a Registration Accept with a 5G-GUTI fails both. The check keys only on the 5G-GUTI: an Accept without one SKIPs (`Registration Accept carried no 5G-GUTI; T3550 not started`). Other T3550 triggers (SOR, CAG, NSSAI ...) are not evaluated.

## TS 24.501 Table 10.2.2 T3550 (Timers of 5GS mobility management, AMF side), descriptive

> T3550: 6s (the normal value; the same for T3560)

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** Rel-16 adds WB-N1/CE mode values (T3550 18s). Rel-17 onward adds "For access via a satellite NG-RAN cell, 11s". The normal value is 6s in every version read, V15.0.0 through V19.8.0.
- **Use in the check:** descriptive only. The table value is a default the operator may configure, so the spacing is recorded, not asserted. Detail gives each gap and the estimated timer, and notes a gap outside [0.5, 1.5] × the estimate ("gaps uneven") or × 6 s ("timer differs from the 6s default"). The 18s WB-N1/CE value, the largest in the table, bounds the wait for the first retransmission.
