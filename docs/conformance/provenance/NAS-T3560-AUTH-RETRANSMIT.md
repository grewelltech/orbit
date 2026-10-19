# NAS-T3560-AUTH-RETRANSMIT

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). Versions read (orbit-architect, 2026-10-12 and 2026-10-18; issue #135): V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`).

## TS 24.501 5.4.1.3.7 b) (Abnormal cases on the network side, "Expiry of timer T3560"), shall

> "The network shall, on the first expiry of the timer T3560, retransmit the AUTHENTICATION REQUEST message and shall reset and start timer T3560. This retransmission is repeated four times, i.e. on the fifth expiry of timer T3560, the network shall abort the 5G AKA based primary authentication and key agreement procedure and any ongoing 5GMM specific procedure and release the N1 NAS signalling connection."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`). The clause is 5.4.1.3.7 item b) in V15.0.0, V15.7.0, V16.15.0 and V19.8.0 alike; the text in V15.0.0 and V19.8.0 starts identically.
- **Changes:** none. The wording is present and unchanged in every version read, V15.0.0 through V19.8.0. Earlier drafts cited item c), which is wrong in every release read.
- **Scope of the check:** the UE withholds its AUTHENTICATION RESPONSE. The check counts the AUTHENTICATION REQUESTs after the first (want four) and looks for the release of the N1 NAS signalling connection, which on N2 is a UE CONTEXT RELEASE COMMAND for the UE, no later than 1.5 T3560 after the fourth retransmission. The same pattern for EAP-AKA' (5.4.1.2.4.4 a)) and for the Security Mode Command (5.4.2.7 b), which says only that the procedure shall be aborted) is not cited.
- **Verdicts:** four retransmissions, then the release: PASS. No retransmission, another count, or no release in time: FAIL, since 5.4.1.3.7 b) is a shall; Observed names the first part that missed, and Detail gives both. A reject before the window, or no AUTHENTICATION REQUEST within 10 s: ERROR. A core that then stops completing NG Setup: the crash FAIL.

## TS 24.501 Table 10.2.2 T3560 (Timers of 5GS mobility management, AMF side), descriptive

> T3560: 6s (the normal value; the same for T3550)

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** Rel-16 adds WB-N1/CE mode values (T3560 24s). Rel-17 onward adds "For access via a satellite NG-RAN cell, 11s". The normal value is 6s in every version read, V15.0.0 through V19.8.0.
- **Use in the check:** descriptive only. The table value is a default the operator may configure, so the spacing of the retransmissions is recorded, not asserted: Detail gives each gap and notes a gap outside [0.5, 1.5] × 6 s as "timer differs from the 6s default".
