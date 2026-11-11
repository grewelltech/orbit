# NGAP-NGSETUP-UNKNOWN-PLMN

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/](https://www.3gpp.org/ftp/Specs/archive/38_series/38.413/). Versions read: V15.0.0 (`38413-f00.zip`), V15.1.0 (`38413-f10.zip`), V15.2.0 (`38413-f20.zip`), V15.13.0 (`38413-fd0.zip`), V16.4.0 (`38413-g40.zip`), V16.5.0 (`38413-g50.zip`), V16.9.0 (`38413-g90.zip`), V16.18.0 (`38413-gi0.zip`), V17.15.0 (`38413-hf0.zip`), V18.11.0 (`38413-ib0.zip`), V19.0.0 (`38413-j00.zip`), V19.4.0 (`38413-j40.zip`).

## TS 38.413 8.7.1.4 (NG Setup, Abnormal Conditions), shall, primary

> "If the AMF does not identify any of the PLMNs/SNPNs indicated in the NG SETUP REQUEST message, it shall reject the NG Setup procedure with an appropriate cause value."

- **Since:** Rel-15, V15.2.0 (`38413-f20.zip`). The clause is "Void." in V15.0.0 and V15.1.0. The Rel-15 wording: "If the NG-RAN node initiates the procedure by sending an NG SETUP REQUEST message including the PLMN Identity IEs and none of the PLMNs provided by the NG-RAN node is identified by the AMF, then the AMF shall reject the NG Setup procedure with an appropriate cause value."
- **Changes:** Rel-16: reworded to the PLMNs/SNPNs form above in V16.9.0 (`38413-g90.zip`). Rel-17, Rel-18: unchanged. Rel-19: V19.0.0 adds "If the NG Setup procedure is executed between the NG-RAN node and the AIOTF, the specification text above concerning the AMF applies for the AIOTF."; the quoted sentence is unchanged.
- **Scope:** the shall binds only when **none** of the PLMNs/SNPNs is identified. A request listing one served and one unserved PLMN need not be rejected, so the stimulus lists only an unserved PLMN (999/99, or 998/98 when the run's own PLMN is 999/99), in both the Global RAN Node ID and the Supported TA List, with a TAC distinct from the served one. It never lists a served PLMN with an unknown TAC: an AMF that matches whole TAIs rejects that with the same cause, and a PASS on it would say nothing about the PLMN check.
- **Rejection form:** 8.7.1.3 (Unsuccessful Operation) says "If the AMF cannot accept the setup, it should respond with an NG SETUP FAILURE message and appropriate cause value." It is not the requirement and is not cited as a reference: a should reference would allow a DEVIATION this check must never give. It is why "reject the NG Setup procedure" means NG SETUP FAILURE, the procedure's unsuccessful-outcome message.

## TS 38.413 9.3.1.2 (Cause, Misc "Unknown PLMN or SNPN"), descriptive

> "Unknown PLMN or SNPN | The AMF does not identify any PLMN or SNPN provided by the NG-RAN node."

- **Since:** Rel-15, as "Unknown PLMN".
- **Changes:** "Unknown PLMN" through V16.4.0 (`38413-g40.zip`); renamed "Unknown PLMN or SNPN" in V16.5.0 (`38413-g50.zip`). The enumeration value is unchanged.
- **Use:** the cause an AMF is expected to give for 8.7.1.4. The check names it in the detail when present and does not assert it.

## Verdict

- **Class:** conformance, one shall (8.7.1.4).
- **Stimulus:** one association from the suite's N2 source address and one NG SETUP REQUEST whose Global RAN Node ID and Supported TA List name only the unserved PLMN, with a TAC distinct from the served one. The answer is awaited for 4 s.
- **PASS** `setup-failure`: NG SETUP FAILURE. Its cause is recorded, not asserted (8.7.1.4 asks for "an appropriate cause value").
- **FAIL** (8.7.1.4), each its own outcome: `accepted` (NG SETUP RESPONSE), `other-pdu` (an ERROR INDICATION or another PDU, named in the detail), `silent-drop` (neither within 4 s, a bound of the check: TS 38.413 sets no timer for the NG Setup outcome; the absence is inferred within it), `association-reset`.
- **CRASH:** after any outcome but a reset association, a fresh NG Setup with the served PLMN and another gNB ID is tried; a core that no longer completes it did not survive the stimulus.
- **ERROR:** an NG SETUP RESPONSE whose PLMN Support List or Served GUAMI List names the presented PLMN: the AMF serves it, so the check cannot run there, never a false FAIL. Also the association could not be opened, the stimulus could not be built or sent, the per-test timeout expired before the 4 s wait, or the liveness probe could not run (a probe the AMF rejects means the run's --mcc/--mnc is not one it serves).
- **BLOCKED:** none: the NG Setup is the stimulus, so the check needs no procedure first.
- **SKIP:** a target release older than Rel-15.
- **Limits:** the cause value is not asserted. A core that serves every PLMN it is shown cannot be tested.
