# NAS-DEREG-ACCEPT-NOT-SWITCH-OFF

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). Versions read (step 0 of issue #145, 2026-11-10): V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`), V19.8.0 (`24501-j80.zip`). The Rel-15 versions between V15.0.0 and V15.7.0 were not read, so the version in which the clause title changes is not pinned.

## TS 24.501 5.5.2.2.2 (UE-initiated de-registration procedure completion), shall, primary

> "When the DEREGISTRATION REQUEST message is received by the AMF, the AMF shall send a DEREGISTRATION ACCEPT message to the UE, if the De-registration type IE does not indicate "switch off". Otherwise, the procedure is completed when the AMF receives the DEREGISTRATION REQUEST message."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** the two quoted sentences are word-for-word the same in every version read, V15.0.0 through V19.8.0. Rel-15: V15.0.0 titles the clause "UE-initiated de-registration procedure completion - common procedure"; V15.7.0 onward "UE-initiated de-registration procedure completion". The clause's next sentence, on the UE, gains "stop timer T3519 if running, and delete any stored SUCI" by V15.7.0; the check does not assert it.
- **Stimulus:** the UE registers without a PDU session, then sends a DEREGISTRATION REQUEST (UE originating de-registration) under its NAS security context, with its 5G-GUTI, access type 3GPP access, and the De-registration type's switch-off bit 0 ("normal de-registration") and re-registration bit 0.
- **Not asserted:** the UE CONTEXT RELEASE COMMAND after the Accept. No sentence of 5.5.2.2.2 requires it; TS 23.502 4.2.2.3.2 step 8 describes it ("the AMF sends N2 UE Release command"). It is awaited for 5 s, answered with UE CONTEXT RELEASE COMPLETE and recorded in the detail.
- **Cleanup:** a core that answers with neither an Accept nor a release may still hold the UE as registered, so a switch-off DEREGISTRATION REQUEST follows before the association closes; the detail says so.

## TS 24.501 Table 10.2.1 T3521 (Timers of 5GS mobility management, UE side), descriptive

> T3521: 15s. Cause of start: "Transmission of DEREGISTRATION REQUEST message when de-registration procedure is not due to a "switch off"". Normal stop: "DEREGISTRATION ACCEPT message received". On expiry: "Retransmission of DEREGISTRATION REQUEST message".

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** Rel-16 adds "In WB-N1/CE mode, 45s". Rel-17 onward adds "For access via a satellite NG-RAN cell, 27s". The normal value is 15s in every version read.
- **Use in the check:** descriptive only. It is the UE's timer, not a bound on the AMF: 5.5.2.2.2 sets no time limit. The check waits one T3521 at its normal value, the time after which a UE would retransmit the request, and sends no retransmission.

## Verdict

- **Class:** conformance, one shall (5.5.2.2.2). The check owns the procedure "deregistration": a check that needs a UE deregistered and whose deregistration the core does not complete reports BLOCKED and names this check.
- **Stimulus:** as above; the DEREGISTRATION ACCEPT is awaited for 15 s, one T3521 at its normal value.
- **PASS** `accepted`: a DEREGISTRATION ACCEPT, read through the UE's NAS security context. The detail gives the delay and records the release that follows, without judging it.
- **FAIL** (5.5.2.2.2), each reporting the deregistration as not completed: `release-without-accept` (a UE CONTEXT RELEASE COMMAND in place of the Accept), `no-accept` (none within 15 s, the absence inferred within that bound; a switch-off DEREGISTRATION REQUEST follows, and the detail says how the core answered it). The detail carries `no Deregistration Accept for a non-switch-off deregistration. TS 24.501 5.5.2.2.2. Likely cause: AMF treats every deregistration as switch-off.`
- **Not completed:** both FAIL outcomes report the deregistration as not completed, so the BLOCKED results of dependent checks in the same run are counted under this one.
- **CRASH:** a fresh NG Setup after the deregistration that no longer completes.
- **BLOCKED** `blocked-registration`: the core did not complete the UE's registration (`NAS-INITIAL-REGISTRATION` owns whether it completes); `blocked-ng-setup`: nor the NG Setup before it.
- **ERROR:** the harness could not be set up; the request could not be sent (no 5G-GUTI assigned, which ORBIT needs to build it); a local fault during the registration; or the per-test timeout expired during the registration or the wait for the Accept.
- **SKIP:** no UE configured (`skip-no-ue`); a target release older than Rel-15.
- **Limits:** the release after the Accept is recorded, not asserted (see "Not asserted" above). The wait is the UE's timer, not a bound on the AMF.
