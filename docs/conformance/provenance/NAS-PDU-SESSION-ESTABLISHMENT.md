# NAS-PDU-SESSION-ESTABLISHMENT

Source: the 3GPP archive only, [https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/](https://www.3gpp.org/ftp/Specs/archive/24_series/24.501/). Versions read (2026-11-11): V15.0.0 (`24501-f00.zip`), V15.7.0 (`24501-f70.zip`), V16.15.0 (`24501-gf0.zip`), V17.18.1 (`24501-hi1.zip`), V18.14.0 (`24501-ie0.zip`) and V19.8.0 (`24501-j80.zip`), the first version and the latest of each release. Versions between these were not read.

## TS 24.501 6.4.1.3 (UE-requested PDU session establishment procedure accepted by the network), shall, primary

> "If the connectivity with the requested DN is accepted by the network, the SMF shall create a PDU SESSION ESTABLISHMENT ACCEPT message."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`).
- **Changes:** none. The sentence opens the clause, word for word the same, in all six versions read.
- **Use in the check:** the answer to a request the network accepts. A PDU SESSION ESTABLISHMENT ACCEPT, with the session then set up on N2, is PASS.

## TS 24.501 6.4.1.4.1 (6.4.1.4 in V15.1.0 to V15.7.0) (UE-requested PDU session establishment procedure not accepted by the network), shall

> "If the connectivity with the requested DN is rejected by the network, the SMF shall create a PDU SESSION ESTABLISHMENT REJECT message."

- **Since:** Rel-15, V15.0.0 (`24501-f00.zip`), in 6.4.1.4.1 "General", reading "a SM PDU SESSION ESTABLISHMENT REJECT message".
- **Changes:**
  - Rel-15: in V15.7.0 the clause has no sub-clause, so the sentence sits directly in 6.4.1.4, and it reads "a SM PDU SESSION ESTABLISHMENT REJECT message" (NAS-PDU-UNKNOWN-DNN records that the sub-clause is dropped from V15.1.0).
  - Rel-16: by V16.15.0 the sentence is in 6.4.1.4.1 again and reads "a PDU SESSION ESTABLISHMENT REJECT message". It is unchanged through V19.8.0.
- **Use in the check:** with 6.4.1.3 it leaves the SMF two answers. A network that has not accepted the connectivity has rejected it, and shall then send the REJECT, so an answer that is neither (silence within 10 s, a release, another 5GSM message) is FAIL. The REJECT itself violates nothing: no shall obliges the network to accept a request.

## Verdict

- **Class:** conformance, two shalls (6.4.1.3, 6.4.1.4.1). The check owns the procedure "PDU session establishment": a check whose own PDU session the core does not establish reports BLOCKED and names this check.
- **Stimulus:** one UE registers on the check's own gNB and requests a PDU session with the run's DNN and slice (PDU session identity 1), as every check that needs a session does. The answer, and after a PDU SESSION ESTABLISHMENT ACCEPT the session's setup on N2, are awaited for 10 s from the request. The UE is deregistered at the end.
- **PASS** `established` (6.4.1.3): a PDU SESSION ESTABLISHMENT ACCEPT, and the session set up on N2 (the PDU SESSION RESOURCE SETUP REQUEST answered).
- **FAIL** `unanswered` (6.4.1.3, 6.4.1.4.1): neither an Accept nor a Reject within 10 s, a bound of the check (T3580, on which the UE would retransmit, is longer). A 5GMM STATUS in the window is recorded.
- **FAIL** `released` (6.4.1.3, 6.4.1.4.1): a UE CONTEXT RELEASE COMMAND after the request and neither answer.
- **FAIL** `other-answer` (6.4.1.4.1): a 5GSM message that is neither an Accept nor a Reject, for example a 5GSM STATUS, or the request returned without a 5GMM cause.
- **WARN (availability)** `rejected`: a PDU SESSION ESTABLISHMENT REJECT for the run's own DNN and slice. It is the answer 6.4.1.4.1 requires of a network that rejects the request, so no requirement is violated (rule 2 of ADR-0011). The core cannot complete the procedure for the configuration the run uses (rule 4, availability). The run's preflight establishes a session with the same configuration; when that failed, this check reports ERROR instead and the configuration is the user's to fix.
- **WARN (availability)** `not-forwarded`: the request returned by the AMF in a DL NAS TRANSPORT with a 5GMM cause (an SMF selection that failed, or congestion). TS 24.501 5.4.5.2.5, not cited here, has the AMF answer so; the core cannot complete the procedure for the run's configuration.
- **WARN (availability)** `no-user-plane`: an Accept and no setup of the session on N2 within 10 s of the request. No cited clause obliges the N2 setup; the session cannot carry user data.
- **WARN (interoperability)** `integrity-failed`: a downlink after the request whose MAC fails the integrity check. A conformant UE discards it, so the session cannot complete.
- **Not completed:** every outcome but `established` reports the PDU session establishment as not completed, so the BLOCKED results of dependent checks in the same run are counted under this one.
- **BLOCKED** (NG Setup, registration): the UE did not register, or did not reach the request within 10 s. The check needs a registered UE; its own registration failing is not its verdict.
- **CRASH:** the AMF no longer completes an NG Setup after the request.
- **ERROR:** the harness could not set up the UE; a downlink that does not decode (Orbit's NAS codec covers about Rel-15/16); an attach that failed after the request other than by a release; the per-test timeout expiring before the 10 s wait (raise --per-test-timeout); a local socket.
- **SKIP:** no UE configured, or a target release older than Rel-15.
- **Limits:** the user plane is not exercised: no G-PDU is sent or awaited, so a UPF that drops the session's packets is not seen here. Only one session, on the run's first slice, is requested.
